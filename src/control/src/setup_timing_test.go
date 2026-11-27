package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"signaling-server/internal/admission"
	"signaling-server/internal/events"

	"github.com/gorilla/websocket"
)

func withEvents(t *testing.T) func(*Server) {
	return func(s *Server) {
		log, err := events.Open(filepath.Join(t.TempDir(), "events"), 30)
		if err != nil {
			t.Fatal(err)
		}
		s.events = log
	}
}

func findEvents(s *Server, result string) []events.Event {
	found, _ := s.events.Find(events.Query{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour), Result: result})
	return found
}

// The server logs its own view of one setup (request -> offer -> answer)
// and keeps the camera's step times from its reports (ST-5).
func TestSetupStepsAreLogged(t *testing.T) {
	n := newNode(t, withEvents(t))
	camera := n.connect("camera0", "acme01", admission.RoleCamera)
	viewerID := strings.Repeat("v", 43)
	viewer := n.connectScopedViewer(viewerID, "acme01", "camera0")

	send(t, viewer, map[string]any{"type": "request", "id": "camera0", "password": "pw"})
	if got := receive(camera, time.Second); got == nil || got["type"] != "request" {
		t.Fatalf("request not forwarded: %v", got)
	}
	send(t, camera, map[string]any{"type": "offer", "id": viewerID, "sdp": "v=0"})
	if got := receive(viewer, time.Second); got == nil || got["type"] != "offer" {
		t.Fatalf("offer not forwarded: %v", got)
	}
	send(t, viewer, map[string]any{"type": "answer", "id": "camera0", "sdp": "v=0"})
	if got := receive(camera, time.Second); got == nil || got["type"] != "answer" {
		t.Fatalf("answer not forwarded: %v", got)
	}
	// A second answer to the same request is not a second setup.
	send(t, viewer, map[string]any{"type": "answer", "id": "camera0", "sdp": "v=0"})
	send(t, camera, map[string]any{"type": "report", "result": "connected", "path": "direct", "setup_ms": 2100,
		"auth_ms": 40, "offer_ms": 900, "answer_ms": 1500, "bogus_ms": 5, "video_ms": -1})
	send(t, camera, map[string]any{"type": "quality", "id": viewerID, "stage": "start", "duration_s": 10,
		"channel_ms": 2300, "video_ms": 3900})
	waitFor(t, "the quality report", func() bool { return len(findEvents(n.srv, "quality start")) == 1 })

	answered := findEvents(n.srv, "answered")
	if len(answered) != 1 {
		t.Fatalf("answered events = %d", len(answered))
	}
	for _, key := range []string{"connect_ms", "offer_ms", "answer_ms"} {
		if _, ok := answered[0].Steps[key]; !ok {
			t.Fatalf("answered steps = %v", answered[0].Steps)
		}
	}
	connected := findEvents(n.srv, "connected")
	if len(connected) != 1 || len(connected[0].Steps) != 3 || connected[0].Steps["offer_ms"] != 900 {
		t.Fatalf("connected steps = %+v", connected)
	}
	quality := findEvents(n.srv, "quality start")
	if quality[0].Steps["video_ms"] != 3900 || quality[0].Steps["channel_ms"] != 2300 {
		t.Fatalf("quality steps = %v", quality[0].Steps)
	}

	table := n.srv.setupTiming(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	byLabel := map[string]setupStep{}
	for _, row := range append(table.Steps, table.ByPath...) {
		byLabel[row.Label] = row
	}
	if r := byLabel["Request → first video sent"]; r.N != 1 || r.P50 != "3.9 s" {
		t.Fatalf("first video row = %+v", r)
	}
	if r := byLabel["Direct"]; r.N != 1 || r.P90 != "2.1 s" {
		t.Fatalf("direct row = %+v", r)
	}
	if r := byLabel["Offer → viewer's answer"]; r.N != 1 {
		t.Fatalf("answer row = %+v", r)
	}
}

func TestSetupTablePercentiles(t *testing.T) {
	srv := NewServer("", t.TempDir()+"/")
	withEvents(t)(srv)
	for i := 1; i <= 100; i++ {
		srv.events.Add(events.Event{Type: events.Report, Role: "camera", Result: "connected", Path: "relay", SetupMillis: i * 100})
	}
	table := srv.setupTiming(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	r := table.ByPath[2]
	if r.N != 100 || r.P50 != "5.1 s" || r.P90 != "9.1 s" || r.P95 != "9.6 s" {
		t.Fatalf("relay row = %+v", r)
	}
	if table.ByPath[0].P50 != "-" {
		t.Fatalf("an empty row shows a dash: %+v", table.ByPath[0])
	}
}

// On stop the server tells every client it is restarting (close 1012) and
// refuses new connections until it exits (R4).
func TestStopTellsClientsToReconnect(t *testing.T) {
	n := newNode(t, nil)
	camera := n.connect("camera0", "acme01", admission.RoleCamera)
	n.waitRegistered("acme01", "camera0")

	done := make(chan struct{})
	go func() { n.srv.stopClients(2 * time.Second); close(done) }()
	_ = camera.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := camera.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseServiceRestart {
		t.Fatalf("camera read = %v, want close 1012", err)
	}
	camera.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stopClients did not return")
	}

	if _, response := n.dial("/camera1?ticket=" + n.ticket("camera1", "acme01", admission.RoleCamera)); response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("new connection while stopping: %d", response.StatusCode)
	}
	w := httptest.NewRecorder()
	n.srv.handleHealth(w, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"stopping"`) {
		t.Fatalf("health while stopping: %d %s", w.Code, w.Body.String())
	}
}
