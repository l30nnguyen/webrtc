package relay

import (
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
	"signaling-server/internal/history"
	"signaling-server/internal/host"
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
	// Machine is the relay's machine load (relay 1.2.0 and later).
	Machine *host.Load `json:"machine,omitempty"`
}

// Relay is the configuration part of a report, as the fleet lists relays.
func (r Report) Relay() fleet.Relay {
	return fleet.Relay{Name: r.Name, Region: r.Region, Host: r.Host, Port: r.Port, CapacityMbps: r.CapacityMbps,
		TCPPort: r.TCPPort, TLSPort: r.TLSPort, TLSHost: r.TLSHost}
}

type entry struct {
	Report    Report    `json:"report"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`

	signedAt int64
	lastSig  string
	silent   bool // reported as silent; its next report is "back"
}

// Registry keeps the relays that report to this control server: their
// last report, saved in dir/relays.json so they are known again after a
// restart. Each report also goes to History, the region's statistics.
type Registry struct {
	dir     string
	Now     func() time.Time
	History *history.Store
	// OnChange, when set, hears a relay registered, restarted, stopping,
	// silent (no report for 45 s) or back.
	OnChange func(name, change, detail string)

	mu     sync.Mutex
	relays map[string]*entry
}

// NewRegistry loads the relays saved in dir.
func NewRegistry(dir string) (*Registry, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
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
	detail := fmt.Sprintf("%s %s:%d version %s", report.Region, report.Host, report.Port, report.Version)
	if e == nil {
		e = &entry{FirstSeen: now}
		r.relays[report.Name] = e
		log.Printf("relays: %s registered (%s)", report.Name, detail)
		r.change(report.Name, "registered", detail)
	} else if changed {
		log.Printf("relays: %s changed or restarted (%s)", report.Name, detail)
		r.change(report.Name, "restarted", detail)
	} else if e.silent || e.Report.State == "stopping" && report.State == "up" {
		r.change(report.Name, "back", "")
	}
	e.silent = false
	if report.State == "stopping" && e.Report.State != "stopping" {
		log.Printf("relays: %s is stopping", report.Name)
		r.change(report.Name, "stopping", "")
	}
	r.addHistory(e, report)
	e.Report, e.LastSeen, e.signedAt, e.lastSig = report, now, signedAt, sig
	if changed {
		r.saveLocked()
	}
	return nil
}

// addHistory records a report in the statistics: the clients it serves by
// transport and its bandwidth as samples, its counters as what they grew by
// since the last report (all of them after the relay restarted).
func (r *Registry) addHistory(e *entry, report Report) {
	if r.History == nil {
		return
	}
	gauges := map[string]float64{"clients": float64(report.Allocations), "mbps": report.Mbps}
	for _, transport := range []string{"udp", "tcp", "tls"} {
		gauges["clients_"+transport] = float64(report.ByTransport[transport])
	}
	if report.Machine != nil {
		for gauge, v := range report.Machine.Gauges() {
			gauges[gauge] = v
		}
	}
	r.History.Sample(history.SourceRelay, report.Name, report.Region, gauges)
	// A relay new here or restarted counts from zero.
	before := e.Report
	restarted := before.StartedAt != report.StartedAt
	grew := func(now, then int64) int64 {
		if restarted || now < then {
			return now
		}
		return now - then
	}
	counts := map[string]int64{
		"reports":       1,
		"rx_bytes":      grew(report.RxBytes, before.RxBytes),
		"tx_bytes":      grew(report.TxBytes, before.TxBytes),
		"new_clients":   grew(report.AllocationsTotal, before.AllocationsTotal),
		"auth_failures": grew(report.AuthFailures, before.AuthFailures),
		"quota_rejects": grew(report.QuotaRejects, before.QuotaRejects),
		"peers_denied":  grew(report.PeersDenied, before.PeersDenied),
	}
	r.History.Count(history.SourceRelay, report.Name, report.Region, counts)
}

// Flush forgets relays silent for forgetAfter. Run it every minute.
func (r *Registry) Flush() {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	forgot := false
	for name, e := range r.relays {
		if !e.silent && e.Report.State == "up" && now.Sub(e.LastSeen) > goneAfter {
			e.silent = true
			r.change(name, "silent", "no report since "+e.LastSeen.UTC().Format(time.RFC3339))
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

func (r *Registry) change(name, change, detail string) {
	if r.OnChange != nil {
		r.OnChange(name, change, detail)
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
			Allocations: int(s.Allocations), CapacityMbps: s.CapacityMbps, At: s.LastSeen, Version: s.Version}
		if s.Up {
			status.Machine = s.Machine
		}
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

var warnings sync.Map

func warnOnce(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	if _, seen := warnings.LoadOrStore(message, struct{}{}); !seen {
		log.Print(message)
	}
}
