package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func newLinkLab(t *testing.T) *linkLab {
	t.Helper()
	l := &linkLab{t: t, centerKey: keyFromSeed(1), nodeKey: keyFromSeed(2), dir: t.TempDir()}
	registry, err := fleet.NewFromConfig(l.config(1, publicKeyText(l.nodeKey)), linkCenter, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	l.registry = registry
	// httptest's certificate is for example.com: that is the Center name.
	l.hub = newLinkHub(registry, l.centerKey, "example.com")
	l.hub.SetConfig(registry.Config())
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
