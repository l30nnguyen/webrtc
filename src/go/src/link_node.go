package main

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	mathrand "math/rand"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"signaling-server/internal/admission"
	"signaling-server/internal/fleet"
	"signaling-server/internal/link"
)

// nodeLink is a control node's end of its link to Center (see package link).
type nodeLink struct {
	srv        *Server
	url        string // wss://center:port/v1/link
	serverName string // Center host name: TLS check and login signature
	address    string // this node's WSS root, as listed in config.json
	key        ed25519.PrivateKey
	centerKey  ed25519.PublicKey // verifies pushed configurations
	tls        *tls.Config
	configPath string // last accepted configuration, kept across restarts

	events    chan link.Message // camera online/offline, sent in order
	resync    atomic.Bool       // events were dropped: reconnect to resend all
	connected atomic.Bool       // Center welcomed this link

	mu      sync.Mutex
	version int
	config  fleet.Config
}

type nodeConfigFile struct {
	Message link.Message `json:"message"`
}

func newNodeLink(srv *Server, url, serverName, address string, key ed25519.PrivateKey, centerKey ed25519.PublicKey,
	tlsConfig *tls.Config, configPath string) *nodeLink {
	return &nodeLink{srv: srv, url: url, serverName: serverName, address: address, key: key, centerKey: centerKey,
		tls: tlsConfig, configPath: configPath, events: make(chan link.Message, 4096)}
}

// cameraChanged is called by the server when a camera connects or leaves.
// It never blocks: when Center is slow the link is rebuilt with a full list.
func (n *nodeLink) cameraChanged(id string, online bool) {
	select {
	case n.events <- link.Message{Type: "camera", ID: id, Online: online}:
	default:
		n.resync.Store(true)
	}
}

// Config returns the configuration Center last pushed, and its version
// (0 before the first one).
func (n *nodeLink) Config() (fleet.Config, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.config, n.version
}

// loadSaved restores the last configuration accepted from Center, checking
// its signature again.
func (n *nodeLink) loadSaved() {
	data, err := os.ReadFile(n.configPath)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	var saved nodeConfigFile
	if err == nil {
		err = json.Unmarshal(data, &saved)
	}
	if err == nil {
		err = n.accept(saved.Message, false)
	}
	if err != nil {
		log.Printf("link: ignoring saved configuration %s: %v", n.configPath, err)
		return
	}
	log.Printf("link: using saved configuration version %d", n.version)
}

// accept checks a pushed configuration and makes it the node's own.
func (n *nodeLink) accept(m link.Message, save bool) error {
	data, err := link.VerifyConfig(n.centerKey, m)
	if err != nil {
		return err
	}
	config, err := fleet.ParseConfig(data)
	if err != nil {
		return err
	}
	if config.Version != m.Version {
		return fmt.Errorf("signed version %d does not match the content's %d", m.Version, config.Version)
	}
	if err := config.Validate(""); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if m.Version <= n.version {
		return fmt.Errorf("version %d is not newer than %d", m.Version, n.version)
	}
	if save {
		if err := writeFileAtomic(n.configPath, nodeConfigFile{Message: m}); err != nil {
			return fmt.Errorf("cannot save it: %w", err)
		}
	}
	n.config, n.version = config, m.Version
	return nil
}

func writeFileAtomic(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".node-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// run keeps the link up, reconnecting with a growing, jittered wait.
func (n *nodeLink) run() {
	wait := time.Second
	reported := false
	for {
		err := n.connectOnce()
		if n.connected.Swap(false) {
			log.Printf("link: Center link lost: %v", err)
			wait, reported = time.Second, true
		} else if !reported {
			log.Printf("link: cannot reach Center: %v", err)
			reported = true
		}
		time.Sleep(wait + time.Duration(mathrand.Int63n(int64(wait))))
		if wait < 30*time.Second {
			wait *= 2
		}
	}
}

func (n *nodeLink) connectOnce() error {
	dialer := websocket.Dialer{TLSClientConfig: n.tls, HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.Dial(n.url, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetReadLimit(link.MaxMessageBytes)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var challenge link.Message
	if err := conn.ReadJSON(&challenge); err != nil || challenge.Type != "challenge" {
		return fmt.Errorf("no challenge: %v", err)
	}
	nonce, err := base64.RawURLEncoding.DecodeString(challenge.Nonce)
	if err != nil || len(nonce) != link.NonceSize {
		return errors.New("bad challenge")
	}
	sig := ed25519.Sign(n.key, link.LoginMessage(nonce, n.serverName, n.address))
	var writeMu sync.Mutex
	write := func(m link.Message) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteJSON(m)
	}
	if err := write(link.Message{Type: "login", Address: n.address, Sig: base64.RawURLEncoding.EncodeToString(sig)}); err != nil {
		return err
	}

	// The full list replaces anything queued before it.
	for len(n.events) > 0 {
		<-n.events
	}
	n.resync.Store(false)
	report := n.srv.LocalReport(n.address)
	_, version := n.Config()
	if err := write(link.Message{Type: "hello", Cameras: report.Cameras, Clients: report.Clients, Stats: report.Stats,
		Relays: n.srv.relayStatuses(), Version: version}); err != nil {
		return err
	}

	done := make(chan struct{})
	defer close(done)
	go n.sendLoop(conn, write, done)

	conn.SetReadDeadline(time.Now().Add(link.ReadTimeout))
	conn.SetPingHandler(func(data string) error {
		conn.SetReadDeadline(time.Now().Add(link.ReadTimeout))
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})
	for {
		var m link.Message
		if err := conn.ReadJSON(&m); err != nil {
			return err
		}
		conn.SetReadDeadline(time.Now().Add(link.ReadTimeout))
		if m.Type == "welcome" {
			// Center accepted the login and the camera list.
			n.connected.Store(true)
			log.Printf("link: connected to Center %s as %s", n.serverName, n.address)
			continue
		}
		if m.Type != "config" {
			continue
		}
		if err := n.accept(m, true); err != nil {
			log.Printf("link: configuration version %d refused: %v", m.Version, err)
			if err := write(link.Message{Type: "config_error", Version: m.Version, Error: err.Error()}); err != nil {
				return err
			}
			continue
		}
		log.Printf("link: configuration version %d applied", m.Version)
		if err := write(link.Message{Type: "ack", Version: m.Version}); err != nil {
			return err
		}
	}
}

// sendLoop forwards camera changes as they happen and counters every
// link.StatsInterval.
func (n *nodeLink) sendLoop(conn *websocket.Conn, write func(link.Message) error, done chan struct{}) {
	ticker := time.NewTicker(link.StatsInterval)
	defer ticker.Stop()
	resync := time.NewTicker(time.Second)
	defer resync.Stop()
	for {
		select {
		case m := <-n.events:
			if write(m) != nil {
				conn.Close()
				return
			}
		case <-ticker.C:
			report := n.srv.LocalReport(n.address)
			if write(link.Message{Type: "stats", Clients: report.Clients, Stats: report.Stats, Relays: n.srv.relayStatuses()}) != nil {
				conn.Close()
				return
			}
		case <-resync.C:
			if n.resync.Load() {
				log.Printf("link: camera changes were dropped; reconnecting to resend the full list")
				conn.Close()
				return
			}
		case <-done:
			return
		}
	}
}

// readNodeKey reads this node's link key (PKCS#8 Ed25519 PEM, owner-only).
func readNodeKey(path string) (ed25519.PrivateKey, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("must be a regular file readable only by its owner")
	}
	return admission.ReadPrivateKey(path)
}

// setupNodeLink checks the node's link flags and builds its link.
func setupNodeLink(srv *Server, root, linkTo, serverName, caPath, keyPath, address, mainAddress, ticketAudience, configPath string) (*nodeLink, error) {
	u, err := url.Parse(linkTo)
	if err != nil || u.Scheme != "wss" || u.Hostname() == "" || u.Path != "/" || u.RawQuery != "" || u.User != nil {
		return nil, errors.New("-link-to must be wss://host:port/")
	}
	if serverName == "" {
		serverName = u.Hostname()
	}
	key, err := readNodeKey(keyPath)
	if err != nil {
		return nil, fmt.Errorf("-node-key: %w", err)
	}
	if address == "" {
		address = mainAddress
	}
	if address == "" {
		address = ticketAudience
	}
	if admission.ValidateAudience(address) != nil {
		return nil, errors.New("-node-address must be an exact wss://host[:port]/ URL")
	}
	if srv.ticketKey == nil {
		return nil, errors.New("-link-to needs Center's public key: run with -ticket-public-key (or -center)")
	}
	tlsConfig := &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	if caPath != "" {
		pem, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("-link-ca: %w", err)
		}
		tlsConfig.RootCAs = x509.NewCertPool()
		if !tlsConfig.RootCAs.AppendCertsFromPEM(pem) {
			return nil, errors.New("-link-ca holds no PEM certificate")
		}
	}
	if configPath == "" {
		configPath = root + "cfg/node-config.json"
	}
	n := newNodeLink(srv, "wss://"+u.Host+link.Path, serverName, address, key, srv.ticketKey, tlsConfig, configPath)
	srv.selfAddress = address
	n.loadSaved()
	log.Printf("Linking to Center %s as node %s", u.Host, address)
	return n, nil
}
