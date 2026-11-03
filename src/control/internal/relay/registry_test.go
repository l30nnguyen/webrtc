package relay

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

var testKey = []byte("relay-shared-secret-0123456789")

func secretFor(name string) ([]byte, bool) {
	if name == "relay-sg01" {
		return testKey, true
	}
	return nil, false
}

// signed builds a report request as src/relay/report.go signs it.
func signed(t *testing.T, report Report, key []byte, at time.Time) *http.Request {
	t.Helper()
	body, _ := json.Marshal(report)
	stamp := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signedPrefix + stamp + "\n"))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, ReportPath, bytes.NewReader(body))
	req.Header.Set(SignatureHeader, "t="+stamp+",sig="+base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
	return req
}

func sampleReport() Report {
	return Report{Name: "relay-sg01", Region: "SG", Host: "152.42.0.10", Port: 3478, TCPPort: 80, TLSPort: 443,
		TLSHost: "relay-sg01.example.test", CapacityMbps: 500, Version: "1.1.0", StartedAt: "2026-11-03T08:00:00Z",
		State: "up", ByTransport: map[string]int64{"udp": 2, "tcp": 0, "tls": 1}}
}

func post(r *Registry, req *http.Request) int {
	w := httptest.NewRecorder()
	r.Handle(w, req, secretFor)
	return w.Code
}

func TestRegistryTakesSignedReports(t *testing.T) {
	now := time.Date(2026, 11, 3, 8, 0, 10, 0, time.UTC)
	r, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r.Now = func() time.Time { return now }

	report := sampleReport()
	if code := post(r, signed(t, report, testKey, now)); code != http.StatusNoContent {
		t.Fatalf("report: %d", code)
	}
	if relays := r.Relays(); len(relays) != 1 || relays[0].TLSHost != "relay-sg01.example.test" || relays[0].CapacityMbps != 500 {
		t.Fatalf("relays %+v", relays)
	}
	for name, req := range map[string]*http.Request{
		"wrong key":    signed(t, report, []byte("another-secret-0123456789"), now),
		"replay":       signed(t, report, testKey, now),
		"clock off":    signed(t, report, testKey, now.Add(-2*time.Minute)),
		"unknown name": signed(t, func() Report { x := report; x.Name = "relay-xx"; return x }(), testKey, now),
		"unsigned":     httptest.NewRequest(http.MethodPost, ReportPath, bytes.NewReader([]byte(`{"name":"relay-sg01"}`))),
	} {
		if code := post(r, req); code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, code)
		}
	}
	bad := report
	bad.TLSHost = ""
	bad.Host = "152.42.0.10"
	now = now.Add(time.Second)
	if code := post(r, signed(t, bad, testKey, now)); code != http.StatusBadRequest {
		t.Errorf("TLS without a host name: %d, want 400", code)
	}

	// Load comes from the report.
	report.Mbps, report.Allocations = 250, 3
	now = now.Add(15 * time.Second)
	post(r, signed(t, report, testKey, now))
	if fraction, healthy, known := r.Load("relay-sg01"); !known || !healthy || fraction != 0.5 {
		t.Fatalf("load %v %v %v", fraction, healthy, known)
	}
	// Silent for three intervals: not handed out, still listed.
	now = now.Add(goneAfter + time.Second)
	if len(r.Relays()) != 0 {
		t.Fatal("silent relay handed out")
	}
	if s := r.Statuses(); len(s) != 1 || s[0].Up || s[0].Silent == "" {
		t.Fatalf("statuses %+v", s)
	}
	// Back, then stopping: gone at once.
	post(r, signed(t, report, testKey, now))
	report.State = "stopping"
	now = now.Add(time.Second)
	post(r, signed(t, report, testKey, now))
	if len(r.Relays()) != 0 {
		t.Fatal("stopping relay handed out")
	}
}

func TestRegistryKeepsRelaysAndMinutes(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 3, 8, 0, 5, 0, time.UTC)
	r, _ := NewRegistry(dir)
	r.Now = func() time.Time { return now }
	report := sampleReport()
	for i, counters := range []struct{ rx, tx, total, alloc int64 }{{0, 0, 0, 0}, {1e6, 2e6, 4, 2}, {3e6, 5e6, 6, 3}} {
		report.RxBytes, report.TxBytes, report.AllocationsTotal, report.Allocations = counters.rx, counters.tx, counters.total, counters.alloc
		report.Mbps = float64(i)
		post(r, signed(t, report, testKey, now))
		now = now.Add(15 * time.Second)
	}
	// The relay restarts: its counters start over.
	report.StartedAt, report.RxBytes, report.TxBytes, report.AllocationsTotal = "2026-11-03T08:01:00Z", 1e6, 1e6, 1
	now = time.Date(2026, 11, 3, 8, 1, 10, 0, time.UTC)
	post(r, signed(t, report, testKey, now))
	now = now.Add(time.Minute)
	r.Flush()

	minutes := r.Minutes(now.Add(-time.Hour))
	if len(minutes) != 2 {
		t.Fatalf("minutes %+v", minutes)
	}
	first, second := minutes[0], minutes[1]
	if first.Reports != 3 || first.TxBytes != 5e6 || first.NewAllocations != 6 || first.AllocationsMax != 3 || first.MbpsMax != 2 || first.MbpsAvg != 1 {
		t.Fatalf("first minute %+v", first)
	}
	if second.TxBytes != 1e6 || second.NewAllocations != 1 {
		t.Fatalf("after the restart %+v", second)
	}
	if _, err := os.Stat(filepath.Join(dir, "stats", "2026-11-03.jsonl")); err != nil {
		t.Fatal(err)
	}
	// Known again after the control server restarts, but not handed out
	// until it reports again.
	again, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	again.Now = r.Now
	if known := again.Known(); len(known) != 1 || known[0].Name != "relay-sg01" {
		t.Fatalf("reloaded %+v", known)
	}
	if len(again.Relays()) != 0 {
		t.Fatal("a relay from the saved file is handed out before it reports")
	}
}

func TestStatsArePruned(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewRegistry(dir)
	old := filepath.Join(dir, "stats", "2026-01-01.jsonl")
	os.WriteFile(old, []byte("{}\n"), 0o600)
	r.Now = func() time.Time { return time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC) }
	post(r, signed(t, sampleReport(), testKey, r.Now()))
	r.Now = func() time.Time { return time.Date(2026, 11, 3, 8, 1, 0, 0, time.UTC) }
	r.Flush()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("statistics older than StatsDays kept")
	}
}
