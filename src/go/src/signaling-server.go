package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"signaling-server/internal/activation"
	"signaling-server/internal/admission"
	"signaling-server/internal/center"
	"signaling-server/internal/sid"
)

const (
	InactivityTimeout     = 90 * time.Second
	KeepaliveInterval     = 30 * time.Second
	KeepalivePingTimeout  = 10 * time.Second
	WriteWait             = 10 * time.Second
	MaxCameraPasswordSize = 256
)

type DeviceInfo struct {
	ID          string  `json:"id"`
	Nickname    string  `json:"nickname,omitempty"`
	Tenant      string  `json:"tenant,omitempty"`
	ConnectedAt string  `json:"connected_at"`
	Path        string  `json:"path"`
	Type        string  `json:"type"`
	RTT         float64 `json:"rtt"`
}

type Client struct {
	// lastSeenNano is the last inbound traffic time (unix nanoseconds). It is
	// atomic so the read pump never takes the global server lock for it.
	lastSeenNano atomic.Int64

	ID            string
	Tenant        string // from the signed ticket; empty only when admission is off
	Role          string // admission.RoleCamera or admission.RolePlayer
	Digest        string // SID digest from the ticket; empty without admission
	CameraIDScope string // nonempty for viewer tickets limited to one camera
	registeredSID string // in-memory camera SID; only emitted by configured discovery
	Conn          *websocket.Conn
	Info          *DeviceInfo
	Send          chan []byte
	done          chan struct{}
	closeOnce     sync.Once
	pingMu        sync.Mutex
	pingTime      time.Time
}

func (c *Client) Close() {
	c.closeOnce.Do(func() { close(c.done) })
}

// clientKey namespaces a client ID by tenant, so two tenants may use the same
// ID and a message can only ever reach a client of the sender's own tenant.
func clientKey(tenant, id string) string { return tenant + "/" + id }

type Server struct {
	clients    map[string]*Client // keyed by clientKey(tenant, id)
	mu         sync.RWMutex
	upgrader   websocket.Upgrader
	playerPath string
	rootDir    string
	admission  *admission.Validator
	// demoTenant, when set together with admission, lets unauthenticated
	// "Player*" web-demo clients join that one tenant. Development only.
	demoTenant      string
	discoveryTenant string
	center          http.Handler
	trustProxyTLS   bool
}

func NewServer(playerPath string, rootDir string) *Server {
	return &Server{
		clients: make(map[string]*Client),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
		playerPath: playerPath,
		rootDir:    rootDir,
	}
}

// SetAdmission requires a Center-issued ticket from every client. The ticket
// supplies the client's tenant and role.
func (s *Server) SetAdmission(validator admission.Validator) {
	s.admission = &validator
}

// SetDemoTenant lets unauthenticated "Player*" clients join tenant. Only
// meaningful with SetAdmission; never enable it in production.
func (s *Server) SetDemoTenant(tenant string) { s.demoTenant = tenant }

// SetDiscoveryTenant exposes camera-only metadata for one tenant through the
// read-only device list. It does not affect admission or message routing.
func (s *Server) SetDiscoveryTenant(tenant string) { s.discoveryTenant = tenant }

// SetTrustProxyTLS requires requests to arrive over TLS or carry the exact
// HTTPS marker set by the explicitly trusted TLS-terminating proxy.
func (s *Server) SetTrustProxyTLS(enabled bool) { s.trustProxyTLS = enabled }

// SetCenterHandler mounts Center's HTTPS endpoints on this signaling listener.
// Center paths are reserved before WebSocket routing, including requests that
// carry upgrade headers.
func (s *Server) SetCenterHandler(handler http.Handler) { s.center = handler }

func (s *Server) loadTurnConfig() []map[string]interface{} {
	data, err := os.ReadFile(s.rootDir + "cfg/turn.conf")
	if err != nil {
		return nil
	}
	var servers []map[string]interface{}
	if err := json.Unmarshal(data, &servers); err != nil {
		return nil
	}
	return servers
}

func (s *Server) loadVersion() string {
	data, err := os.ReadFile(s.rootDir + "cfg/version")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(data))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.trustProxyTLS && r.TLS == nil {
		forwardedProto := r.Header.Values("X-Forwarded-Proto")
		if len(forwardedProto) != 1 || forwardedProto[0] != "https" {
			http.Error(w, "HTTPS required", http.StatusBadRequest)
			return
		}
	}
	if s.center != nil && isCenterPath(r.URL.Path) {
		s.center.ServeHTTP(w, r)
		return
	}
	switch {
	case r.URL.Path == "/":
		http.ServeFile(w, r, s.playerPath)
	case r.URL.Path == "/api/devices":
		s.handleDevices(w, r)
	case r.URL.Path == "/api/health":
		s.handleHealth(w, r)
	case strings.HasPrefix(r.URL.Path, "/downloads/"):
		s.handleDownloads(w, r)
	default:
		s.handleWebSocket(w, r)
	}
}

func isCenterPath(path string) bool {
	switch path {
	case "/verify", "/v1/challenge", "/v1/resolve", "/v1/view", "/v1/demo/resolve", "/v1/admin/status", "/v1/admin/release":
		return true
	default:
		return false
	}
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	clientID := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[0]
	if clientID == "" {
		http.Error(w, "Client ID required", http.StatusBadRequest)
		return
	}
	tenant, role, digest, cameraIDScope := "", admission.RoleCamera, "", ""
	var spend *admission.Claims
	if strings.HasPrefix(clientID, "Player") {
		role = admission.RolePlayer
	}
	if s.admission != nil {
		if !admission.ValidDeviceID(clientID) || r.URL.Path != "/"+clientID || r.URL.EscapedPath() != "/"+clientID {
			http.Error(w, "Admission required", http.StatusUnauthorized)
			return
		}
		query, queryErr := url.ParseQuery(r.URL.RawQuery)
		tickets, hasTicket := query["ticket"]
		anonymousDemo := s.demoTenant != "" && role == admission.RolePlayer && len(query) == 0
		switch {
		case anonymousDemo:
			tenant = s.demoTenant
		case queryErr != nil || !hasTicket || len(tickets) != 1 || len(query) != 1 || tickets[0] == "":
			http.Error(w, "Admission required", http.StatusUnauthorized)
			return
		default:
			claims, err := s.admission.Check(tickets[0], clientID)
			if err != nil {
				if errors.Is(err, admission.ErrUnavailable) {
					http.Error(w, "Admission temporarily unavailable", http.StatusServiceUnavailable)
				} else {
					http.Error(w, "Admission required", http.StatusUnauthorized)
				}
				return
			}
			tenant, role, digest, cameraIDScope = claims.Tenant, claims.Role, claims.Digest, claims.CameraID
			spend = &claims
		}
		// Remove the bearer ticket from the request object before upgrade and any
		// future request logging or instrumentation can observe it.
		r.URL.RawQuery = ""
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	// The ticket is spent only now, so a failed handshake leaves it usable.
	if spend != nil && !s.admission.Consume(*spend) {
		conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "ticket already used"),
			time.Now().Add(time.Second))
		conn.Close()
		return
	}

	log.Printf("New connection from %s path=%s", r.RemoteAddr, r.URL.Path)

	deviceType := "consumer"
	if role == admission.RoleCamera {
		deviceType = "producer"
	}

	c := &Client{
		ID:            clientID,
		Tenant:        tenant,
		Role:          role,
		Digest:        digest,
		CameraIDScope: cameraIDScope,
		Conn:          conn,
		Info: &DeviceInfo{
			ID:          clientID,
			Tenant:      tenant,
			ConnectedAt: time.Now().UTC().Format(time.RFC3339),
			Path:        r.URL.Path,
			Type:        deviceType,
		},
		Send: make(chan []byte, 256),
		done: make(chan struct{}),
	}
	c.touch()

	// With admission on, clientID is derived from the device's own key, so a
	// reconnect under the same tenant and ID is the same device and replaces
	// its stale connection. Different tenants never collide.
	key := clientKey(tenant, clientID)
	s.mu.Lock()
	if old, exists := s.clients[key]; exists {
		old.Close()
		old.Conn.Close()
	}
	s.clients[key] = c
	s.mu.Unlock()

	log.Printf("Client %s connected tenant=%q role=%s", clientID, tenant, role)

	go s.writePump(c)
	s.readPump(c)
}

func (s *Server) readPump(c *Client) {
	defer func() {
		s.mu.Lock()
		if key := clientKey(c.Tenant, c.ID); s.clients[key] == c {
			delete(s.clients, key)
		}
		s.mu.Unlock()
		c.Conn.Close()
		c.Close()
		log.Printf("Client %s disconnected", c.ID)
	}()

	pongWait := KeepaliveInterval + KeepalivePingTimeout
	c.Conn.SetReadLimit(512 * 1024)
	c.Conn.SetReadDeadline(time.Now().Add(pongWait))
	c.Conn.SetPongHandler(func(string) error {
		c.Conn.SetReadDeadline(time.Now().Add(pongWait))
		c.touch()
		c.pingMu.Lock()
		if !c.pingTime.IsZero() {
			rtt := time.Since(c.pingTime).Milliseconds()
			s.mu.Lock()
			c.Info.RTT = float64(rtt)
			s.mu.Unlock()
			c.pingTime = time.Time{}
		}
		c.pingMu.Unlock()
		return nil
	})

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			var closeErr *websocket.CloseError

			if errors.As(err, &closeErr) {
				switch closeErr.Code {
				case websocket.CloseNormalClosure, websocket.CloseGoingAway:
					// 1000 or 1001: Client sent a clean disconnect frame
					log.Printf("[%s] Connection closed normally (%d)", c.ID, closeErr.Code)

				case websocket.CloseAbnormalClosure:
					// 1006: Hard drop at TCP/TLS layer (socket reset, peer crash, network timeout)
					log.Printf("[%s] Connection dropped abnormally (1006 / unexpected EOF)", c.ID)

				default:
					// Other WebSocket RFC close codes (e.g., 1002 protocol error, 1003 invalid payload, 1008 policy violation)
					log.Printf("[%s] WebSocket close frame received with code %d: %v", c.ID, closeErr.Code, closeErr.Text)
				}
			} else if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				// Low-level network socket EOF or closed connection before a WS frame could be parsed
				log.Printf("[%s] Network connection reset/EOF", c.ID)
			} else {
				// Generic IO/read errors
				log.Printf("[%s] Read error: %v", c.ID, err)
			}

			break // Exit loop and trigger cleanup
		}

		c.touch()

		var msg map[string]interface{}
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}
		if msg["type"] == "register" {
			nickname, ok := msg["nickname"].(string)
			if c.Role != admission.RoleCamera || !ok || !validNickname(nickname) {
				s.sendInvalidRequest(c)
				continue
			}
			rawSID, hasSID := msg["sid"]
			registeredSID := ""
			if hasSID {
				sidText, validType := rawSID.(string)
				if !validType || !validRegisteredCameraSID(sidText) || center.Digest(sidText) != c.Digest {
					s.sendInvalidRequest(c)
					continue
				}
				registeredSID = sidText
			}
			s.mu.Lock()
			c.Info.Nickname = nickname
			if hasSID {
				c.registeredSID = registeredSID
			}
			s.mu.Unlock()
			continue
		}

		isViewerRequest := c.Role == admission.RolePlayer && msg["type"] == "request"
		if isViewerRequest {
			if s.admission != nil {
				rawPassword, present := msg["password"]
				password, ok := rawPassword.(string)
				if !present || !ok || len(password) < 1 || len(password) > MaxCameraPasswordSize {
					s.sendInvalidRequest(c)
					continue
				}
			}
		} else {
			// Passwords are accepted only on a viewer's initial camera request.
			delete(msg, "password")
		}

		if msg["type"] == "bye" {
			log.Printf("[%s] sent bye, closing cleanly", c.ID)
			c.Conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
				time.Now().Add(WriteWait))
			break
		}

		destID, _ := msg["id"].(string)
		if destID == "" {
			continue
		}

		// Look up the target inside the sender's own tenant only. A target in
		// another tenant is indistinguishable from an offline one.
		s.mu.RLock()
		dest, exists := s.clients[clientKey(c.Tenant, destID)]
		s.mu.RUnlock()
		if exists && s.admission != nil {
			switch c.Role {
			case admission.RolePlayer:
				if c.CameraIDScope != "" {
					exists = dest.ID == c.CameraIDScope && dest.Role == admission.RoleCamera
				} else {
					// Unscoped legacy player tickets are accepted only as an
					// explicit development compatibility path.
					exists = s.demoTenant != "" && c.Tenant == s.demoTenant && dest.Role == admission.RoleCamera
				}
			case admission.RoleCamera:
				if dest.Role == admission.RolePlayer && dest.CameraIDScope != "" {
					exists = dest.CameraIDScope == c.ID
				} else {
					exists = s.demoTenant != "" && c.Tenant == s.demoTenant && dest.Role == admission.RolePlayer
				}
			}
		}

		if exists {
			msg["id"] = c.ID
			if c.Role == admission.RolePlayer && msg["type"] == "request" {
				// This value is trusted only because the ticket was verified above;
				// never accept a caller-supplied SID digest.
				msg["sid_digest"] = c.Digest
				if servers := s.loadTurnConfig(); len(servers) > 0 {
					msg["turn"] = servers
				}
			}
			data, _ := json.Marshal(msg)
			select {
			case dest.Send <- data:
			default:
			}
		} else {
			data, _ := json.Marshal(map[string]string{"type": "error", "msg": "Device offline"})
			select {
			case c.Send <- data:
			default:
			}
		}
	}
}

func validNickname(nickname string) bool {
	if nickname == "" || len(nickname) > 128 || !utf8.ValidString(nickname) {
		return false
	}
	for _, r := range nickname {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validRegisteredCameraSID(value string) bool {
	if len(value) != sid.EncodedSize {
		return false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(raw) == sid.RawSize && base64.RawURLEncoding.EncodeToString(raw) == value
}

func (s *Server) sendInvalidRequest(c *Client) {
	data, _ := json.Marshal(map[string]string{"type": "error", "msg": "Invalid request"})
	select {
	case c.Send <- data:
	default:
	}
}

func (s *Server) writePump(c *Client) {
	ticker := time.NewTicker(KeepaliveInterval)
	defer func() {
		ticker.Stop()
		c.Conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.Send:
			c.Conn.SetWriteDeadline(time.Now().Add(WriteWait))
			if !ok {
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.Conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.Conn.SetWriteDeadline(time.Now().Add(WriteWait))
			c.pingMu.Lock()
			c.pingTime = time.Now()
			c.pingMu.Unlock()
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

func (s *Server) sweepInactiveClients() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		s.sweep(time.Now())
	}
}

// sweep disconnects clients that went quiet and, with admission on, clients
// whose SID, tenant or device has since been blocked or revoked (for example a
// camera whose SID was released to another device). Tickets are only checked
// at connect time, so this is what ends a connection that is already open.
func (s *Server) sweep(now time.Time) {
	s.mu.RLock()
	var stale, revoked []*Client
	for _, c := range s.clients {
		if now.Sub(c.lastSeenTime()) > InactivityTimeout {
			stale = append(stale, c)
		} else if s.admission != nil {
			revoked = append(revoked, c)
		}
	}
	s.mu.RUnlock()

	for _, c := range stale {
		log.Printf("[%s] Inactive for %v, disconnecting", c.ID, InactivityTimeout)
		c.Close()
	}
	if s.admission == nil {
		return
	}
	list := s.admission.Blocklist
	for _, c := range revoked {
		blocked, err := list.Blocked(c.Digest, c.Tenant)
		if err == nil && !blocked && c.Role == admission.RoleCamera {
			blocked, err = list.DeviceRevoked(c.ID, now)
		}
		if err != nil {
			log.Printf("sweep: blocklist unavailable: %v", err)
			return
		}
		if blocked {
			log.Printf("[%s] Blocked or revoked, disconnecting", c.ID)
			c.Close()
		}
	}
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	// Discovery is read-only metadata exposure for one configured tenant. The
	// legacy demo tenant remains the fallback for existing development setups.
	listTenant := s.discoveryTenant
	discoveryOnly := listTenant != ""
	if listTenant == "" {
		listTenant = s.demoTenant
	}
	if discoveryOnly && s.admission == nil {
		http.NotFound(w, r)
		return
	}
	if s.admission != nil && listTenant == "" {
		http.NotFound(w, r)
		return
	}
	typeFilter := r.URL.Query().Get("type")
	s.mu.RLock()
	defer s.mu.RUnlock()

	devices := make([]any, 0)
	for _, c := range s.clients {
		if typeFilter != "" && c.Info.Type != typeFilter {
			continue
		}
		if s.admission != nil && c.Tenant != listTenant {
			continue
		}
		if discoveryOnly && c.Role != admission.RoleCamera {
			continue
		}
		if discoveryOnly {
			devices = append(devices, struct {
				DeviceInfo
				SID string `json:"sid,omitempty"`
			}{DeviceInfo: *c.Info, SID: c.registeredSID})
		} else {
			devices = append(devices, *c.Info)
		}
	}

	w.Header().Set("Access-Control-Allow-Origin", "*")
	if discoveryOnly {
		w.Header().Set("Cache-Control", "no-store")
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"count":   len(devices),
		"devices": devices,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	n := len(s.clients)
	s.mu.RUnlock()

	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":           "ok",
		"clients":          n,
		"version":          s.loadVersion(),
		"ticket_admission": s.admission != nil,
	})
}

// downloadWriteTimeout bounds a whole file download. The listener-wide 10 s
// write deadline would cut the camera binary off on a slow link.
const downloadWriteTimeout = 10 * time.Minute

func (s *Server) handleDownloads(w http.ResponseWriter, r *http.Request) {
	filename := strings.TrimPrefix(r.URL.Path, "/downloads/")
	if filename == "" || strings.Contains(filename, "..") {
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		return
	}
	filepath := s.rootDir + "downloads/" + filename
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(downloadWriteTimeout))
	http.ServeFile(w, r, filepath)
}

type serverOptions struct {
	centerMode        bool
	https             bool
	trustProxyTLS     bool
	sidPublicPath     string
	adminTokenPath    string
	minIssuedText     string
	centerPrivatePath string
	centerKID         string
	mainAddress       string
	tenantsPath       string
	activationsPath   string
	ticketPublicPath  string
	ticketKID         string
	ticketAudience    string
	blocklistPath     string
	demoTenant        string
	discoveryTenant   string
	ticketLifetime    time.Duration
	viewOrigin        string
}

func configureServer(srv *Server, opts serverOptions) error {
	if opts.https && opts.trustProxyTLS {
		return errors.New("choose either -https or -trust-proxy-tls")
	}
	if opts.demoTenant != "" && !sid.ValidTenant(opts.demoTenant) {
		return errors.New("-demo-tenant must be 6-8 characters of [a-z0-9]")
	}
	if opts.discoveryTenant != "" && !sid.ValidTenant(opts.discoveryTenant) {
		return errors.New("-discovery-tenant must be 6-8 characters of [a-z0-9]")
	}
	srv.SetTrustProxyTLS(opts.trustProxyTLS)
	centerConfigProvided := opts.sidPublicPath != "" || opts.minIssuedText != "" || opts.centerPrivatePath != "" || opts.centerKID != "" || opts.mainAddress != "" || opts.tenantsPath != "" || opts.activationsPath != ""
	if centerConfigProvided && !opts.centerMode {
		return errors.New("Center configuration flags require -center")
	}
	if opts.centerMode && !opts.https && !opts.trustProxyTLS {
		return errors.New("-center requires -https or explicitly configured -trust-proxy-tls")
	}
	if opts.centerMode {
		if opts.sidPublicPath == "" || opts.minIssuedText == "" || opts.centerPrivatePath == "" || opts.centerKID == "" || opts.mainAddress == "" || opts.tenantsPath == "" || opts.activationsPath == "" || opts.blocklistPath == "" {
			return errors.New("-center requires -sid-public-key, -min-issued, -center-private-key, -kid (or -center-kid), -main-address, -blocklist-file, -tenants-file, and -activation-store")
		}
		minIssued, err := time.Parse(time.RFC3339, opts.minIssuedText)
		if err != nil {
			return errors.New("-min-issued must be RFC3339")
		}
		sidPublic, err := admission.ReadPublicKey(opts.sidPublicPath)
		if err != nil {
			return errors.New("SID verifier configuration is invalid")
		}
		centerPrivate, err := admission.ReadPrivateKey(opts.centerPrivatePath)
		if err != nil {
			return errors.New("Center signer configuration is invalid")
		}
		centerPublic := centerPrivate.Public().(ed25519.PublicKey)
		if string(sidPublic) == string(centerPublic) {
			return errors.New("Center signing key must be separate from the SID issuer key")
		}
		if err := admission.ValidateAudience(opts.mainAddress); err != nil {
			return errors.New("-main-address must be an exact wss://host[:port]/ URL")
		}
		if opts.ticketKID != "" && opts.ticketKID != opts.centerKID {
			return errors.New("-ticket-kid must match the Center signing key ID")
		}
		if opts.ticketAudience != "" && opts.ticketAudience != opts.mainAddress {
			return errors.New("-ticket-aud must match -main-address")
		}
		if opts.ticketPublicPath != "" {
			configuredPublic, err := admission.ReadPublicKey(opts.ticketPublicPath)
			if err != nil || string(configuredPublic) != string(centerPublic) {
				return errors.New("-ticket-public-key must match -center-private-key")
			}
		}
		blocklist := admission.Blocklist{Path: opts.blocklistPath}
		if err := blocklist.Validate(); err != nil {
			return fmt.Errorf("blocklist configuration is invalid: %w", err)
		}
		tenants := &center.TenantFile{Path: opts.tenantsPath}
		if err := tenants.Validate(); err != nil {
			return fmt.Errorf("tenant configuration is invalid: %w", err)
		}
		var adminToken []byte
		if opts.adminTokenPath != "" {
			adminToken, err = readSecretFile(opts.adminTokenPath)
			if err != nil {
				return fmt.Errorf("-admin-token-file: %w", err)
			}
		}
		store, err := activation.Open(opts.activationsPath)
		if err != nil {
			return fmt.Errorf("activation store is invalid: %w", err)
		}
		handler, err := center.NewHandler(center.Config{
			SIDPublicKey: sidPublic, MinIssued: minIssued,
			Signer:      admission.TicketSigner{PrivateKey: centerPrivate, KeyID: opts.centerKID, Audience: opts.mainAddress, Lifetime: opts.ticketLifetime},
			MainAddress: opts.mainAddress, Blocklist: blocklist, Tenants: tenants, Activations: store,
			ChallengeKey: deriveChallengeKey(centerPrivate), TrustProxyTLS: opts.trustProxyTLS,
			DemoTenant: opts.demoTenant, ViewOrigin: opts.viewOrigin, AdminToken: adminToken,
		})
		if err != nil {
			store.Close()
			return fmt.Errorf("Center configuration is invalid: %w", err)
		}
		srv.SetCenterHandler(handler)
		srv.SetAdmission(admission.Validator{
			Tickets:   admission.TicketVerifier{PublicKey: centerPublic, KeyID: opts.centerKID, Audience: opts.mainAddress},
			Blocklist: blocklist, Replay: &admission.ReplayCache{},
		})
	} else {
		admissionConfigured := opts.ticketPublicPath != "" || opts.ticketKID != "" || opts.ticketAudience != "" || opts.blocklistPath != ""
		if admissionConfigured {
			if !opts.https && !opts.trustProxyTLS {
				return errors.New("ticket admission requires -https or explicitly configured -trust-proxy-tls")
			}
			if opts.ticketPublicPath == "" || opts.ticketKID == "" || opts.ticketAudience == "" || opts.blocklistPath == "" {
				return errors.New("ticket admission requires -ticket-public-key, -ticket-kid, -ticket-aud, and -blocklist-file")
			}
			publicKey, err := admission.ReadPublicKey(opts.ticketPublicPath)
			if err != nil {
				return errors.New("ticket admission public-key configuration is invalid")
			}
			if err := admission.ValidateAudience(opts.ticketAudience); err != nil {
				return errors.New("ticket admission audience is invalid")
			}
			blocklist := admission.Blocklist{Path: opts.blocklistPath}
			if err := blocklist.Validate(); err != nil {
				return fmt.Errorf("blocklist configuration is invalid: %w", err)
			}
			srv.SetAdmission(admission.Validator{Tickets: admission.TicketVerifier{PublicKey: publicKey, KeyID: opts.ticketKID, Audience: opts.ticketAudience}, Blocklist: blocklist, Replay: &admission.ReplayCache{}})
		}
	}
	if opts.demoTenant != "" {
		if srv.admission == nil {
			return errors.New("-demo-tenant requires ticket admission")
		}
		log.Printf("WARNING: demo tenant %q accepts unauthenticated Player* clients; never use -demo-tenant in production", opts.demoTenant)
		srv.SetDemoTenant(opts.demoTenant)
	}
	if opts.discoveryTenant != "" {
		if srv.admission == nil {
			return errors.New("-discovery-tenant requires ticket admission")
		}
		srv.SetDiscoveryTenant(opts.discoveryTenant)
	}
	return nil
}

// readSecretFile reads a secret that only its owner may access.
func readSecretFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("must be a regular file readable only by its owner")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	secret := bytes.TrimSpace(data)
	if len(secret) < 32 {
		return nil, errors.New("must hold at least 32 bytes")
	}
	return secret, nil
}

func deriveChallengeKey(private ed25519.PrivateKey) []byte {
	mac := hmac.New(sha256.New, private.Seed())
	mac.Write([]byte("center-challenge-key-v1"))
	return mac.Sum(nil)
}

func configureHTTPServer(server *http.Server) {
	server.ReadHeaderTimeout = 5 * time.Second
	server.ReadTimeout = 10 * time.Second
	server.WriteTimeout = 10 * time.Second
	server.IdleTimeout = 60 * time.Second
	server.MaxHeaderBytes = 8 << 10
}

func main() {
	port := flag.Int("port", 8000, "Listen port")
	rootDir := flag.String("root", "", "Root directory for config and downloads (default: current dir)")
	https := flag.Bool("https", false, "Enable HTTPS (uses cert.pem from root directory)")
	trustProxyTLS := flag.Bool("trust-proxy-tls", false, "trust X-Forwarded-Proto: https only behind a TLS proxy that is the listener's sole network entry point")
	adminTokenPath := flag.String("admin-token-file", "", "file holding the pairing system's bearer token (32+ bytes, mode 0600); enables /v1/admin/status and /v1/admin/release with -center")
	centerMode := flag.Bool("center", false, "enable Center activation and viewer endpoints on this signaling server")
	sidPublicPath := flag.String("sid-public-key", "", "SID issuer PKIX Ed25519 public-key PEM (required by -center)")
	minIssuedText := flag.String("min-issued", "", "minimum accepted SID issue time, RFC3339 (required by -center)")
	centerPrivatePath := flag.String("center-private-key", "", "Center PKCS#8 Ed25519 signing key PEM (required by -center)")
	centerKID := flag.String("kid", "", "Center ticket signing key ID (required by -center; alias: -center-kid)")
	centerKIDAlias := flag.String("center-kid", "", "Center ticket signing key ID (alias for -kid)")
	mainAddress := flag.String("main-address", "", "exact MainServer WSS root URL and JWT audience (required by -center)")
	tenantsPath := flag.String("tenants-file", "", "registered tenant JSON file (required by -center)")
	activationsPath := flag.String("activation-store", "", "activation store path (required by -center)")
	ticketPublicPath := flag.String("ticket-public-key", "", "Center PKIX Ed25519 public key PEM; enables ticket admission for every client")
	ticketKID := flag.String("ticket-kid", "", "Center ticket signing key ID")
	ticketAudience := flag.String("ticket-aud", "", "exact configured WSS root URL audience")
	blocklistPath := flag.String("blocklist-file", "", "required local blocklist file (SID digests and tenant:NAME lines)")
	ticketLifetime := flag.Duration("ticket-lifetime", admission.DefaultTicketLifetime, "Center ticket lifetime (max 5m)")
	viewOrigin := flag.String("view-origin", "", "optional exact HTTPS browser origin allowed to call POST /v1/view")
	demoTenant := flag.String("demo-tenant", "", "DEV ONLY: with ticket admission, let unauthenticated Player* clients join this tenant")
	discoveryTenant := flag.String("discovery-tenant", "", "DEV ONLY: expose camera-only /api/devices metadata for one admitted tenant")
	flag.Parse()
	if *demoTenant != "" && !sid.ValidTenant(*demoTenant) {
		log.Fatal("-demo-tenant must be 6-8 characters of [a-z0-9]")
	}
	if *discoveryTenant != "" && !sid.ValidTenant(*discoveryTenant) {
		log.Fatal("-discovery-tenant must be 6-8 characters of [a-z0-9]")
	}

	root := *rootDir
	if root != "" && !strings.HasSuffix(root, "/") {
		root += "/"
	}

	playerPath := root + "player/ws.html"
	srv := NewServer(playerPath, root)
	if *demoTenant != "" && *ticketPublicPath == "" && !*centerMode {
		log.Fatal("-demo-tenant requires ticket admission")
	}
	centerSigningKID := *centerKID
	if *centerKIDAlias != "" {
		if centerSigningKID != "" && centerSigningKID != *centerKIDAlias {
			log.Fatal("-kid and -center-kid must match when both are set")
		}
		centerSigningKID = *centerKIDAlias
	}
	if err := configureServer(srv, serverOptions{
		centerMode: *centerMode, adminTokenPath: *adminTokenPath, https: *https, trustProxyTLS: *trustProxyTLS,
		sidPublicPath: *sidPublicPath, minIssuedText: *minIssuedText,
		centerPrivatePath: *centerPrivatePath, centerKID: centerSigningKID,
		mainAddress: *mainAddress, tenantsPath: *tenantsPath,
		activationsPath: *activationsPath, ticketPublicPath: *ticketPublicPath,
		ticketKID: *ticketKID, ticketAudience: *ticketAudience,
		blocklistPath: *blocklistPath, demoTenant: *demoTenant, discoveryTenant: *discoveryTenant,
		ticketLifetime: *ticketLifetime, viewOrigin: *viewOrigin,
	}); err != nil {
		log.Fatal(err)
	}
	go srv.sweepInactiveClients()

	addr := fmt.Sprintf(":%d", *port)
	httpServer := &http.Server{
		Addr:    addr,
		Handler: srv,
	}
	configureHTTPServer(httpServer)

	if *https {
		certPath := root + "cert.pem"
		cert, err := tls.LoadX509KeyPair(certPath, certPath)
		if err != nil {
			log.Fatalf("TLS: %v", err)
		}
		httpServer.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		log.Printf("Listening on wss://0.0.0.0%s", addr)
		log.Printf("  GET https://0.0.0.0%s/         — WebRTC Player", addr)
		log.Printf("  GET https://0.0.0.0%s/api/devices  — list online devices", addr)
		log.Printf("  GET https://0.0.0.0%s/api/health   — health check", addr)
		log.Fatal(httpServer.ListenAndServeTLS("", ""))
	} else {
		log.Printf("Listening on ws://0.0.0.0%s", addr)
		log.Printf("  GET http://0.0.0.0%s/         — WebRTC Player", addr)
		log.Printf("  GET http://0.0.0.0%s/api/devices  — list online devices", addr)
		log.Printf("  GET http://0.0.0.0%s/api/health   — health check", addr)
		log.Fatal(httpServer.ListenAndServe())
	}
}

func (c *Client) touch() { c.lastSeenNano.Store(time.Now().UnixNano()) }

func (c *Client) lastSeenTime() time.Time { return time.Unix(0, c.lastSeenNano.Load()) }
