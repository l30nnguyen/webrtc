package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"signaling-server/internal/admission"
	"signaling-server/internal/center"
	"signaling-server/internal/sid"
)

func TestCenterConfigurationFailsClosedWhenIncomplete(t *testing.T) {
	server := NewServer("", "")
	err := configureServer(server, serverOptions{centerMode: true, https: true})
	if err == nil || !strings.Contains(err.Error(), "-center requires") {
		t.Fatalf("configureServer error = %v, want incomplete Center configuration", err)
	}
	if server.center != nil || server.admission != nil {
		t.Fatal("incomplete Center configuration partially enabled routes or admission")
	}
}

func TestTrustProxyTLSGuardsAPIsAndWebSockets(t *testing.T) {
	server := NewServer("", "")
	if err := configureServer(server, serverOptions{trustProxyTLS: true}); err != nil {
		t.Fatalf("configure trusted proxy mode: %v", err)
	}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()

	client := httpServer.Client()
	response, err := client.Get(httpServer.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("API without forwarded HTTPS marker returned %d", response.StatusCode)
	}
	request, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Forwarded-Proto", "https")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("API with forwarded HTTPS marker returned %d", response.StatusCode)
	}

	websocketURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/camera-proxy-test"
	dialer := websocket.Dialer{}
	badHeaders := http.Header{"X-Forwarded-Proto": []string{"http"}}
	_, rejected, err := dialer.Dial(websocketURL, badHeaders)
	if err == nil || rejected == nil || rejected.StatusCode != http.StatusBadRequest {
		t.Fatalf("WebSocket without trusted HTTPS marker: response=%v err=%v", rejected, err)
	}
	goodHeaders := http.Header{"X-Forwarded-Proto": []string{"https"}}
	conn, accepted, err := dialer.Dial(websocketURL, goodHeaders)
	if err != nil {
		t.Fatalf("WebSocket with trusted HTTPS marker rejected: response=%v err=%v", accepted, err)
	}
	conn.Close()
}

const testAudience = "wss://main.example.test/"

var testNow = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

type node struct {
	t         *testing.T
	srv       *Server
	http      *httptest.Server
	key       ed25519.PrivateKey
	blocklist string
}

func newNode(t *testing.T, configure func(*Server)) *node {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blocklist := filepath.Join(t.TempDir(), "blocklist.txt")
	if err := os.WriteFile(blocklist, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir() + "/"
	if err := os.MkdirAll(root+"cfg", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"cfg/turn.conf", []byte(`[{"urls":"turn:turn.example.test:3478","username":"u","credential":"secret-credential"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := NewServer("", root)
	srv.SetAdmission(admission.Validator{
		Tickets:   admission.TicketVerifier{PublicKey: key.Public().(ed25519.PublicKey), KeyID: "center-v1", Audience: testAudience, Now: func() time.Time { return testNow }},
		Blocklist: admission.Blocklist{Path: blocklist},
		Replay:    &admission.ReplayCache{Now: func() time.Time { return testNow }},
	})
	if configure != nil {
		configure(srv)
	}
	httpServer := httptest.NewTLSServer(srv)
	t.Cleanup(httpServer.Close)
	return &node{t: t, srv: srv, http: httpServer, key: key, blocklist: blocklist}
}

func (n *node) ticket(id, tenant, role string, mutate ...func(*admission.Claims)) string {
	n.t.Helper()
	digest := sha256.Sum256([]byte("sid-" + tenant + id))
	jti := make([]byte, 16)
	_, _ = rand.Read(jti)
	claims := admission.Claims{Issuer: "center", Subject: id, Role: role, Tenant: tenant, Audience: testAudience, Digest: hex.EncodeToString(digest[:]), IssuedAt: testNow.Unix(), Expires: testNow.Unix() + 60, TokenID: base64.RawURLEncoding.EncodeToString(jti)}
	for _, m := range mutate {
		m(&claims)
	}
	return signTestTicket(n.t, n.key, "center-v1", claims)
}

func signTestTicket(t *testing.T, key ed25519.PrivateKey, kid string, claims admission.Claims) string {
	t.Helper()
	enc := base64.RawURLEncoding
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "kid": kid, "typ": "JWT"})
	claimBytes, _ := json.Marshal(claims)
	input := enc.EncodeToString(header) + "." + enc.EncodeToString(claimBytes)
	return fmt.Sprintf("%s.%s", input, enc.EncodeToString(ed25519.Sign(key, []byte(input))))
}

func (n *node) wsURL(path string) string {
	return "wss" + strings.TrimPrefix(n.http.URL, "https") + path
}

func (n *node) dial(path string) (*websocket.Conn, *http.Response) {
	n.t.Helper()
	dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	conn, response, err := dialer.Dial(n.wsURL(path), http.Header{})
	if response == nil {
		n.t.Fatalf("missing handshake response: %v", err)
	}
	response.Body.Close()
	return conn, response
}

// connect opens an authenticated WebSocket for id and fails the test unless it is accepted.
func (n *node) connect(id, tenant, role string) *websocket.Conn {
	n.t.Helper()
	conn, response := n.dial("/" + id + "?ticket=" + url.QueryEscape(n.ticket(id, tenant, role)))
	if response.StatusCode != http.StatusSwitchingProtocols {
		n.t.Fatalf("connect %s/%s status = %d", tenant, id, response.StatusCode)
	}
	n.t.Cleanup(func() { conn.Close() })
	n.waitRegistered(tenant, id)
	return conn
}

func (n *node) connectScopedViewer(id, tenant, cameraID string) *websocket.Conn {
	n.t.Helper()
	ticket := n.ticket(id, tenant, admission.RolePlayer, func(c *admission.Claims) { c.CameraID = cameraID })
	conn, response := n.dial("/" + id + "?ticket=" + url.QueryEscape(ticket))
	if response.StatusCode != http.StatusSwitchingProtocols {
		n.t.Fatalf("scoped viewer %s/%s status = %d", tenant, id, response.StatusCode)
	}
	n.t.Cleanup(func() { conn.Close() })
	n.waitRegistered(tenant, id)
	return conn
}

func (n *node) waitRegistered(tenant, id string) {
	n.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n.srv.mu.RLock()
		_, ok := n.srv.clients[clientKey(tenant, id)]
		n.srv.mu.RUnlock()
		if ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	n.t.Fatalf("client %s/%s never registered", tenant, id)
}

func send(t *testing.T, conn *websocket.Conn, message map[string]any) {
	t.Helper()
	if err := conn.WriteJSON(message); err != nil {
		t.Fatal(err)
	}
}

// receive returns the next message, or nil if none arrives within wait.
func receive(conn *websocket.Conn, wait time.Duration) map[string]any {
	_ = conn.SetReadDeadline(time.Now().Add(wait))
	var message map[string]any
	if err := conn.ReadJSON(&message); err != nil {
		return nil
	}
	return message
}

func TestWebSocketAdmission(t *testing.T) {
	n := newNode(t, nil)
	digest := hex.EncodeToString(make([]byte, 32))
	for _, test := range []struct {
		name        string
		id          string
		ticket      func() string
		blocklist   string
		querySuffix string
		wantStatus  int
	}{
		{name: "missing ticket", id: "camera0", wantStatus: http.StatusUnauthorized},
		{name: "malformed ticket", id: "camera0", ticket: func() string { return "not-a-jwt" }, wantStatus: http.StatusUnauthorized},
		{name: "expired ticket", id: "camera0", ticket: func() string {
			return n.ticket("camera0", "acme01", "camera", func(c *admission.Claims) { c.IssuedAt -= 120; c.Expires = testNow.Unix() })
		}, wantStatus: http.StatusUnauthorized},
		{name: "wrong identity", id: "camera1", ticket: func() string { return n.ticket("camera0", "acme01", "camera") }, wantStatus: http.StatusUnauthorized},
		{name: "wrong audience", id: "camera0", ticket: func() string {
			return n.ticket("camera0", "acme01", "camera", func(c *admission.Claims) { c.Audience = "wss://other.example.test/" })
		}, wantStatus: http.StatusUnauthorized},
		{name: "unknown role", id: "camera0", ticket: func() string {
			return n.ticket("camera0", "acme01", "admin")
		}, wantStatus: http.StatusUnauthorized},
		{name: "ticket without tenant", id: "camera0", ticket: func() string {
			return n.ticket("camera0", "", "camera")
		}, wantStatus: http.StatusUnauthorized},
		{name: "lifetime above five minutes", id: "camera0", ticket: func() string {
			return n.ticket("camera0", "acme01", "camera", func(c *admission.Claims) { c.Expires = testNow.Unix() + 301 })
		}, wantStatus: http.StatusUnauthorized},
		{name: "blocked SID", id: "camera0", ticket: func() string {
			return n.ticket("camera0", "acme01", "camera", func(c *admission.Claims) { c.Digest = digest })
		}, blocklist: digest + "\n", wantStatus: http.StatusUnauthorized},
		{name: "blocked tenant", id: "camera0", ticket: func() string { return n.ticket("camera0", "badco01", "camera") }, blocklist: "tenant:badco01\n", wantStatus: http.StatusUnauthorized},
		{name: "malformed extra query parameter", id: "camera0", ticket: func() string { return n.ticket("camera0", "acme01", "camera") }, querySuffix: "&extra=%ZZ", wantStatus: http.StatusUnauthorized},
		{name: "anonymous Player without demo mode", id: "PlayerAbc", wantStatus: http.StatusUnauthorized},
		{name: "accepted camera", id: "camera0", ticket: func() string { return n.ticket("camera0", "acme01", "camera") }, wantStatus: http.StatusSwitchingProtocols},
		{name: "accepted player with a hashed id", id: "Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9", ticket: func() string {
			return n.ticket("Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9", "acme01", "player")
		}, wantStatus: http.StatusSwitchingProtocols},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(n.blocklist, []byte(test.blocklist), 0o600); err != nil {
				t.Fatal(err)
			}
			future := time.Now().Add(time.Hour)
			_ = os.Chtimes(n.blocklist, future, future)
			path := "/" + test.id
			if test.ticket != nil {
				path += "?ticket=" + url.QueryEscape(test.ticket()) + test.querySuffix
			}
			conn, response := n.dial(path)
			if response.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.wantStatus)
			}
			if test.wantStatus == http.StatusSwitchingProtocols {
				_ = conn.Close()
			} else if conn != nil {
				_ = conn.Close()
				t.Fatal("invalid admission unexpectedly connected")
			}
		})
	}
}

func TestTicketIsSingleUse(t *testing.T) {
	n := newNode(t, nil)
	ticket := n.ticket("camera0", "acme01", "camera")
	path := "/camera0?ticket=" + url.QueryEscape(ticket)
	first, response := n.dial(path)
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("first use status = %d", response.StatusCode)
	}
	defer first.Close()
	if _, response := n.dial(path); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replayed ticket status = %d, want 401", response.StatusCode)
	}
}

func TestBlocklistFailureIs503(t *testing.T) {
	n := newNode(t, nil)
	ticket := n.ticket("camera0", "acme01", "camera")
	if err := os.Remove(n.blocklist); err != nil {
		t.Fatal(err)
	}
	if _, response := n.dial("/camera0?ticket=" + url.QueryEscape(ticket)); response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.StatusCode)
	}
}

func TestAdmissionPathMustBeSingleSafeSegment(t *testing.T) {
	n := newNode(t, nil)
	for _, path := range []string{"/camera0/extra", "/camera0%2Fextra", "/camera%30"} {
		t.Run(path, func(t *testing.T) {
			if _, response := n.dial(path); response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", response.StatusCode)
			}
		})
	}
}

// --- tenant isolation: the regression suite for the security boundary ---

func TestMessagesNeverCrossTenants(t *testing.T) {
	n := newNode(t, nil)
	cameraA := n.connect("camera0", "acme01", "camera")
	cameraB := n.connect("camera0", "other01", "camera") // same ID, other tenant
	playerA := n.connectScopedViewer("PlayerAAAAAA", "acme01", "camera0")
	playerC := n.connectScopedViewer("PlayerCCCCCC", "third01", "camera0")

	send(t, playerA, map[string]any{"type": "request", "id": "camera0", "password": "test-pass"})
	got := receive(cameraA, time.Second)
	if got == nil || got["type"] != "request" || got["id"] != "PlayerAAAAAA" {
		t.Fatalf("same-tenant camera did not receive the request: %v", got)
	}
	if leaked := receive(cameraB, 300*time.Millisecond); leaked != nil {
		t.Fatalf("a tenant's message reached another tenant's camera with the same ID: %v", leaked)
	}

	// A tenant with no such camera sees exactly the offline reply, never a hint
	// that the ID exists elsewhere.
	send(t, playerC, map[string]any{"type": "request", "id": "camera0", "password": "test-pass"})
	reply := receive(playerC, time.Second)
	if reply == nil || reply["type"] != "error" || reply["msg"] != "Device offline" {
		t.Fatalf("cross-tenant target did not look offline: %v", reply)
	}
	send(t, playerC, map[string]any{"type": "request", "id": "never-existed", "password": "test-pass"})
	missing := receive(playerC, time.Second)
	if fmt.Sprint(reply) != fmt.Sprint(missing) {
		t.Fatalf("existing-elsewhere and nonexistent targets differ: %v vs %v", reply, missing)
	}
	if extra := receive(cameraA, 200*time.Millisecond); extra != nil {
		t.Fatalf("camera received an unexpected message: %v", extra)
	}
}

func TestCameraScopedViewerTicketRoutesOnlyToItsCameraAndOverwritesDigest(t *testing.T) {
	n := newNode(t, nil)
	camera := n.connect("camera0", "acme01", "camera")
	otherCamera := n.connect("camera1", "acme01", "camera")
	viewerID := "q3N0k3NoKkPyHf4W9Jw9F2iQ7n_oa3q8T1LZ6KfV5zA"
	cameraDigestBytes := sha256.Sum256([]byte("camera SID digest"))
	cameraDigest := hex.EncodeToString(cameraDigestBytes[:])
	ticket := n.ticket(viewerID, "acme01", admission.RolePlayer, func(c *admission.Claims) {
		c.Digest = cameraDigest
		c.CameraID = "camera0"
	})
	viewer, response := n.dial("/" + viewerID + "?ticket=" + url.QueryEscape(ticket))
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("scoped viewer status=%d", response.StatusCode)
	}
	defer viewer.Close()
	n.waitRegistered("acme01", viewerID)
	n.srv.mu.RLock()
	registeredViewer := n.srv.clients[clientKey("acme01", viewerID)]
	gotScope := registeredViewer.CameraIDScope
	n.srv.mu.RUnlock()
	if gotScope != "camera0" {
		t.Fatalf("viewer scope = %q", gotScope)
	}

	send(t, viewer, map[string]any{"type": "request", "id": "camera1", "sid_digest": strings.Repeat("f", 64), "password": "camera-secret"})
	if reply := receive(viewer, time.Second); reply == nil || reply["type"] != "error" || reply["msg"] != "Device offline" {
		t.Fatalf("other camera was not hidden as offline: %v", reply)
	}
	if got := receive(otherCamera, 200*time.Millisecond); got != nil {
		t.Fatalf("request reached non-scoped camera: %v", got)
	}
	send(t, viewer, map[string]any{"type": "request", "id": "camera0"})
	if reply := receive(viewer, time.Second); reply == nil || reply["msg"] != "Invalid request" {
		t.Fatalf("request without a password was not rejected: %v", reply)
	}
	send(t, viewer, map[string]any{"type": "request", "id": "camera0", "password": ""})
	if reply := receive(viewer, time.Second); reply == nil || reply["msg"] != "Invalid request" {
		t.Fatalf("empty password was not rejected: %v", reply)
	}
	send(t, viewer, map[string]any{"type": "request", "id": "camera0", "password": strings.Repeat("x", MaxCameraPasswordSize+1)})
	if reply := receive(viewer, time.Second); reply == nil || reply["msg"] != "Invalid request" {
		t.Fatalf("oversized password was not rejected: %v", reply)
	}
	send(t, viewer, map[string]any{"type": "request", "id": "camera0", "sid_digest": strings.Repeat("f", 64), "password": "camera-secret"})
	request := receive(camera, time.Second)
	if request == nil || request["id"] != viewerID || request["sid_digest"] != cameraDigest || request["password"] != "camera-secret" {
		t.Fatalf("camera received wrong scoped request: %v", request)
	}

	send(t, camera, map[string]any{"type": "offer", "id": viewerID, "sdp": "camera-offer"})
	if reply := receive(viewer, time.Second); reply == nil || reply["type"] != "offer" || reply["id"] != "camera0" {
		t.Fatalf("scoped camera reply did not reach viewer: %v", reply)
	}
	send(t, viewer, map[string]any{"type": "answer", "id": "camera0", "password": "must-not-forward"})
	if reply := receive(camera, time.Second); reply == nil || reply["type"] != "answer" {
		t.Fatalf("viewer answer did not reach camera: %v", reply)
	} else if _, hasPassword := reply["password"]; hasPassword {
		t.Fatalf("password was forwarded outside initial request: %v", reply)
	}
	send(t, otherCamera, map[string]any{"type": "offer", "id": viewerID, "sdp": "wrong-camera"})
	if reply := receive(viewer, 200*time.Millisecond); reply != nil {
		t.Fatalf("another camera reached the scoped viewer: %v", reply)
	}
}

func TestUnscopedPlayerTicketCannotRequestProductionCamera(t *testing.T) {
	n := newNode(t, nil)
	camera := n.connect("camera0", "acme01", admission.RoleCamera)
	viewerID := "LegacyViewer123"
	viewer, response := n.dial("/" + viewerID + "?ticket=" + url.QueryEscape(n.ticket(viewerID, "acme01", admission.RolePlayer)))
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("player ticket status=%d", response.StatusCode)
	}
	defer viewer.Close()
	n.waitRegistered("acme01", viewerID)
	send(t, viewer, map[string]any{"type": "request", "id": "camera0", "password": "must-not-bypass"})
	if reply := receive(viewer, time.Second); reply == nil || reply["msg"] != "Device offline" {
		t.Fatalf("unscoped player should get offline response: %v", reply)
	}
	if got := receive(camera, 200*time.Millisecond); got != nil {
		t.Fatalf("unscoped player reached camera: %v", got)
	}
}

func TestSameIDInTwoTenantsCoexists(t *testing.T) {
	n := newNode(t, nil)
	cameraA := n.connect("camera0", "acme01", "camera")
	n.connect("camera0", "other01", "camera")
	n.srv.mu.RLock()
	count := len(n.srv.clients)
	n.srv.mu.RUnlock()
	if count != 2 {
		t.Fatalf("clients = %d, want 2: another tenant evicted the first", count)
	}
	// the first connection must still be open
	playerA := n.connectScopedViewer("PlayerAAAAAA", "acme01", "camera0")
	send(t, playerA, map[string]any{"type": "request", "id": "camera0", "password": "test-pass"})
	if got := receive(cameraA, time.Second); got == nil {
		t.Fatal("first tenant's camera was disconnected by the second tenant's camera")
	}
}

func TestReconnectInSameTenantReplacesStaleConnection(t *testing.T) {
	n := newNode(t, nil)
	old := n.connect("camera0", "acme01", "camera")
	fresh := n.connect("camera0", "acme01", "camera")
	_ = old.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := old.ReadMessage(); err == nil {
		t.Fatal("stale connection still open after the device reconnected")
	}
	player := n.connectScopedViewer("PlayerAAAAAA", "acme01", "camera0")
	send(t, player, map[string]any{"type": "request", "id": "camera0", "password": "test-pass"})
	if got := receive(fresh, time.Second); got == nil {
		t.Fatal("replacement connection does not receive messages")
	}
}

func TestTurnCredentialsOnlyGoToPlayerRequests(t *testing.T) {
	n := newNode(t, nil)
	camera := n.connect("camera0", "acme01", "camera")
	player := n.connectScopedViewer("PlayerAAAAAA", "acme01", "camera0")
	send(t, player, map[string]any{"type": "request", "id": "camera0", "password": "test-pass"})
	got := receive(camera, time.Second)
	if got == nil || got["turn"] == nil {
		t.Fatalf("player request lacks TURN servers: %v", got)
	}
	send(t, camera, map[string]any{"type": "request", "id": "PlayerAAAAAA"})
	back := receive(player, time.Second)
	if back == nil || back["turn"] != nil {
		t.Fatalf("camera-originated message carried TURN credentials: %v", back)
	}
}

func TestMessageBodiesAreNotLogged(t *testing.T) {
	var captured strings.Builder
	log.SetOutput(&captured)
	defer log.SetOutput(os.Stderr)
	n := newNode(t, nil)
	camera := n.connect("camera0", "acme01", "camera")
	player := n.connectScopedViewer("PlayerAAAAAA", "acme01", "camera0")
	send(t, player, map[string]any{"type": "request", "id": "camera0", "sdp": "SDP-PAYLOAD-MARKER", "password": "secret-credential"})
	if receive(camera, time.Second) == nil {
		t.Fatal("request not delivered")
	}
	logged := captured.String()
	if strings.Contains(logged, "secret-credential") || strings.Contains(logged, "SDP-PAYLOAD-MARKER") {
		t.Fatalf("message bodies or TURN credentials were logged:\n%s", logged)
	}
}

// --- demo mode ---

func TestDemoTenantAllowsAnonymousPlayersOfThatTenantOnly(t *testing.T) {
	n := newNode(t, func(s *Server) { s.SetDemoTenant("demo0001") })
	demoCamera := n.connect("demo-camera", "demo0001", "camera")
	prodCamera := n.connect("prod-camera", "acme01", "camera")

	anonymous, response := n.dial("/PlayerWeb123")
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("anonymous demo player status = %d", response.StatusCode)
	}
	defer anonymous.Close()
	n.waitRegistered("demo0001", "PlayerWeb123")

	send(t, anonymous, map[string]any{"type": "request", "id": "demo-camera", "password": "test-pass"})
	if got := receive(demoCamera, time.Second); got == nil || got["id"] != "PlayerWeb123" {
		t.Fatalf("demo camera did not receive the demo player's request: %v", got)
	}
	send(t, anonymous, map[string]any{"type": "request", "id": "prod-camera", "password": "test-pass"})
	if reply := receive(anonymous, time.Second); reply == nil || reply["msg"] != "Device offline" {
		t.Fatalf("demo player could reach a production tenant: %v", reply)
	}
	if leaked := receive(prodCamera, 300*time.Millisecond); leaked != nil {
		t.Fatalf("production camera received a demo message: %v", leaked)
	}
	// Demo mode does not open cameras: an anonymous non-Player client is refused.
	if _, response := n.dial("/anon-camera"); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous camera status = %d, want 401", response.StatusCode)
	}
}

func TestDevicesEndpointNeverListsOtherTenants(t *testing.T) {
	list := func(n *node) (int, []DeviceInfo) {
		request, _ := http.NewRequest(http.MethodGet, n.http.URL+"/api/devices", nil)
		response, err := n.http.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var body struct {
			Devices []DeviceInfo `json:"devices"`
		}
		_ = json.NewDecoder(response.Body).Decode(&body)
		return response.StatusCode, body.Devices
	}
	plain := newNode(t, nil)
	plain.connect("camera0", "acme01", "camera")
	if status, _ := list(plain); status != http.StatusNotFound {
		t.Fatalf("/api/devices under tenancy without demo = %d, want 404", status)
	}
	demo := newNode(t, func(s *Server) { s.SetDemoTenant("demo0001") })
	demo.connect("demo-camera", "demo0001", "camera")
	demo.connect("prod-camera", "acme01", "camera")
	status, devices := list(demo)
	if status != http.StatusOK || len(devices) != 1 || devices[0].ID != "demo-camera" {
		t.Fatalf("demo list = %d %+v", status, devices)
	}
}

func TestDiscoveryTenantListsOnlyCameraMetadataAndDoesNotWidenAdmission(t *testing.T) {
	n := newNode(t, func(s *Server) { s.SetDiscoveryTenant("test01") })
	cameraSID := base64.RawURLEncoding.EncodeToString(make([]byte, sid.RawSize))
	ticket := n.ticket("test-camera", "test01", admission.RoleCamera, func(c *admission.Claims) {
		c.Digest = center.Digest(cameraSID)
	})
	camera, cameraResponse := n.dial("/test-camera?ticket=" + ticket)
	if cameraResponse.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("camera connect status = %d", cameraResponse.StatusCode)
	}
	defer camera.Close()
	n.t.Cleanup(func() { camera.Close() })
	n.waitRegistered("test01", "test-camera")
	n.connect("second-camera", "test01", admission.RoleCamera)
	n.connect("other-camera", "other01", admission.RoleCamera)
	nickname := "Dev camera"
	send(t, camera, map[string]any{"type": "register", "nickname": nickname, "sid": cameraSID})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n.srv.mu.RLock()
		registered := n.srv.clients[clientKey("test01", "test-camera")]
		updated := registered != nil && registered.Info.Nickname == nickname
		n.srv.mu.RUnlock()
		if updated {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	response, err := n.http.Client().Get(n.http.URL + "/api/devices")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("discovery list status = %d, want 200", response.StatusCode)
	}
	devices, ok := body["devices"].([]any)
	if !ok || len(devices) != 2 {
		t.Fatalf("discovery devices = %v, want both cameras from the configured tenant", body["devices"])
	}
	foundNickname := false
	for _, rawDevice := range devices {
		device, ok := rawDevice.(map[string]any)
		if !ok {
			t.Fatalf("discovery card has unexpected shape: %v", rawDevice)
		}
		id, _ := device["id"].(string)
		if id == "test-camera" && device["nickname"] == nickname {
			if device["sid"] != cameraSID {
				t.Fatalf("discovery did not include authenticated camera SID: %v", device)
			}
			foundNickname = true
		}
		if id != "test-camera" && id != "second-camera" {
			t.Fatalf("discovery exposed a device outside tenant test01: %q", id)
		}
		for _, secretField := range []string{"sid_digest", "password", "ticket"} {
			if _, exposed := device[secretField]; exposed {
				t.Fatalf("discovery metadata exposed secret field %q", secretField)
			}
		}
	}
	if !foundNickname {
		t.Fatalf("discovery did not include nickname %q", nickname)
	}

	if _, anonymousResponse := n.dial("/PlayerAnonymous01"); anonymousResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("discovery-only anonymous WSS status = %d, want 401", anonymousResponse.StatusCode)
	}

	healthResponse, err := n.http.Client().Get(n.http.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	var health map[string]any
	if err := json.NewDecoder(healthResponse.Body).Decode(&health); err != nil {
		healthResponse.Body.Close()
		t.Fatal(err)
	}
	healthResponse.Body.Close()
	if health["ticket_admission"] != true {
		t.Fatalf("health ticket_admission = %v, want true", health["ticket_admission"])
	}
}

func TestDiscoveryTenantRequiresValidTenantAndAdmission(t *testing.T) {
	invalid := NewServer("", "")
	if err := configureServer(invalid, serverOptions{discoveryTenant: "bad"}); err == nil {
		t.Fatal("invalid discovery tenant was accepted")
	}
	noAdmission := NewServer("", "")
	if err := configureServer(noAdmission, serverOptions{discoveryTenant: "test01"}); err == nil || !strings.Contains(err.Error(), "requires ticket admission") {
		t.Fatalf("discovery without ticket admission error = %v", err)
	}
}

// --- legacy mode (no admission configured) keeps working for development ---

func TestLegacyModeStillRoutesWithoutAdmission(t *testing.T) {
	srv := NewServer("", "")
	httpServer := httptest.NewTLSServer(srv)
	defer httpServer.Close()
	dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	base := "wss" + strings.TrimPrefix(httpServer.URL, "https")
	camera, _, err := dialer.Dial(base+"/camera0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer camera.Close()
	player, _, err := dialer.Dial(base+"/PlayerOld", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer player.Close()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		srv.mu.RLock()
		n := len(srv.clients)
		srv.mu.RUnlock()
		if n == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	send(t, player, map[string]any{"type": "request", "id": "camera0", "password": "test-pass"})
	if got := receive(camera, time.Second); got == nil || got["id"] != "PlayerOld" {
		t.Fatalf("legacy routing broke: %v", got)
	}
}

// A handshake that fails (here: a plain HTTP request without Upgrade headers)
// must not spend the ticket; the same ticket then still opens a connection.
func TestFailedUpgradeDoesNotBurnTicket(t *testing.T) {
	n := newNode(t, nil)
	ticket := n.ticket("camera0", "acme01", "camera")
	path := "/camera0?ticket=" + url.QueryEscape(ticket)
	client := n.http.Client()
	response, err := client.Get(n.http.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("plain GET must not upgrade")
	}
	conn, response := n.dial(path)
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("ticket was burned by the failed upgrade: status %d", response.StatusCode)
	}
	conn.Close()
}

// The camera binary is served with its own write deadline, so the listener's
// 10 s limit does not cut off a slow download.
func TestDownloadsExtendWriteDeadline(t *testing.T) {
	n := newNode(t, nil)
	if err := os.MkdirAll(n.srv.rootDir+"downloads", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(n.srv.rootDir+"downloads/webrtc", []byte("binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	response, err := n.http.Client().Get(n.http.URL + "/downloads/webrtc")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
}

// A connection that is already open is ended by the sweep once its device is
// revoked (SID released to another device), its digest is blocked or its
// tenant is suspended; other clients stay.
func TestSweepDropsRevokedAndBlockedClients(t *testing.T) {
	n := newNode(t, nil)
	revoked := n.connect("camera0", "acme01", "camera")
	blockedDigest := n.connect("camera1", "acme01", "camera")
	suspended := n.connect("camera2", "other01", "camera")
	staying := n.connect("camera3", "acme01", "camera")

	list := n.srv.admission.Blocklist
	if err := list.RevokeDevice("camera0", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("sid-acme01camera1"))
	f, err := os.OpenFile(n.blocklist, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, "%s\ntenant:other01\n", hex.EncodeToString(digest[:]))
	f.Close()

	n.srv.sweep(time.Now())

	closed := func(c *websocket.Conn) bool {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, err := c.ReadMessage()
		return err != nil
	}
	for name, conn := range map[string]*websocket.Conn{"revoked device": revoked, "blocked digest": blockedDigest, "suspended tenant": suspended} {
		if !closed(conn) {
			t.Errorf("%s stayed connected after the sweep", name)
		}
	}
	n.srv.mu.RLock()
	_, stays := n.srv.clients[clientKey("acme01", "camera3")]
	n.srv.mu.RUnlock()
	if !stays {
		t.Error("an unaffected client was dropped")
	}
	_ = staying
}

// An expired revocation no longer drops anyone.
func TestSweepIgnoresExpiredRevocation(t *testing.T) {
	n := newNode(t, nil)
	n.connect("camera0", "acme01", "camera")
	if err := n.srv.admission.Blocklist.RevokeDevice("camera0", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	n.srv.sweep(time.Now())
	n.srv.mu.RLock()
	_, stays := n.srv.clients[clientKey("acme01", "camera0")]
	n.srv.mu.RUnlock()
	if !stays {
		t.Fatal("an expired revocation dropped the client")
	}
}
