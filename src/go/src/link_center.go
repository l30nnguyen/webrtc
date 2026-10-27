package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"signaling-server/internal/fleet"
	"signaling-server/internal/link"
)

// linkHub is Center's end of the node links (see package link): it logs nodes
// in, feeds their presence into the fleet registry and pushes the signed
// configuration to them.
type linkHub struct {
	registry *fleet.Registry
	key      ed25519.PrivateKey // Center ticket key: signs configurations
	name     string             // Center host name the nodes sign at login
	pending  chan struct{}      // connections that have not logged in yet
	config   atomic.Pointer[[]byte]

	mu    sync.Mutex
	links map[string]*centerLink // node address -> live link
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
	return &linkHub{registry: registry, key: key, name: name,
		pending: make(chan struct{}, link.MaxPendingLogins), links: map[string]*centerLink{}}
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
	if r.URL.Path != link.Path {
		http.NotFound(w, r)
		return
	}
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
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	address, ok := h.login(conn)
	if !ok {
		log.Printf("link: login refused from %s", peer)
		conn.Close()
		return
	}
	loggedIn = true
	<-h.pending
	h.serve(conn, address, peer)
}

// login runs the challenge and returns the node's address when its
// signature checks out against its key in the configuration.
func (h *linkHub) login(conn *websocket.Conn) (string, bool) {
	conn.SetReadLimit(link.MaxLoginBytes)
	deadline := time.Now().Add(link.LoginTimeout)
	conn.SetReadDeadline(deadline)
	conn.SetWriteDeadline(deadline)
	nonce := make([]byte, link.NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", false
	}
	if conn.WriteJSON(link.Message{Type: "challenge", Nonce: base64.RawURLEncoding.EncodeToString(nonce)}) != nil {
		return "", false
	}
	var login link.Message
	if conn.ReadJSON(&login) != nil || login.Type != "login" {
		return "", false
	}
	key, ok := h.registry.LinkKey(login.Address)
	sig, err := base64.RawURLEncoding.DecodeString(login.Sig)
	if !ok || err != nil || !ed25519.Verify(key, link.LoginMessage(nonce, h.name, login.Address), sig) {
		return "", false
	}
	return login.Address, true
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
	gen, err := h.registry.Linked(fleet.Report{Address: address, Cameras: hello.Cameras, Clients: hello.Clients, Stats: hello.Stats}, hello.Version)
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
	log.Printf("link: node %s connected from %s (%d cameras, config version %d)", address, peer, len(hello.Cameras), hello.Version)

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
			h.registry.NodeStats(l.address, l.gen, m.Clients, m.Stats)
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

// startLinkListener opens Center's private link port with the server's
// certificate. Bind it to the private (VPC) address and let only the nodes
// reach it (see the design document's DigitalOcean setup).
func (s *Server) startLinkListener(listen, name, mainAddress string, tlsConfig *tls.Config) error {
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
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: hub, ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 8 << 10}
	s.hub = hub
	log.Printf("Node link port listening on %s (Center name %s)", listen, name)
	go func() {
		log.Fatalf("link port: %v", server.Serve(tls.NewListener(listener, tlsConfig.Clone())))
	}()
	return nil
}
