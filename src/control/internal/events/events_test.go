package events

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEventsAreFoundAndPruned(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	l, err := Open(dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	l.Now = func() time.Time { return now }
	l.Add(Event{Type: Request, Camera: "cam1", Viewer: "Player1", Result: "forwarded", Relays: []string{"relay-sg01"}})
	now = now.Add(2 * time.Second)
	l.Add(Event{Type: Report, Role: "viewer", Camera: "cam1", Viewer: "Player1", Result: "failed", Error: "no_path"})
	now = now.Add(24 * time.Hour)
	l.Add(Event{Type: Report, Role: "camera", Camera: "cam2", Result: "connected", Path: "relay", RelayProtocol: "tls"})

	all, more := l.Find(Query{From: now.Add(-48 * time.Hour), To: now.Add(time.Minute)})
	if more || len(all) != 3 || all[0].Camera != "cam2" || all[2].Type != Request {
		t.Fatalf("all, newest first: %+v", all)
	}
	failed, _ := l.Find(Query{From: now.Add(-48 * time.Hour), To: now.Add(time.Minute), Camera: "cam1", Result: "failed"})
	if len(failed) != 1 || failed[0].Error != "no_path" || failed[0].T != "2026-11-03T08:00:02.000Z" {
		t.Fatalf("failed for cam1: %+v", failed)
	}
	byRelay, _ := l.Find(Query{From: now.Add(-48 * time.Hour), To: now.Add(time.Minute), Relay: "relay-sg01"})
	if len(byRelay) != 1 {
		t.Fatalf("by relay: %+v", byRelay)
	}
	limited, more := l.Find(Query{From: now.Add(-48 * time.Hour), To: now.Add(time.Minute), Limit: 2})
	if len(limited) != 2 || !more {
		t.Fatalf("limit: %d %v", len(limited), more)
	}

	// Ten days on, the first day's file is removed.
	now = now.Add(10 * 24 * time.Hour)
	l.Add(Event{Type: Camera, Camera: "cam1", Result: "online"})
	if _, err := os.Stat(filepath.Join(dir, "2026-11-03.jsonl")); !os.IsNotExist(err) {
		t.Fatal("old events kept")
	}
}

func TestADayIsCapped(t *testing.T) {
	l, _ := Open(t.TempDir(), 30)
	l.MaxDayBytes = 300
	for i := 0; i < 10; i++ {
		l.Add(Event{Type: Refused, Result: "bad_ticket", From: "203.0.113.9"})
	}
	found, _ := l.Find(Query{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour)})
	if len(found) == 0 || len(found) >= 10 || l.dropped == 0 {
		t.Fatalf("kept %d, dropped %d", len(found), l.dropped)
	}
}
