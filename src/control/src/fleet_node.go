package main

import (
	"log"
	"os"
	"os/signal"
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

// relayProtocols are the ways a viewer may reach the relay stream server.
var relayProtocols = map[string]bool{"udp": true, "tcp": true, "tls": true}

// recordReport counts a {"type":"report"} message: the outcome of one viewing
// attempt as the camera or the viewer app saw it. The role comes from the
// client's ticket, never from the message. Reports are never forwarded.
func (s *Server) recordReport(c *Client, msg map[string]interface{}) {
	if s.stats == nil && s.history == nil && s.events == nil {
		return
	}
	report := stats.Report{Role: stats.RolePlayer}
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
		SetupMillis: report.SetupMillis, RelayOffered: &report.RelayOffered, RelayReady: &report.RelayReady}
	if report.Role == stats.RoleCamera {
		e.Role, e.Camera = "camera", c.ID
	} else {
		e.Viewer, e.Camera = c.ID, c.CameraIDScope
	}
	if protocol, _ := msg["relay_protocol"].(string); report.Path == "relay" && relayProtocols[protocol] {
		e.RelayProtocol = protocol
	}
	s.addEvent(e)
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
