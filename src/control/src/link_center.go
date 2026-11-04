package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"signaling-server/internal/fleet"
	"signaling-server/internal/link"
)

// Limits on link attempts from one IP address.
const (
	linkMaxFailures   = 5
	linkFailureWindow = time.Hour
	linkBlockTime     = time.Hour
	linkResolveEvery  = 5 * time.Minute
)

// linkHub is Center's end of the node links (see package link): it logs nodes
// in, feeds their presence into the fleet registry and pushes the signed
// configuration to them.
type linkHub struct {
	registry *fleet.Registry
	key      ed25519.PrivateKey // Center ticket key: signs configurations and challenges
	name     string             // Center host name the nodes sign at login
	pending  chan struct{}      // connections that have not logged in yet
	config   atomic.Pointer[[]byte]
	// allowed maps each node address to the IP prefixes its link may come from.
	allowed   atomic.Pointer[map[string][]netip.Prefix]
	lookup    func(ctx context.Context, host string) ([]netip.Addr, error)
	resolveMu sync.Mutex

	mu    sync.Mutex
	links map[string]*centerLink // node address -> live link

	guardMu    sync.Mutex
	inProgress map[netip.Addr]bool          // IPs with an unfinished login
	failures   map[netip.Addr]*linkFailures // IPs whose attempts failed
	nonces     map[string]time.Time         // upgrade nonces seen -> when they expire
}

type linkFailures struct {
	count        int
	since        time.Time
	blockedUntil time.Time
}

type centerLink struct {
	address string
	gen     uint64
	conn    *websocket.Conn
	send    chan []byte
	done    chan struct{}
	once    sync.Once
}

func (l *centerLink) close() {
	l.once.Do(func() {
		close(l.done)
		l.conn.Close()
	})
}

func newLinkHub(registry *fleet.Registry, key ed25519.PrivateKey, name string) *linkHub {
	h := &linkHub{registry: registry, key: key, name: name,
		pending: make(chan struct{}, link.MaxPendingLogins), links: map[string]*centerLink{},
		inProgress: map[netip.Addr]bool{}, failures: map[netip.Addr]*linkFailures{}, nonces: map[string]time.Time{},
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}}
	h.allowed.Store(&map[string][]netip.Prefix{})
	return h
}

// resolve rebuilds the IP prefixes each node may link from: its link_from,
// or the addresses its host name resolves to. A name that does not resolve
// keeps its last addresses.
func (h *linkHub) resolve() {
	h.resolveMu.Lock()
	defer h.resolveMu.Unlock()
	old := *h.allowed.Load()
	next := map[string][]netip.Prefix{}
	for _, n := range h.registry.Config().Nodes {
		if n.PublicKey == "" {
			continue
		}
		if len(n.LinkFrom) > 0 {
			for _, item := range n.LinkFrom {
				if prefix, err := fleet.ParsePrefix(item); err == nil {
					next[n.Address] = append(next[n.Address], prefix)
				}
			}
			continue
		}
		u, err := url.Parse(n.Address)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		addrs, err := h.lookup(ctx, u.Hostname())
		cancel()
		if err != nil || len(addrs) == 0 {
			log.Printf("link: cannot resolve node %s (keeping its last addresses): %v", u.Hostname(), err)
			next[n.Address] = old[n.Address]
			continue
		}
		for _, addr := range addrs {
			addr = addr.Unmap()
			next[n.Address] = append(next[n.Address], netip.PrefixFrom(addr, addr.BitLen()))
		}
	}
	h.allowed.Store(&next)
}

// resolveLoop follows nodes' DNS changes.
func (h *linkHub) resolveLoop() {
	for range time.Tick(linkResolveEvery) {
		h.resolve()
	}
}

// fromNode reports whether ip may link: as node address when address is set,
// or as any node otherwise.
func (h *linkHub) fromNode(ip netip.Addr, address string) bool {
	for node, prefixes := range *h.allowed.Load() {
		if address != "" && node != address {
			continue
		}
		for _, prefix := range prefixes {
			if prefix.Contains(ip) {
				return true
			}
		}
	}
	return false
}

func (h *linkHub) blocked(ip netip.Addr) bool {
	h.guardMu.Lock()
	defer h.guardMu.Unlock()
	f := h.failures[ip]
	return f != nil && time.Now().Before(f.blockedUntil)
}

// failed counts a failed attempt from ip and blocks it after too many.
func (h *linkHub) failed(ip netip.Addr, reason string) {
	h.guardMu.Lock()
	defer h.guardMu.Unlock()
	now := time.Now()
	f := h.failures[ip]
	if f == nil || now.Sub(f.since) > linkFailureWindow {
		f = &linkFailures{since: now}
		h.failures[ip] = f
	}
	f.count++
	log.Printf("link: refused %s: %s", ip, reason)
	if f.count >= linkMaxFailures {
		f.blockedUntil = now.Add(linkBlockTime)
		f.count, f.since = 0, now
		log.Printf("link: %s failed %d times; ignoring it for %s", ip, linkMaxFailures, linkBlockTime)
	}
}

// begin marks ip as logging in; false when it already is.
func (h *linkHub) begin(ip netip.Addr) bool {
	h.guardMu.Lock()
	defer h.guardMu.Unlock()
	if h.inProgress[ip] {
		return false
	}
	h.inProgress[ip] = true
	return true
}

func (h *linkHub) end(ip netip.Addr) {
	h.guardMu.Lock()
	defer h.guardMu.Unlock()
	delete(h.inProgress, ip)
}

// freshNonce records an upgrade nonce; false when it was used already.
func (h *linkHub) freshNonce(nonce string) bool {
	h.guardMu.Lock()
	defer h.guardMu.Unlock()
	now := time.Now()
	for n, expires := range h.nonces {
		if now.After(expires) {
			delete(h.nonces, n)
		}
	}
	if _, seen := h.nonces[nonce]; seen {
		return false
	}
	h.nonces[nonce] = now.Add(2 * link.HeaderWindow)
	return true
}

// checkHeader verifies the upgrade request's signed headers (step 0) and
// returns the node's address and upgrade nonce.
func (h *linkHub) checkHeader(r *http.Request, ip netip.Addr) (string, []byte, string) {
	address := r.Header.Get(link.HeaderNode)
	key, ok := h.registry.LinkKey(address)
	if !ok {
		return "", nil, "unknown node"
	}
	if !h.fromNode(ip, address) {
		return "", nil, "not an address of node " + address
	}
	unix, err := strconv.ParseInt(r.Header.Get(link.HeaderTime), 10, 64)
	if err != nil {
		return "", nil, "no time"
	}
	if skew := time.Since(time.Unix(unix, 0)); skew > link.HeaderWindow || skew < -link.HeaderWindow {
		return "", nil, "time off by " + skew.Round(time.Second).String() + " (check the node's clock)"
	}
	nonceText := r.Header.Get(link.HeaderNonce)
	nonce, err := base64.RawURLEncoding.DecodeString(nonceText)
	if err != nil || len(nonce) != link.NonceSize {
		return "", nil, "bad nonce"
	}
	sig, err := base64.RawURLEncoding.DecodeString(r.Header.Get(link.HeaderSig))
	if err != nil || !ed25519.Verify(key, link.HeaderMessage(unix, nonce, h.name, address), sig) {
		return "", nil, "bad signature for node " + address
	}
	if !h.freshNonce(nonceText) {
		return "", nil, "repeated request"
	}
	return address, nonce, ""
}

var linkUpgrader = websocket.Upgrader{
	HandshakeTimeout: 5 * time.Second,
	// Nodes are programs, not browsers: refuse anything a web page sends.
	CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" },
}

// SetConfig signs config and pushes it to every linked node; nodes already
// running that version ignore it.
func (h *linkHub) SetConfig(config fleet.Config) {
	data, err := json.Marshal(config)
	if err != nil {
		log.Printf("link: cannot encode configuration: %v", err)
		return
	}
	message, err := json.Marshal(link.SignConfig(h.key, config.Version, data))
	if err != nil {
		return
	}
	h.config.Store(&message)
	go h.resolve()
	h.mu.Lock()
	defer h.mu.Unlock()
	for address, l := range h.links {
		// A node removed from the configuration, or given a new key, must
		// log in again (and is refused if it is no longer listed).
		if !h.registry.Current(address, l.gen) {
			l.close()
			continue
		}
		l.queue(message)
	}
}

func (l *centerLink) queue(message []byte) {
	select {
	case l.send <- message:
	case <-l.done:
	default:
		log.Printf("link: node %s is not reading; dropping its link", l.address)
		l.close()
	}
}

func (h *linkHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// To anyone but a node, the link looks like any unknown path.
	addrPort, err := netip.ParseAddrPort(r.RemoteAddr)
	if r.URL.Path != link.Path || err != nil {
		http.NotFound(w, r)
		return
	}
	ip := addrPort.Addr().Unmap()
	if !h.fromNode(ip, "") || h.blocked(ip) {
		http.NotFound(w, r)
		return
	}
	address, nodeNonce, reason := h.checkHeader(r, ip)
	if reason != "" {
		h.failed(ip, reason)
		http.NotFound(w, r)
		return
	}
	if !h.begin(ip) {
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	defer h.end(ip)
	select {
	case h.pending <- struct{}{}:
	default:
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	loggedIn := false
	defer func() {
		if !loggedIn {
			<-h.pending
		}
	}()
	conn, err := linkUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	if !h.login(conn, address, nodeNonce) {
		h.failed(ip, "login refused for node "+address)
		conn.Close()
		return
	}
	loggedIn = true
	<-h.pending
	h.end(ip)
	h.serve(conn, address, ip.String())
}

// login runs the challenge, signed by Center, and checks the node's
// signature over it against its key in the configuration.
func (h *linkHub) login(conn *websocket.Conn, address string, nodeNonce []byte) bool {
	conn.SetReadLimit(link.MaxLoginBytes)
	deadline := time.Now().Add(link.LoginTimeout)
	conn.SetReadDeadline(deadline)
	conn.SetWriteDeadline(deadline)
	nonce := make([]byte, link.NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return false
	}
	centerSig := ed25519.Sign(h.key, link.CenterMessage(nodeNonce, nonce, h.name, address))
	if conn.WriteJSON(link.Message{Type: "challenge", Nonce: base64.RawURLEncoding.EncodeToString(nonce),
		Sig: base64.RawURLEncoding.EncodeToString(centerSig)}) != nil {
		return false
	}
	var login link.Message
	if conn.ReadJSON(&login) != nil || login.Type != "login" || login.Address != address {
		return false
	}
	key, ok := h.registry.LinkKey(address)
	sig, err := base64.RawURLEncoding.DecodeString(login.Sig)
	return ok && err == nil && ed25519.Verify(key, link.LoginMessage(nonce, h.name, address), sig)
}

func (h *linkHub) serve(conn *websocket.Conn, address, peer string) {
	conn.SetReadLimit(link.MaxMessageBytes)
	conn.SetReadDeadline(time.Now().Add(link.ReadTimeout))
	conn.SetWriteDeadline(time.Time{})
	var hello link.Message
	if conn.ReadJSON(&hello) != nil || hello.Type != "hello" {
		conn.Close()
		return
	}
	gen, err := h.registry.Linked(fleet.Report{Address: address, Cameras: hello.Cameras, Clients: hello.Clients, Stats: hello.Stats,
		Machine: hello.Machine, Version: hello.ServerVersion}, hello.Version)
	if err != nil {
		log.Printf("link: node %s refused: %v", address, err)
		conn.Close()
		return
	}
	l := &centerLink{address: address, gen: gen, conn: conn, send: make(chan []byte, 16), done: make(chan struct{})}
	h.mu.Lock()
	if old := h.links[address]; old != nil {
		old.close()
	}
	h.links[address] = l
	h.mu.Unlock()
	h.registry.NodeStats(address, gen, hello.Clients, hello.Stats, hello.Relays, nil, "")
	log.Printf("link: node %s connected from %s (%d cameras, config version %d)", address, peer, len(hello.Cameras), hello.Version)

	if welcome, err := json.Marshal(link.Message{Type: "welcome"}); err == nil {
		l.queue(welcome)
	}
	if message := h.config.Load(); message != nil {
		var current link.Message
		if json.Unmarshal(*message, &current) == nil && current.Version > hello.Version {
			l.queue(*message)
		}
	}
	go h.writeLoop(l)
	h.readLoop(l)

	l.close()
	h.registry.Unlinked(address, gen)
	h.mu.Lock()
	if h.links[address] == l {
		delete(h.links, address)
	}
	h.mu.Unlock()
	log.Printf("link: node %s disconnected", address)
}

func (h *linkHub) writeLoop(l *centerLink) {
	ping := time.NewTicker(link.PingInterval)
	defer ping.Stop()
	for {
		select {
		case message := <-l.send:
			l.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if l.conn.WriteMessage(websocket.TextMessage, message) != nil {
				l.close()
				return
			}
		case <-ping.C:
			if l.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
				l.close()
				return
			}
		case <-l.done:
			return
		}
	}
}

func (h *linkHub) readLoop(l *centerLink) {
	l.conn.SetPongHandler(func(string) error {
		return l.conn.SetReadDeadline(time.Now().Add(link.ReadTimeout))
	})
	for {
		var m link.Message
		if l.conn.ReadJSON(&m) != nil {
			return
		}
		l.conn.SetReadDeadline(time.Now().Add(link.ReadTimeout))
		switch m.Type {
		case "camera":
			h.registry.Camera(l.address, l.gen, m.ID, m.Online)
		case "stats":
			h.registry.NodeStats(l.address, l.gen, m.Clients, m.Stats, m.Relays, m.Machine, m.ServerVersion)
		case "ack":
			h.registry.ConfigAck(l.address, l.gen, m.Version)
			log.Printf("link: node %s runs configuration version %d", l.address, m.Version)
		case "config_error":
			log.Printf("link: node %s refused configuration version %d: %s", l.address, m.Version, m.Error)
		default:
			log.Printf("link: node %s sent an unknown message; dropping its link", l.address)
			return
		}
	}
}

// startLinkHub serves the node links at link.Path on the public port. Only
// the nodes' own IP addresses get past a 404 (see package link).
func (s *Server) startLinkHub(name, mainAddress string) error {
	if s.fleet == nil || s.centerKey == nil {
		return errors.New("needs -center and -config-file")
	}
	if name == "" {
		u, err := url.Parse(mainAddress)
		if err != nil || u.Hostname() == "" {
			return errors.New("cannot take the host name from -main-address; set -link-name")
		}
		name = u.Hostname()
	}
	hub := newLinkHub(s.fleet, s.centerKey, name)
	hub.SetConfig(s.fleet.Config())
	hub.resolve()
	go hub.resolveLoop()
	s.hub = hub
	log.Printf("Node links at %s (Center name %s)", link.Path, name)
	return nil
}

// startLinkListener also serves the node links on a separate port, for
// example a private (VPC) address.
func (s *Server) startLinkListener(listen string, tlsConfig *tls.Config) error {
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: s.hub, ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 8 << 10}
	log.Printf("Node link port listening on %s", listen)
	go func() {
		log.Fatalf("link port: %v", server.Serve(tls.NewListener(listener, tlsConfig.Clone())))
	}()
	return nil
}
