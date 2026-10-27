package main

import (
	"strings"
	"testing"
	"time"

	"signaling-server/internal/admission"
	"signaling-server/internal/stats"
)

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestViewingAttemptsAndReportsAreCounted(t *testing.T) {
	n := newNode(t, func(s *Server) { s.SetStats(stats.New(nil)) })
	today := func() stats.Day { return n.srv.stats.Snapshot()[time.Now().UTC().Format(time.DateOnly)] }
	camera := n.connect("camera0", "acme01", admission.RoleCamera)
	viewerID := strings.Repeat("v", 43)
	viewer := n.connectScopedViewer(viewerID, "acme01", "camera0")

	send(t, viewer, map[string]any{"type": "request", "id": "camera0", "password": "pw"})
	if request := receive(camera, time.Second); request == nil || request["type"] != "request" {
		t.Fatalf("request not forwarded: %v", request)
	}
	send(t, camera, map[string]any{"type": "report", "id": viewerID, "result": "connected", "path": "relay",
		"relay_offered": true, "relay_ready": true, "setup_ms": 1200})
	send(t, viewer, map[string]any{"type": "report", "id": "camera0", "result": "failed", "error": "no_path", "relay_offered": true})
	waitFor(t, "both reports", func() bool { d := today(); return d.Camera.Connected == 1 && d.Player.Failed == 1 })

	// Reports stay on the server: neither side receives the other's report.
	if got := receive(viewer, 200*time.Millisecond); got != nil {
		t.Fatalf("camera report was forwarded: %v", got)
	}
	if got := receive(camera, 200*time.Millisecond); got != nil {
		t.Fatalf("viewer report was forwarded: %v", got)
	}
	day := today()
	if day.Requests != 1 || day.Camera.Paths["relay"] != 1 || day.Camera.RelayReady != 1 || day.Camera.SetupMillisSum != 1200 ||
		day.Player.Errors["no_path"] != 1 || day.Player.RelayOffered != 1 {
		t.Fatalf("counters = %+v", day)
	}

	// A request for a camera that is not connected counts as camera offline.
	other := n.connectScopedViewer(strings.Repeat("w", 43), "acme01", "camera9")
	send(t, other, map[string]any{"type": "request", "id": "camera9", "password": "pw"})
	waitFor(t, "camera offline", func() bool { return today().CameraOffline == 1 })

	// A missing ticket counts as an admission rejection.
	n.dial("/" + strings.Repeat("x", 43))
	waitFor(t, "admission rejection", func() bool { return today().AdmissionRejected == 1 })
}

func TestLocalReportListsCamerasOnly(t *testing.T) {
	n := newNode(t, func(s *Server) { s.SetStats(stats.New(nil)) })
	cameraID := strings.Repeat("c", 43)
	n.connect(cameraID, "acme01", admission.RoleCamera)
	n.connectScopedViewer(strings.Repeat("v", 43), "acme01", cameraID)
	report := n.srv.LocalReport("wss://node.example.test/")
	if report.Address != "wss://node.example.test/" || len(report.Cameras) != 1 || report.Cameras[0] != cameraID || report.Clients != 2 {
		t.Fatalf("report = %+v", report)
	}
}
