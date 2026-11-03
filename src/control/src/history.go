package main

import (
	"log"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"signaling-server/internal/admission"
	"signaling-server/internal/history"
)

// controlSampleInterval is how often the control server samples itself
// into the statistics (cameras and viewers connected).
const controlSampleInterval = 15 * time.Second

// SetHistory turns on the region's statistics (-statistic-dir).
func (s *Server) SetHistory(store *history.Store) { s.history = store }

// historyIdentity names this control server in the statistics: its node
// name and region in the fleet configuration, else its host name.
func (s *Server) historyIdentity() (name, region string) {
	if config, ok := s.fleetConfig(); ok {
		for _, n := range config.Nodes {
			if n.Address == s.selfAddress {
				name, region = n.Name, n.Region
			}
		}
	}
	if name == "" {
		if u, err := url.Parse(s.selfAddress); err == nil && u.Hostname() != "" {
			name = u.Hostname()
		} else {
			name = "control"
		}
	}
	return name, region
}

// countHistory adds to this control server's counters of the minute.
func (s *Server) countHistory(counts map[string]int64) {
	if s.history == nil {
		return
	}
	name, region := s.historyIdentity()
	s.history.Count(history.SourceControl, name, region, counts)
}

// saveOnStop writes the open minute of statistics and the viewing counters
// (statsPath, when set) when systemd stops the server, then exits.
func (s *Server) saveOnStop(statsPath string) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop
	if s.history != nil {
		s.history.Close()
	}
	if s.stats != nil && statsPath != "" {
		if err := s.stats.Save(statsPath); err != nil {
			log.Printf("stats: %v", err)
		}
	}
	log.Print("Stopping")
	os.Exit(0)
}

// sampleControl samples the cameras and viewers connected, every
// controlSampleInterval.
func (s *Server) sampleControl() {
	for {
		cameras, viewers := 0, 0
		s.mu.RLock()
		for _, c := range s.clients {
			if c.Role == admission.RoleCamera {
				cameras++
			} else {
				viewers++
			}
		}
		s.mu.RUnlock()
		name, region := s.historyIdentity()
		s.history.Sample(history.SourceControl, name, region, map[string]float64{
			"cameras": float64(cameras), "viewers": float64(viewers),
		})
		time.Sleep(controlSampleInterval)
	}
}
