package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"signaling-server/internal/fleet"
	"signaling-server/internal/link"
)

const (
	linkCenter = "wss://center.example.test:443/"
	linkNode   = "wss://eu.example.test:443/"
)

type linkLab struct {
	t         *testing.T
	registry  *fleet.Registry
	hub       *linkHub
	centerKey ed25519.PrivateKey
	nodeKey   ed25519.PrivateKey
	url       string
	tls       *tls.Config
	dir       string
}

func keyFromSeed(seed byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
}

func publicKeyText(key ed25519.PrivateKey) string {
	return base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
}

// nodeAt makes the node's host name resolve to ip.
func nodeAt(ip string) func(context.Context, string) ([]netip.Addr, error) {
	return func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr(ip)}, nil }
}

func newLinkLab(t *testing.T) *linkLab {
	return newLinkLabWith(t, keyFromSeed(1), nodeAt("127.0.0.1"))
}

// newLinkLabWith runs Center with hubKey (the node trusts keyFromSeed(1)) and
// lookup resolving the node's name.
func newLinkLabWith(t *testing.T, hubKey ed25519.PrivateKey, lookup func(context.Context, string) ([]netip.Addr, error)) *linkLab {
	t.Helper()
	l := &linkLab{t: t, centerKey: keyFromSeed(1), nodeKey: keyFromSeed(2), dir: t.TempDir()}
	registry, err := fleet.NewFromConfig(l.config(1, publicKeyText(l.nodeKey)), linkCenter, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	l.registry = registry
	// httptest's certificate is for example.com: that is the Center name.
	l.hub = newLinkHub(registry, hubKey, "example.com")
	l.hub.lookup = lookup
	l.hub.SetConfig(registry.Config())
	l.hub.resolve()
	server := httptest.NewTLSServer(l.hub)
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	l.tls = &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12}
	l.url = "wss://" + server.Listener.Addr().String() + link.Path
	return l
}

func (l *linkLab) config(version int, nodeKey string) fleet.Config {
	return fleet.Config{Version: version, Nodes: []fleet.Node{
		{Name: "center", Address: linkCenter, Region: "US"},
		{Name: "eu", Address: linkNode, Region: "DE", PublicKey: nodeKey},
	}, Routes: map[string]string{"IT": "DE"}}
}

func (l *linkLab) startNode(key ed25519.PrivateKey) (*Server, *nodeLink) {
	srv := NewServer("", l.dir+"/")
	srv.version = "9.9.9"
	srv.host.Sample()
	n := newNodeLink(srv, l.url, "example.com", linkNode, key, l.centerKey.Public().(ed25519.PublicKey), l.tls,
		filepath.Join(l.dir, "node-config.json"))
	srv.nodeLink = n
	go n.run()
	return srv, n
}

func (l *linkLab) apply(config fleet.Config) {
	l.t.Helper()
	if err := l.registry.Apply(config); err != nil {
		l.t.Fatal(err)
	}
	l.hub.SetConfig(config)
}

func (l *linkLab) node() fleet.NodeStatus {
	for _, s := range l.registry.Status() {
		if s.Address == linkNode {
			return s
		}
	}
	return fleet.NodeStatus{}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestNodeLinksPresenceAndConfig(t *testing.T) {
	l := newLinkLab(t)
	_, n := l.startNode(l.nodeKey)

	// The node logs in, gets version 1 and runs it.
	eventually(t, "node up with version 1", func() bool { s := l.node(); return s.Healthy && s.ConfigVersion == 1 })
	if _, version := n.Config(); version != 1 {
		t.Fatalf("node runs version %d", version)
	}
	// Its server version and machine load come with it (on Linux).
	if s := l.node(); s.Version != "9.9.9" || (runtime.GOOS == "linux" && (s.Machine == nil || s.Machine.MemMB <= 0)) {
		t.Fatalf("node status %+v machine %+v", s, s.Machine)
	}

	// Cameras coming and going reach Center at once.
	camera := strings.Repeat("Q", 43)
	n.cameraChanged(camera, true)
	eventually(t, "camera online", func() bool { node, online := l.registry.Locate(camera); return online && node == linkNode })
	n.cameraChanged(camera, false)
	eventually(t, "camera offline", func() bool { _, online := l.registry.Locate(camera); return !online })

	// A new configuration is pushed, acknowledged and saved.
	next := l.config(2, publicKeyText(l.nodeKey))
	next.Routes["FR"] = "DE"
	l.apply(next)
	eventually(t, "version 2 acknowledged", func() bool { return l.node().ConfigVersion == 2 })
	if config, version := n.Config(); version != 2 || config.Routes["FR"] != "DE" {
		t.Fatalf("node config %d %+v", version, config.Routes)
	}
	saved := newNodeLink(nil, "", "", "", nil, l.centerKey.Public().(ed25519.PublicKey), nil, filepath.Join(l.dir, "node-config.json"))
	saved.loadSaved()
	if _, version := saved.Config(); version != 2 {
		t.Fatalf("saved version %d", version)
	}

	// Removing the node from the configuration drops it.
	without := l.config(3, "")
	without.Nodes = without.Nodes[:1]
	without.Routes = nil
	l.apply(without)
	eventually(t, "node removed", func() bool { return l.node().Address == "" })
}

func TestNodeWithAWrongKeyIsRefused(t *testing.T) {
	l := newLinkLab(t)
	_, n := l.startNode(keyFromSeed(9))
	time.Sleep(500 * time.Millisecond)
	if l.node().Healthy {
		t.Fatal("node with a wrong key linked")
	}
	if _, version := n.Config(); version != 0 {
		t.Fatal("node with a wrong key got the configuration")
	}
	if n.connected.Load() {
		t.Fatal("node with a wrong key counts itself connected")
	}
}

func TestNodeRefusesAConfigNotSignedByCenter(t *testing.T) {
	dir := t.TempDir()
	center := keyFromSeed(1)
	n := newNodeLink(nil, "", "", "", nil, center.Public().(ed25519.PublicKey), nil, filepath.Join(dir, "c.json"))
	config := []byte(`{"version":5,"nodes":[]}`)
	if err := n.accept(link.SignConfig(keyFromSeed(3), 5, config), true); err == nil {
		t.Fatal("config signed by another key accepted")
	}
	if err := n.accept(link.SignConfig(center, 6, config), true); err == nil {
		t.Fatal("signed version different from the content accepted")
	}
	if err := n.accept(link.SignConfig(center, 5, config), true); err != nil {
		t.Fatal(err)
	}
	if err := n.accept(link.SignConfig(center, 5, config), true); err == nil {
		t.Fatal("same version accepted twice")
	}
	if _, err := os.Stat(filepath.Join(dir, "c.json")); err != nil {
		t.Fatal("accepted config not saved")
	}
}

func TestNodeHealthNamesItsCenter(t *testing.T) {
	l := newLinkLab(t)
	srv, _ := l.startNode(l.nodeKey)
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"center":"https://example.com"`) {
		t.Fatalf("health %s", rec.Body.String())
	}
}

// upgrade sends a link request with the node's signed headers.
func (l *linkLab) upgrade(key ed25519.PrivateKey, unix int64, nonce []byte) int {
	l.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "https://"+strings.TrimPrefix(l.url, "wss://"), nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set(link.HeaderNode, linkNode)
	req.Header.Set(link.HeaderTime, strconv.FormatInt(unix, 10))
	req.Header.Set(link.HeaderNonce, base64.RawURLEncoding.EncodeToString(nonce))
	req.Header.Set(link.HeaderSig, base64.RawURLEncoding.EncodeToString(
		ed25519.Sign(key, link.HeaderMessage(unix, nonce, "example.com", linkNode))))
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: l.tls}}
	resp, err := client.Do(req)
	if err != nil {
		l.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestLinkFromAnotherIPLooksLikeNothing(t *testing.T) {
	l := newLinkLabWith(t, keyFromSeed(1), nodeAt("192.0.2.7"))
	_, n := l.startNode(l.nodeKey)
	time.Sleep(500 * time.Millisecond)
	if l.node().Healthy || n.connected.Load() {
		t.Fatal("node linked from an IP that is not its own")
	}
	nonce := bytes.Repeat([]byte{4}, link.NonceSize)
	if code := l.upgrade(l.nodeKey, time.Now().Unix(), nonce); code != http.StatusNotFound {
		t.Fatalf("request from another IP got %d", code)
	}
}

func TestLinkFromOverridesDNS(t *testing.T) {
	l := newLinkLabWith(t, keyFromSeed(1), func(context.Context, string) ([]netip.Addr, error) {
		return nil, errors.New("no such host")
	})
	config := l.config(2, publicKeyText(l.nodeKey))
	config.Nodes[1].LinkFrom = []string{"127.0.0.0/8"}
	l.apply(config)
	l.hub.resolve()
	l.startNode(l.nodeKey)
	eventually(t, "node up", func() bool { return l.node().Healthy })
}

func TestLinkHeaderChecks(t *testing.T) {
	l := newLinkLab(t)
	now := time.Now().Unix()
	nonce := func(b byte) []byte { return bytes.Repeat([]byte{b}, link.NonceSize) }
	if code := l.upgrade(l.nodeKey, now, nonce(1)); code != http.StatusSwitchingProtocols {
		t.Fatalf("good request got %d", code)
	}
	time.Sleep(100 * time.Millisecond)
	if code := l.upgrade(l.nodeKey, now, nonce(1)); code != http.StatusNotFound {
		t.Fatalf("repeated request got %d", code)
	}
	if code := l.upgrade(l.nodeKey, now-120, nonce(2)); code != http.StatusNotFound {
		t.Fatalf("old request got %d", code)
	}
	if code := l.upgrade(keyFromSeed(9), now, nonce(3)); code != http.StatusNotFound {
		t.Fatalf("request signed by another key got %d", code)
	}
}

func TestLinkBlocksAnIPAfterFailures(t *testing.T) {
	l := newLinkLab(t)
	for i := 0; i < linkMaxFailures; i++ {
		l.upgrade(keyFromSeed(9), time.Now().Unix(), bytes.Repeat([]byte{byte(10 + i)}, link.NonceSize))
	}
	// Even a correct request is ignored while the IP is blocked.
	if code := l.upgrade(l.nodeKey, time.Now().Unix(), bytes.Repeat([]byte{1}, link.NonceSize)); code != http.StatusNotFound {
		t.Fatalf("blocked IP got %d", code)
	}
}

func TestNodeRefusesAServerWithoutCenterKey(t *testing.T) {
	l := newLinkLabWith(t, keyFromSeed(7), nodeAt("127.0.0.1"))
	_, n := l.startNode(l.nodeKey)
	time.Sleep(500 * time.Millisecond)
	if n.connected.Load() || l.node().Healthy {
		t.Fatal("node linked to a server without Center's key")
	}
}

func TestLinkPathIsNotFoundOnANode(t *testing.T) {
	srv := NewServer("", t.TempDir()+"/")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, link.Path, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("link path on a node got %d", rec.Code)
	}
}
