package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"signaling-server/internal/fleet"
	"signaling-server/internal/history"
	"signaling-server/internal/host"
	"signaling-server/internal/relay"
)

func relayServer(t *testing.T, secrets string) *Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "cfg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cfg", "turn.conf"), []byte(`[{"host":"old.example.test","port":3478,"username":"u","password":"p"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if secrets != "" {
		if err := os.WriteFile(filepath.Join(dir, relaySecretsFile), []byte(secrets), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := fleet.NewFromConfig(fleet.Config{Version: 1,
		Nodes: []fleet.Node{{Address: cliSelf, Region: "DE"}},
		Relays: []fleet.Relay{
			{Name: "turn-de", Region: "DE", Host: "turn-de.example.test"},
			{Name: "turn-us", Region: "US", Host: "turn-us.example.test", Port: 3479},
		}}, cliSelf, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer("", dir+"/")
	srv.fleet, srv.selfAddress = registry, cliSelf
	return srv
}

func TestRelaysComeFromTheConfigWithExpiringCredentials(t *testing.T) {
	srv := relayServer(t, `{"turn-de":"de-secret-0123456789","turn-us":"us-secret-0123456789"}`)
	servers := srv.relaysFor("camera1")
	if len(servers) != 2 || servers[0]["host"] != "turn-de.example.test" || servers[1]["host"] != "turn-us.example.test" ||
		servers[0]["port"] != fleet.DefaultRelayPort || servers[1]["port"] != 3479 {
		t.Fatalf("servers %v", servers)
	}
	user := servers[0]["username"].(string)
	expiry, id, _ := strings.Cut(user, ":")
	at, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil || id != "camera1" || time.Until(time.Unix(at, 0)) < 23*time.Hour {
		t.Fatalf("user %q", user)
	}
	mac := hmac.New(sha1.New, []byte("de-secret-0123456789"))
	mac.Write([]byte(user))
	if servers[0]["password"] != base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
		t.Fatal("password not minted with the relay's secret")
	}
}

func TestRelaysWithoutSecretsFallBackToTurnConf(t *testing.T) {
	for name, secrets := range map[string]string{
		"no secrets file": "",
		"no matching key": `{"other":"0123456789abcdef"}`,
		"short secret":    `{"turn-de":"short"}`,
	} {
		servers := relayServer(t, secrets).relaysFor("camera1")
		if len(servers) != 1 || servers[0]["host"] != "old.example.test" {
			t.Errorf("%s: %v", name, servers)
		}
	}
	// Only the relays with a secret are handed out.
	servers := relayServer(t, `{"turn-us":"us-secret-0123456789"}`).relaysFor("camera1")
	if len(servers) != 1 || servers[0]["host"] != "turn-us.example.test" {
		t.Fatalf("partial secrets: %v", servers)
	}
}

func TestRelaysWithFixedCredentials(t *testing.T) {
	srv := relayServer(t, `{"turn-de":{"username":"matrix","password":"violet-pass"}}`)
	servers := srv.relaysFor("camera1")
	if len(servers) != 1 || servers[0]["username"] != "matrix" || servers[0]["password"] != "violet-pass" {
		t.Fatalf("servers %v", servers)
	}
	if got := relayServer(t, `{"turn-de":{"username":"matrix"}}`).relaysFor("camera1"); got[0]["host"] != "old.example.test" {
		t.Fatalf("incomplete fixed credentials accepted: %v", got)
	}
}

func offerURLs(t *testing.T, srv *Server, candidates interface{}) []interface{} {
	t.Helper()
	msg := map[string]interface{}{"type": "offer", "candidates": candidates}
	srv.addRelayURLs(msg)
	list, ok := msg["candidates"].([]interface{})
	if !ok {
		list = []interface{}{msg["candidates"]}
	}
	urls, _ := list[0].(map[string]interface{})["urls"].([]interface{})
	return urls
}

func TestOfferGetsRelayURLsInPreferenceOrder(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "cfg"), 0o755)
	registry, err := fleet.NewFromConfig(fleet.Config{Version: 1,
		Nodes: []fleet.Node{{Address: cliSelf, Region: "US"}},
		Relays: []fleet.Relay{
			{Name: "turn-us", Region: "US", Host: "3.95.228.203", Port: 3478, TCPPort: 80, TLSPort: 443, TLSHost: "relay.example.test"},
			{Name: "turn-sg", Region: "SG", Host: "relay-sg.example.test"},
		}}, cliSelf, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer("", dir+"/")
	srv.fleet, srv.selfAddress = registry, cliSelf

	urls := offerURLs(t, srv, []interface{}{map[string]interface{}{"url": "3.95.228.203:3478?transport=udp", "username": "u", "password": "p"}})
	want := []interface{}{"turn:3.95.228.203:3478?transport=udp", "turn:3.95.228.203:80?transport=tcp", "turns:relay.example.test:443?transport=tcp"}
	if fmt.Sprint(urls) != fmt.Sprint(want) {
		t.Fatalf("urls %v, want %v", urls, want)
	}
	// A relay without TCP or TLS gets its UDP URL only.
	if urls := offerURLs(t, srv, []interface{}{map[string]interface{}{"url": "relay-sg.example.test:3478?transport=udp"}}); fmt.Sprint(urls) != "[turn:relay-sg.example.test:3478?transport=udp]" {
		t.Fatalf("udp-only relay: %v", urls)
	}
	// URLs never come from the camera, and an unknown relay gets none.
	if urls := offerURLs(t, srv, map[string]interface{}{"url": "elsewhere.test:3478?transport=udp", "urls": []interface{}{"turn:evil.test"}}); urls != nil {
		t.Fatalf("unknown relay: %v", urls)
	}
	// A port this node measured down is left out.
	srv.relayMonitor = &relay.Monitor{}
	silent, _ := net.Listen("tcp", "127.0.0.1:0")
	silent.Close()
	port := silent.Addr().(*net.TCPAddr).Port
	srv.relayMonitor.Measure(context.Background(), []fleet.Relay{{Name: "turn-us", Host: "127.0.0.1", Port: port, TCPPort: port, TLSPort: port, TLSHost: "relay.example.test"}})
	urls = offerURLs(t, srv, []interface{}{map[string]interface{}{"url": "3.95.228.203:3478?transport=udp"}})
	if fmt.Sprint(urls) != "[turn:3.95.228.203:3478?transport=udp]" {
		t.Fatalf("down ports handed out: %v", urls)
	}
}

func TestTurnConfViewerKeysStayOnTheServer(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "cfg"), 0o755)
	os.WriteFile(filepath.Join(dir, "cfg", "turn.conf"), []byte(`[{"host":"relay.example.test","port":3478,"username":"u","password":"p","tcp_port":3478,"tls_port":5349}]`), 0o600)
	srv := NewServer("", dir+"/")
	servers := srv.relaysFor("camera1")
	if len(servers) != 1 || len(servers[0]) != 4 || servers[0]["host"] != "relay.example.test" {
		t.Fatalf("camera got %v", servers)
	}
	urls := offerURLs(t, srv, []interface{}{map[string]interface{}{"url": "relay.example.test:3478?transport=udp"}})
	want := "[turn:relay.example.test:3478?transport=udp turn:relay.example.test:3478?transport=tcp turns:relay.example.test:5349?transport=tcp]"
	if fmt.Sprint(urls) != want {
		t.Fatalf("urls %v", urls)
	}
}

func TestReportingRelayIsHandedOutWithItsURLs(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "cfg"), 0o755)
	os.WriteFile(filepath.Join(dir, "cfg", "turn.conf"), []byte(`[{"host":"old.example.test","port":3478,"username":"u","password":"p"}]`), 0o600)
	os.WriteFile(filepath.Join(dir, relaySecretsFile), []byte(`{"relay-dev":"dev-shared-secret-0123456789"}`), 0o600)
	srv := NewServer("", dir+"/")
	registry, err := relay.NewRegistry(filepath.Join(dir, "relays"))
	if err != nil {
		t.Fatal(err)
	}
	srv.relayRegistry = registry

	// Before any report: cfg/turn.conf as before.
	if servers := srv.relaysFor("camera1"); servers[0]["host"] != "old.example.test" {
		t.Fatalf("before reports: %v", servers)
	}
	report := relay.Report{Name: "relay-dev", Region: "US", Host: "3.95.228.203", Port: 3478, TCPPort: 3478, TLSPort: 5349,
		TLSHost: "webrtc.5gen.care", CapacityMbps: 1000, Version: "1.2.0", StartedAt: "2026-11-03T08:00:00Z", State: "up",
		Machine: &host.Load{CPU: 37, Mem: 41, MemMB: 961, Cores: 1}}
	body, _ := json.Marshal(report)
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte("dev-shared-secret-0123456789"))
	mac.Write([]byte("matrix-relay-report\n" + stamp + "\n"))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, relay.ReportPath, bytes.NewReader(body))
	req.Header.Set(relay.SignatureHeader, "t="+stamp+",sig="+base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
	req.RemoteAddr = "3.95.228.203:50000"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("report: %d %s", w.Code, w.Body)
	}

	servers := srv.relaysFor("camera1")
	if len(servers) != 1 || servers[0]["host"] != "3.95.228.203" || servers[0]["port"] != 3478 || !strings.HasSuffix(servers[0]["username"].(string), ":camera1") {
		t.Fatalf("servers %v", servers)
	}
	urls := offerURLs(t, srv, []interface{}{map[string]interface{}{"url": "3.95.228.203:3478?transport=udp"}})
	want := "[turn:3.95.228.203:3478?transport=udp turn:3.95.228.203:3478?transport=tcp turns:webrtc.5gen.care:5349?transport=tcp]"
	if fmt.Sprint(urls) != want {
		t.Fatalf("urls %v", urls)
	}
	if s := srv.relayStatuses(); len(s) != 1 || s[0].Name != "relay-dev" || !s[0].Healthy ||
		s[0].Version != "1.2.0" || s[0].Machine == nil || s[0].Machine.CPU != 37 {
		t.Fatalf("statuses %+v", s)
	}
}

func TestStatisticPageShowsTheMachine(t *testing.T) {
	dir := t.TempDir()
	srv := NewServer("", dir+"/")
	store, err := history.Open(filepath.Join(dir, "statistic"), "30m")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	srv.SetHistory(store)
	srv.statisticAllow, _ = parsePlayerAllow("115.78.12.186")
	page := func() string {
		req := httptest.NewRequest(http.MethodGet, statisticPath, nil)
		req.RemoteAddr = "115.78.12.186:1000"
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w.Body.String()
	}
	store.Sample(history.SourceControl, "sg00", "SG", map[string]float64{"cameras": 3, "viewers": 1})
	store.Flush()
	if strings.Contains(page(), "Machine %") {
		t.Fatal("machine chart without measurements")
	}
	store.Sample(history.SourceControl, "sg00", "SG", (host.Load{CPU: 55, Mem: 63, Disk: 20, Load1: 0.7, RSSMB: 80}).Gauges())
	store.Flush()
	if body := page(); !strings.Contains(body, "Machine %") || !strings.Contains(body, "55 / 63 / 20") {
		t.Fatalf("no machine on the page:\n%s", body)
	}
}

func TestStatisticPageOnlyForAllowedAddresses(t *testing.T) {
	dir := t.TempDir()
	srv := NewServer("", dir+"/")
	registry, _ := relay.NewRegistry(filepath.Join(dir, "relays"))
	srv.relayRegistry = registry
	get := func(remote, query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, statisticPath+query, nil)
		req.RemoteAddr = remote
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w
	}
	if w := get("115.78.12.186:1000", ""); w.Code != http.StatusNotFound {
		t.Fatalf("without -statistic-allow: %d", w.Code)
	}
	allow, _ := parsePlayerAllow("115.78.12.186")
	srv.statisticAllow = allow
	if w := get("203.0.113.9:1000", ""); w.Code != http.StatusNotFound {
		t.Fatalf("other address: %d", w.Code)
	}
	if w := get("115.78.12.186:1000", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Relay stream servers") || !strings.Contains(w.Body.String(), "Statistics are off") {
		t.Fatalf("allowed address: %d", w.Code)
	}
	srv.statisticAllow = nil
	data := func(remote string) int {
		req := httptest.NewRequest(http.MethodGet, statisticDataPath, nil)
		req.RemoteAddr = remote
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w.Code
	}
	if code := data("115.78.12.186:1000"); code != http.StatusNotFound {
		t.Fatalf("data without -statistic-allow: %d", code)
	}
}
