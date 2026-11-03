package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func playerTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	playerPath := filepath.Join(dir, "ws.html")
	if err := os.WriteFile(playerPath, []byte("<title>player</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	return NewServer(playerPath, dir+"/")
}

func playerStatus(s *Server, path, remote string) int {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec.Code
}

func TestDemoPathServesPlayerOnlyForDiscoveryTenant(t *testing.T) {
	s := playerTestServer(t)
	if code := playerStatus(s, "/demo/test01", "203.0.113.5:4000"); code == http.StatusOK {
		t.Fatalf("demo path served without a discovery tenant")
	}
	s.SetDiscoveryTenant("test01")
	if code := playerStatus(s, "/demo/test01", "203.0.113.5:4000"); code != http.StatusOK {
		t.Fatalf("/demo/test01 = %d, want 200", code)
	}
	if code := playerStatus(s, "/demo/other01", "203.0.113.5:4000"); code == http.StatusOK {
		t.Fatalf("demo path of another tenant served the player")
	}
}

func TestPlayerAllowLimitsPlayerAndDeviceList(t *testing.T) {
	s := playerTestServer(t)
	s.SetDiscoveryTenant("test01")
	allow, err := parsePlayerAllow("115.78.12.186, 10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	s.SetPlayerAllow(allow)

	for _, remote := range []string{"115.78.12.186:5000", "10.1.2.3:5000", "[::ffff:115.78.12.186]:5000"} {
		if code := playerStatus(s, "/demo/test01", remote); code != http.StatusOK {
			t.Fatalf("allowed %s got %d for the player", remote, code)
		}
	}
	for _, path := range []string{"/demo/test01", "/api/devices"} {
		if code := playerStatus(s, path, "198.51.100.7:5000"); code != http.StatusNotFound {
			t.Fatalf("other client got %d for %s, want 404", code, path)
		}
	}
	if code := playerStatus(s, "/api/health", "198.51.100.7:5000"); code != http.StatusOK {
		t.Fatalf("health check must stay public, got %d", code)
	}
}

func TestRootAnswersOKInsteadOfThePlayer(t *testing.T) {
	s := playerTestServer(t)
	s.SetDiscoveryTenant("test01")
	allow, _ := parsePlayerAllow("115.78.12.186")
	s.SetPlayerAllow(allow)
	for _, remote := range []string{"115.78.12.186:5000", "198.51.100.7:5000"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != "OK\n" || rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Fatalf("/ from %s = %d %q %q", remote, rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
		}
	}
}

func TestParsePlayerAllowRejectsGarbage(t *testing.T) {
	if _, err := parsePlayerAllow("115.78.12.186,not-an-ip"); err == nil {
		t.Fatal("invalid entry accepted")
	}
	if prefixes, err := parsePlayerAllow(""); err != nil || len(prefixes) != 0 {
		t.Fatalf("empty list = %v, %v", prefixes, err)
	}
}
