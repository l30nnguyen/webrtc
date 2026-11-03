package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"signaling-server/internal/admission"
	"signaling-server/internal/fleet"
	"signaling-server/internal/stats"
)

// SetStats turns on viewing counters on this node.
func (s *Server) SetStats(counters *stats.Counters) { s.stats = counters }

func (s *Server) countStat(update func(*stats.Counters)) {
	if s.stats != nil {
		update(s.stats)
	}
}

// recordReport counts a {"type":"report"} message: the outcome of one viewing
// attempt as the camera or the viewer app saw it. The role comes from the
// client's ticket, never from the message. Reports are never forwarded.
func (s *Server) recordReport(c *Client, msg map[string]interface{}) {
	if s.stats == nil {
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
	if report.Normalize() {
		s.stats.Record(report)
	}
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
