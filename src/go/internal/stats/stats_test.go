package stats

import (
	"path/filepath"
	"testing"
	"time"
)

func TestReportsAreCountedPerRoleAndDay(t *testing.T) {
	now := time.Date(2026, 10, 26, 23, 59, 0, 0, time.UTC)
	c := New(func() time.Time { return now })
	c.Request()
	c.Request()
	c.CameraOffline()
	for _, r := range []Report{
		{Role: RoleCamera, Result: ResultConnected, Path: "relay", RelayOffered: true, RelayReady: true, SetupMillis: 900},
		{Role: RoleCamera, Result: ResultFailed, Error: "no_path", RelayOffered: true},
		{Role: RolePlayer, Result: ResultFailed, Error: "weird-code"},
		{Role: RolePlayer, Result: ResultConnected, Path: "lan", SetupMillis: 300, RelayReady: true},
	} {
		if !r.Normalize() {
			t.Fatalf("valid report refused: %+v", r)
		}
		c.Record(r)
	}
	day := c.Snapshot()["2026-10-26"]
	if day.Requests != 2 || day.CameraOffline != 1 {
		t.Fatalf("server counters = %+v", day)
	}
	if day.Camera.Connected != 1 || day.Camera.Failed != 1 || day.Camera.Paths["relay"] != 1 || day.Camera.Errors["no_path"] != 1 ||
		day.Camera.RelayOffered != 2 || day.Camera.RelayReady != 1 || day.Camera.SetupMillisSum != 900 {
		t.Fatalf("camera = %+v", day.Camera)
	}
	// unknown error becomes "other"; relay ready without an offered relay is dropped
	if day.Player.Errors["other"] != 1 || day.Player.Paths["lan"] != 1 || day.Player.RelayReady != 0 {
		t.Fatalf("player = %+v", day.Player)
	}

	now = now.Add(2 * time.Minute) // next UTC day
	c.Request()
	if got := c.Snapshot()["2026-10-27"].Requests; got != 1 {
		t.Fatalf("next day requests = %d", got)
	}
	days, total := Range(c.Snapshot(), 1, now)
	if len(days) != 1 || total.Requests != 1 {
		t.Fatalf("last day = %v %+v", days, total)
	}
	if _, total = Range(c.Snapshot(), 0, now); total.Requests != 3 {
		t.Fatalf("all days total = %+v", total)
	}
}

func TestBadReportsAreRefused(t *testing.T) {
	for _, r := range []Report{{Role: "admin", Result: ResultConnected}, {Role: RoleCamera, Result: "maybe"}} {
		if r.Normalize() {
			t.Fatalf("accepted %+v", r)
		}
	}
}

func TestSaveLoadAndPrune(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	c := New(func() time.Time { return now })
	for i := 0; i < KeepDays+5; i++ {
		c.Request()
		now = now.AddDate(0, 0, 1)
	}
	if n := len(c.Snapshot()); n != KeepDays {
		t.Fatalf("kept %d days", n)
	}
	path := filepath.Join(t.TempDir(), "stats.json")
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded := New(func() time.Time { return now })
	if err := loaded.Load(path); err != nil {
		t.Fatal(err)
	}
	if _, total := Range(loaded.Snapshot(), 0, now); total.Requests != KeepDays {
		t.Fatalf("loaded total = %d", total.Requests)
	}
	if err := New(nil).Load(filepath.Join(t.TempDir(), "missing.json")); err != nil {
		t.Fatalf("missing file: %v", err)
	}
}
