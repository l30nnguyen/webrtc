package relay

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"signaling-server/internal/fleet"
)

const (
	// ReportPath is where relays POST their reports.
	ReportPath = "/v1/relay/report"
	// SignatureHeader carries "t=<unix seconds>,sig=<base64url HMAC-SHA256>"
	// of signedPrefix + t + "\n" + body, keyed with the relay's shared secret
	// (its entry in relay-secrets.json).
	SignatureHeader = "X-Relay-Signature"
	signedPrefix    = "matrix-relay-report\n"
	// ReportInterval is how often relays report; one silent for goneAfter
	// is not handed out.
	ReportInterval = 15 * time.Second
	goneAfter      = 3 * ReportInterval
	maxReportBytes = 16 << 10
	maxClockSkew   = time.Minute
	// StatsDays is how long the per-minute statistics are kept.
	StatsDays = 90
	// forgetAfter drops a relay that stopped reporting from the registry.
	forgetAfter = 30 * 24 * time.Hour
)

// Report is what a relay sends (src/relay/report.go): who it is, where
// cameras and viewers reach it, and its load. Keep the two in step.
type Report struct {
	Name         string `json:"name"`
	Region       string `json:"region"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	TCPPort      int    `json:"tcp_port,omitempty"`
	TLSPort      int    `json:"tls_port,omitempty"`
	TLSHost      string `json:"tls_host,omitempty"`
	CapacityMbps int    `json:"capacity_mbps"`
	Version      string `json:"version"`
	StartedAt    string `json:"started_at"`
	State        string `json:"state"` // "up" or "stopping"

	Allocations      int64            `json:"allocations"`
	ByTransport      map[string]int64 `json:"by_transport"`
	Mbps             float64          `json:"mbps"`
	RxBytes          int64            `json:"rx_bytes"`
	TxBytes          int64            `json:"tx_bytes"`
	AllocationsTotal int64            `json:"allocations_total"`
	AuthFailures     int64            `json:"auth_failures"`
	QuotaRejects     int64            `json:"quota_rejects"`
	PeersDenied      int64            `json:"peers_denied"`
}

// Relay is the configuration part of a report, as the fleet lists relays.
func (r Report) Relay() fleet.Relay {
	return fleet.Relay{Name: r.Name, Region: r.Region, Host: r.Host, Port: r.Port, CapacityMbps: r.CapacityMbps,
		TCPPort: r.TCPPort, TLSPort: r.TLSPort, TLSHost: r.TLSHost}
}

// Minute is one relay's statistics over one minute.
type Minute struct {
	Minute         string           `json:"minute"` // RFC 3339 UTC, the minute's start
	Relay          string           `json:"relay"`
	Reports        int              `json:"reports"`
	State          string           `json:"state"`
	AllocationsMax int64            `json:"allocations_max"`
	AllocationsAvg float64          `json:"allocations_avg"`
	TransportsMax  map[string]int64 `json:"transports_max,omitempty"`
	MbpsMax        float64          `json:"mbps_max"`
	MbpsAvg        float64          `json:"mbps_avg"`
	RxBytes        int64            `json:"rx_bytes"` // relayed during the minute
	TxBytes        int64            `json:"tx_bytes"`
	NewAllocations int64            `json:"new_allocations"`
	AuthFailures   int64            `json:"auth_failures"`
}

type entry struct {
	Report    Report    `json:"report"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`

	signedAt int64
	lastSig  string
	minute   *Minute
	sums     struct{ allocations, mbps float64 }
}

// Registry keeps the relays that report to this control server: their
// last report (saved in dir/relays.json, so they are known again after a
// restart) and per-minute statistics (dir/stats/YYYY-MM-DD.jsonl).
type Registry struct {
	dir string
	Now func() time.Time

	mu     sync.Mutex
	relays map[string]*entry
}

// NewRegistry loads the relays saved in dir.
func NewRegistry(dir string) (*Registry, error) {
	if err := os.MkdirAll(filepath.Join(dir, "stats"), 0o700); err != nil {
		return nil, err
	}
	r := &Registry{dir: dir, relays: map[string]*entry{}}
	data, err := os.ReadFile(filepath.Join(dir, "relays.json"))
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &r.relays); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, "relays.json"), err)
	}
	return r, nil
}

func (r *Registry) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Handle takes one relay report. secret returns the shared secret of a
// relay this control server may hand out; a report from any other name, or
// not signed with that secret, is refused.
func (r *Registry) Handle(w http.ResponseWriter, req *http.Request, secret func(name string) ([]byte, bool)) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxReportBytes+1))
	if err != nil || len(body) > maxReportBytes {
		http.Error(w, "report too large", http.StatusRequestEntityTooLarge)
		return
	}
	var report Report
	if err := json.Unmarshal(body, &report); err != nil {
		http.Error(w, "report is not JSON", http.StatusBadRequest)
		return
	}
	key, ok := secret(report.Name)
	if !ok {
		warnOnce("relays: report from %q refused: no shared secret for it in relay-secrets.json", report.Name)
		http.Error(w, "unknown relay", http.StatusForbidden)
		return
	}
	signedAt, sig, err := r.verify(req.Header.Get(SignatureHeader), key, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if err := fleet.ValidRelayIdentity(report.Relay()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if report.State != "up" && report.State != "stopping" {
		http.Error(w, "state must be up or stopping", http.StatusBadRequest)
		return
	}
	if err := r.record(report, signedAt, sig); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (r *Registry) verify(header string, key, body []byte) (int64, string, error) {
	var t, sig string
	for _, part := range strings.Split(header, ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch name {
		case "t":
			t = value
		case "sig":
			sig = value
		}
	}
	at, err := strconv.ParseInt(t, 10, 64)
	if err != nil || sig == "" {
		return 0, "", errors.New("missing or malformed " + SignatureHeader)
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return 0, "", errors.New("malformed signature")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signedPrefix + t + "\n"))
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return 0, "", errors.New("bad signature")
	}
	if skew := r.now().Sub(time.Unix(at, 0)); skew > maxClockSkew || skew < -maxClockSkew {
		return 0, "", errors.New("report time is more than a minute off: check the relay's clock")
	}
	return at, sig, nil
}

func (r *Registry) record(report Report, signedAt int64, sig string) error {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.relays[report.Name]
	if e != nil && (signedAt < e.signedAt || sig == e.lastSig) {
		return errors.New("replayed report")
	}
	changed := e == nil || e.Report.Relay() != report.Relay() || e.Report.Version != report.Version ||
		e.Report.StartedAt != report.StartedAt
	if e == nil {
		e = &entry{FirstSeen: now}
		r.relays[report.Name] = e
		log.Printf("relays: %s registered (%s, %s:%d)", report.Name, report.Region, report.Host, report.Port)
	} else if changed {
		log.Printf("relays: %s changed or restarted (%s, %s:%d, version %s)", report.Name, report.Region, report.Host, report.Port, report.Version)
	}
	if report.State == "stopping" && e.Report.State != "stopping" {
		log.Printf("relays: %s is stopping", report.Name)
	}
	r.addMinute(e, report, now)
	e.Report, e.LastSeen, e.signedAt, e.lastSig = report, now, signedAt, sig
	if changed {
		r.saveLocked()
	}
	return nil
}

// addMinute adds a report to its relay's current minute, writing out the
// previous minute when this one starts a new one.
func (r *Registry) addMinute(e *entry, report Report, now time.Time) {
	start := now.UTC().Truncate(time.Minute).Format(time.RFC3339)
	if e.minute != nil && e.minute.Minute != start {
		r.writeMinute(e)
	}
	if e.minute == nil {
		e.minute = &Minute{Minute: start, Relay: report.Name, TransportsMax: map[string]int64{}}
		e.sums.allocations, e.sums.mbps = 0, 0
	}
	m := e.minute
	m.Reports++
	m.State = report.State
	m.AllocationsMax = max(m.AllocationsMax, report.Allocations)
	m.MbpsMax = max(m.MbpsMax, report.Mbps)
	e.sums.allocations += float64(report.Allocations)
	e.sums.mbps += report.Mbps
	m.AllocationsAvg = e.sums.allocations / float64(m.Reports)
	m.MbpsAvg = e.sums.mbps / float64(m.Reports)
	for transport, n := range report.ByTransport {
		m.TransportsMax[transport] = max(m.TransportsMax[transport], n)
	}
	// Counters only grow while a relay runs; after a restart they start over.
	delta := func(now, before int64) int64 {
		if e.Report.StartedAt != report.StartedAt || now < before {
			return now
		}
		return now - before
	}
	if e.Report.Name != "" {
		m.RxBytes += delta(report.RxBytes, e.Report.RxBytes)
		m.TxBytes += delta(report.TxBytes, e.Report.TxBytes)
		m.NewAllocations += delta(report.AllocationsTotal, e.Report.AllocationsTotal)
		m.AuthFailures += delta(report.AuthFailures, e.Report.AuthFailures)
	}
}

func (r *Registry) writeMinute(e *entry) {
	m := e.minute
	e.minute = nil
	line, err := json.Marshal(m)
	if err != nil {
		return
	}
	day := m.Minute[:10]
	path := filepath.Join(r.dir, "stats", day+".jsonl")
	_, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		warnOnce("relays: statistics not saved: %v", err)
		return
	}
	defer f.Close()
	f.Write(append(line, '\n'))
	if errors.Is(statErr, os.ErrNotExist) {
		r.pruneStats(day)
	}
}

// pruneStats removes the statistics files older than StatsDays.
func (r *Registry) pruneStats(today string) {
	day, err := time.Parse("2006-01-02", today)
	if err != nil {
		return
	}
	oldest := day.AddDate(0, 0, -StatsDays).Format("2006-01-02")
	files, _ := filepath.Glob(filepath.Join(r.dir, "stats", "*.jsonl"))
	for _, file := range files {
		if strings.TrimSuffix(filepath.Base(file), ".jsonl") < oldest {
			os.Remove(file)
		}
	}
}

// Flush writes out every minute that has ended, also for relays that went
// silent, and forgets relays silent for forgetAfter. Run it every minute.
func (r *Registry) Flush() {
	now := r.now()
	start := now.UTC().Truncate(time.Minute).Format(time.RFC3339)
	r.mu.Lock()
	defer r.mu.Unlock()
	forgot := false
	for name, e := range r.relays {
		if e.minute != nil && e.minute.Minute != start {
			r.writeMinute(e)
		}
		if now.Sub(e.LastSeen) > forgetAfter {
			delete(r.relays, name)
			forgot = true
		}
	}
	if forgot {
		r.saveLocked()
	}
}

func (r *Registry) saveLocked() {
	data, err := json.MarshalIndent(r.relays, "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(r.dir, "relays.json")
	if err := os.WriteFile(path+".tmp", data, 0o600); err == nil {
		err = os.Rename(path+".tmp", path)
	}
	if err != nil {
		warnOnce("relays: %s not saved: %v", path, err)
	}
}

// up reports whether a relay reported recently and isn't stopping.
func (r *Registry) up(e *entry, now time.Time) bool {
	return now.Sub(e.LastSeen) <= goneAfter && e.Report.State == "up"
}

// Relays are the relays that may be handed out now: reporting, not stopping.
func (r *Registry) Relays() []fleet.Relay {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []fleet.Relay
	for _, e := range r.relays {
		if r.up(e, now) {
			out = append(out, e.Report.Relay())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Known are all registered relays, reporting or not.
func (r *Registry) Known() []fleet.Relay {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]fleet.Relay, 0, len(r.relays))
	for _, e := range r.relays {
		out = append(out, e.Report.Relay())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Load is a registered relay's reported load in the form Choose takes.
func (r *Registry) Load(name string) (fraction float64, healthy, known bool) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.relays[name]
	if !ok {
		return 0, false, false
	}
	if e.Report.CapacityMbps > 0 {
		fraction = e.Report.Mbps / float64(e.Report.CapacityMbps)
	}
	return fraction, r.up(e, now), true
}

// Registered is a registered relay as the statistics page and the fleet see it.
type Registered struct {
	Report
	Up        bool   `json:"up"`
	FirstSeen string `json:"first_seen"`
	LastSeen  string `json:"last_seen"`
	Silent    string `json:"silent,omitempty"` // how long without a report, when gone
}

// Statuses are all registered relays, by name.
func (r *Registry) Statuses() []Registered {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Registered, 0, len(r.relays))
	for _, e := range r.relays {
		s := Registered{Report: e.Report, Up: r.up(e, now),
			FirstSeen: e.FirstSeen.UTC().Format(time.RFC3339), LastSeen: e.LastSeen.UTC().Format(time.RFC3339)}
		if !s.Up && e.Report.State == "up" {
			s.Silent = now.Sub(e.LastSeen).Round(time.Second).String()
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// FleetStatuses are the registered relays as relay measurements, for the
// fleet status and the link report to Center.
func (r *Registry) FleetStatuses() []fleet.RelayStatus {
	var out []fleet.RelayStatus
	for _, s := range r.Statuses() {
		status := fleet.RelayStatus{Name: s.Name, Region: s.Region, Healthy: s.Up, Mbps: s.Mbps, Measured: true,
			Allocations: int(s.Allocations), CapacityMbps: s.CapacityMbps, At: s.LastSeen}
		switch {
		case s.State == "stopping":
			status.Error = "stopped"
		case !s.Up:
			status.Error = "no report for " + s.Silent
		}
		out = append(out, status)
	}
	return out
}

// Minutes returns the saved and current per-minute statistics since a
// time, oldest first.
func (r *Registry) Minutes(since time.Time) []Minute {
	var out []Minute
	since = since.UTC()
	for day := since.Truncate(24 * time.Hour); !day.After(r.now().UTC()); day = day.AddDate(0, 0, 1) {
		f, err := os.Open(filepath.Join(r.dir, "stats", day.Format("2006-01-02")+".jsonl"))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			var m Minute
			if json.Unmarshal(bytes.TrimSpace(scanner.Bytes()), &m) == nil && m.Minute >= since.Format(time.RFC3339) {
				out = append(out, m)
			}
		}
		f.Close()
	}
	r.mu.Lock()
	for _, e := range r.relays {
		if e.minute != nil && e.minute.Minute >= since.Format(time.RFC3339) {
			out = append(out, *e.minute)
		}
	}
	r.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Minute < out[j].Minute })
	return out
}

var warnings sync.Map

func warnOnce(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	if _, seen := warnings.LoadOrStore(message, struct{}{}); !seen {
		log.Print(message)
	}
}
