package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"signaling-server/internal/admission"
	"signaling-server/internal/events"
)

func TestReportVersionIsLogged(t *testing.T) {
	log, err := events.Open(filepath.Join(t.TempDir(), "events"), 30)
	if err != nil {
		t.Fatal(err)
	}
	n := newNode(t, func(s *Server) { s.events = log })
	camera := n.connect("camera0", "acme01", admission.RoleCamera)
	viewerID := strings.Repeat("v", 43)
	viewer := n.connectScopedViewer(viewerID, "acme01", "camera0")

	send(t, viewer, map[string]any{"type": "report", "id": "camera0", "result": "failed", "error": "timeout", "version": "2.6.6"})
	send(t, camera, map[string]any{"type": "report", "id": viewerID, "result": "failed", "error": "no_answer", "version": "<b>x</b>"})
	var found []events.Event
	waitFor(t, "both reports", func() bool {
		found, _ = log.Find(events.Query{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour), Type: events.Report})
		return len(found) == 2
	})
	for _, e := range found {
		want := map[string]string{"viewer": "2.6.6", "camera": ""}[e.Role]
		if e.Version != want {
			t.Fatalf("%s report version %q, want %q", e.Role, e.Version, want)
		}
	}
}

func TestPlayerIsRevalidatedOnEveryLoad(t *testing.T) {
	s := playerTestServer(t)
	s.SetDiscoveryTenant("test01")
	req := httptest.NewRequest(http.MethodGet, "/demo/test01", nil)
	req.RemoteAddr = "203.0.113.5:4000"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("player: %d, Cache-Control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

func TestPlayerVersionMatchesRelease(t *testing.T) {
	version, err := os.ReadFile("../../../cfg/version")
	if err != nil {
		t.Fatal(err)
	}
	page, err := os.ReadFile("../../../player/ws.html")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`const PLAYER_VERSION = '([^']+)'`).FindSubmatch(page)
	if m == nil || string(m[1]) != strings.TrimSpace(string(version)) {
		t.Fatalf("ws.html PLAYER_VERSION %q, cfg/version %q: bump both", m, strings.TrimSpace(string(version)))
	}
}
