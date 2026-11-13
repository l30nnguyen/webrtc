package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"signaling-server/internal/admission"
	"signaling-server/internal/events"
	"signaling-server/internal/fleet"
	"signaling-server/internal/stats"
)

// SetStats turns on viewing counters on this node.
func (s *Server) SetStats(counters *stats.Counters) { s.stats = counters }

// countStat counts an event in the daily viewing counters and, as name, in
// the region's statistics.
func (s *Server) countStat(name string, update func(*stats.Counters)) {
	if s.stats != nil {
		update(s.stats)
	}
	s.countHistory(map[string]int64{name: 1})
}

// reportVersion is the software version a report may carry, e.g. 2.6.6.
var reportVersion = regexp.MustCompile(`^[0-9A-Za-z._+-]{1,32}$`)

// relayProtocols are the ways a viewer may reach the relay stream server.
var relayProtocols = map[string]bool{"udp": true, "tcp": true, "tls": true}

// recordReport counts a {"type":"report"} message: the outcome of one viewing
// attempt as the camera or the viewer app saw it. The role comes from the
// client's ticket, never from the message. Reports are never forwarded.
func (s *Server) recordReport(c *Client, msg map[string]interface{}) {
	if s.stats == nil && s.history == nil && s.events == nil {
		return
	}
	report := stats.Report{Role: stats.RolePlayer, Country: c.Country}
	if c.Role == admission.RoleCamera {
		report.Role = stats.RoleCamera
	}
	report.Result, _ = msg["result"].(string)
	report.Path, _ = msg["path"].(string)
	report.Error, _ = msg["error"].(string)
	report.RelayOffered, _ = msg["relay_offered"].(bool)
	report.RelayReady, _ = msg["relay_ready"].(bool)
	if ms, ok := msg["setup_ms"].(float64); ok {
		report.SetupMillis = int(ms)
	}
	if !report.Normalize() {
		return
	}
	if s.stats != nil {
		s.stats.Record(report)
	}
	// In the statistics: camera_connected, viewer_path_relay,
	// viewer_relay_tls, viewer_error_no_path, ...
	prefix := "viewer_"
	if report.Role == stats.RoleCamera {
		prefix = "camera_"
	}
	counts := map[string]int64{prefix + report.Result: 1}
	switch report.Result {
	case stats.ResultConnected:
		counts[prefix+"path_"+report.Path] = 1
		if protocol, _ := msg["relay_protocol"].(string); report.Path == "relay" && relayProtocols[protocol] {
			counts[prefix+"relay_"+protocol] = 1
		}
	case stats.ResultFailed:
		counts[prefix+"error_"+report.Error] = 1
	}
	s.countHistory(counts)

	// In the connection log, with the camera it was about: the reporting
	// camera itself, or the camera the viewer's ticket was for.
	e := events.Event{Type: events.Report, Role: "viewer", Result: report.Result, Error: report.Error, Path: report.Path,
		SetupMillis: report.SetupMillis, RelayOffered: &report.RelayOffered, RelayReady: &report.RelayReady, Country: c.Country}
	if report.Role == stats.RoleCamera {
		e.Role, e.Camera = "camera", c.ID
	} else {
		e.Viewer, e.Camera = c.ID, c.CameraIDScope
	}
	if protocol, _ := msg["relay_protocol"].(string); report.Path == "relay" && relayProtocols[protocol] {
		e.RelayProtocol = protocol
	}
	if version, _ := msg["version"].(string); reportVersion.MatchString(version) {
		e.Version = version
	}
	s.addEvent(e)
}

// qualityLimits bound each number of a quality message; a value outside
// (or missing) is "not measured".
var qualityLimits = map[string]float64{
	"duration_s": 7 * 24 * 3600, "video_kbps": 100000, "loss_pct": 100, "max_loss_pct": 100,
	"jitter_ms": 60000, "rtt_ms": 60000, "remb_kbps": 1000000, "pli": 1e7, "nack": 1e9,
}

// recordQuality handles a camera's {"type":"quality"} message (ST-3): its view
// of one viewing session's stream from the viewer's RTCP feedback, sent about
// 10 s after the session connects (stage "start") and when it closes ("end").
// Both go to the connection log; "end" is counted in the statistics. Only
// cameras send it, and it is never forwarded.
func (s *Server) recordQuality(c *Client, msg map[string]interface{}) {
	if c.Role != admission.RoleCamera {
		return
	}
	stage, _ := msg["stage"].(string)
	if stage != "start" && stage != "end" {
		return
	}
	value := func(key string) float64 {
		v, ok := msg[key].(float64)
		if !ok || v < 0 || v > qualityLimits[key] || v != v {
			return -1
		}
		return v
	}
	count := func(key string) int {
		if v := value(key); v > 0 {
			return int(v)
		}
		return 0
	}
	viewerID, _ := msg["id"].(string)
	if !admission.ValidDeviceID(viewerID) {
		viewerID = ""
	}
	q := stats.QualityReport{
		DurationS: value("duration_s"), VideoKbps: value("video_kbps"), LossPct: value("loss_pct"),
		MaxLossPct: value("max_loss_pct"), JitterMs: value("jitter_ms"), RTTMs: value("rtt_ms"),
		REMBKbps: value("remb_kbps"), PLI: count("pli"), NACK: count("nack"),
	}
	// The viewer's country: remembered from its request (the viewer has
	// often left by the "end" report), else from the connected viewer.
	if viewerID != "" {
		key := clientKey(c.Tenant, viewerID)
		q.Country = s.viewerCountries.lookup(key, stage == "end")
		if q.Country == "" {
			s.mu.RLock()
			if v, ok := s.clients[key]; ok && v.Role == admission.RolePlayer {
				q.Country = v.Country
			}
			s.mu.RUnlock()
		}
	}
	if stage == "end" {
		if s.stats != nil {
			s.stats.RecordQuality(q)
		}
		counts := map[string]int64{"quality_sessions": 1}
		if q.Poor() {
			counts["quality_poor"] = 1
		}
		if q.Streamed() {
			counts["quality_streamed"] = 1
		}
		s.countHistory(counts)
	}
	result := "quality " + stage
	if q.Poor() {
		result += " poor"
	}
	s.addEvent(events.Event{Type: events.Report, Role: "camera", Camera: c.ID, Viewer: viewerID,
		Result: result, Detail: qualityText(q), Country: q.Country})
}

// qualityText is a quality report for the connection log.
func qualityText(q stats.QualityReport) string {
	parts := []string{}
	add := func(v float64, format string) {
		if v >= 0 {
			parts = append(parts, fmt.Sprintf(format, v))
		}
	}
	add(q.DurationS, "%.0f s")
	add(q.VideoKbps, "%.0f kbps")
	add(q.LossPct, "loss %.1f%%")
	add(q.MaxLossPct, "max %.1f%%")
	add(q.RTTMs, "rtt %.0f ms")
	add(q.JitterMs, "jitter %.0f ms")
	add(q.REMBKbps, "estimate %.0f kbps")
	parts = append(parts, fmt.Sprintf("pli %d", q.PLI), fmt.Sprintf("nack %d", q.NACK))
	return strings.Join(parts, ", ")
}

// LocalReport is this node's state as it reports it to Center: the cameras
// connected to it, its client count and its counters.
func (s *Server) LocalReport(address string) fleet.Report {
	report := fleet.Report{Address: address, Cameras: []string{}}
	s.mu.RLock()
	for _, c := range s.clients {
		if c.Role == admission.RoleCamera && admission.ValidDeviceID(c.ID) {
			report.Cameras = append(report.Cameras, c.ID)
		}
	}
	report.Clients = len(s.clients)
	s.mu.RUnlock()
	if s.stats != nil {
		report.Stats = s.stats.Snapshot()
	}
	report.Machine, report.Version = s.host.Last(), s.version
	return report
}

// CameraConnected reports whether a camera with this device ID is connected.
func (s *Server) CameraConnected(deviceID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.clients {
		if c.ID == deviceID && c.Role == admission.RoleCamera {
			return true
		}
	}
	return false
}

// reloadFleetOnHangup re-reads the fleet configuration on every SIGHUP
// ("systemctl reload matrix", run by matrix-fleet apply). A configuration that
// does not load or validate is logged and the running one is kept.
func (s *Server) reloadFleetOnHangup(path string) {
	hangup := make(chan os.Signal, 1)
	signal.Notify(hangup, syscall.SIGHUP)
	for range hangup {
		if err := s.reloadFleet(path); err != nil {
			log.Printf("fleet: configuration not applied, keeping version %d: %v", s.fleet.Version(), err)
			continue
		}
		log.Printf("fleet: configuration version %d applied", s.fleet.Version())
	}
}

func (s *Server) reloadFleet(path string) error {
	config, err := fleet.LoadConfig(path)
	if err != nil {
		return err
	}
	if err := s.fleet.Apply(config); err != nil {
		return err
	}
	if s.hub != nil {
		s.hub.SetConfig(config)
	}
	return nil
}

// saveStatsLoop writes the counters to path every minute.
func (s *Server) saveStatsLoop(path string) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		if err := s.stats.Save(path); err != nil {
			log.Printf("stats: cannot save %s: %v", path, err)
		}
	}
}

// viewerCountryCache maps a viewer (tenant/id) to its country for a day at
// most, bounded so a flood of requests can't grow it without limit.
type viewerCountryCache struct {
	mu      sync.Mutex
	entries map[string]viewerCountry
}

type viewerCountry struct {
	country string
	at      time.Time
}

const (
	viewerCountryTTL = 24 * time.Hour
	viewerCountryMax = 100000
)

func (v *viewerCountryCache) remember(key, country string) {
	if country == "" {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.entries == nil {
		v.entries = map[string]viewerCountry{}
	}
	now := time.Now()
	if len(v.entries) >= viewerCountryMax {
		for k, e := range v.entries {
			if now.Sub(e.at) > viewerCountryTTL {
				delete(v.entries, k)
			}
		}
		if len(v.entries) >= viewerCountryMax {
			return
		}
	}
	v.entries[key] = viewerCountry{country: country, at: now}
}

// lookup returns the remembered country ("" when none or expired); forget
// drops the entry (the session ended).
func (v *viewerCountryCache) lookup(key string, forget bool) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, ok := v.entries[key]
	if !ok || time.Since(e.at) > viewerCountryTTL {
		delete(v.entries, key)
		return ""
	}
	if forget {
		delete(v.entries, key)
	}
	return e.country
}
