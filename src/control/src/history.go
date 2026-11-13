package main

import (
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"signaling-server/internal/admission"
	"signaling-server/internal/events"
	"signaling-server/internal/history"
)

// controlSampleInterval is how often the control server samples itself
// (cameras and viewers connected, and its machine's load) into the
// statistics and for the fleet status.
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
	if s.events != nil {
		s.events.Close()
	}
	if s.lastAddresses != nil {
		if err := s.lastAddresses.Save(); err != nil {
			log.Printf("center: saving last_addresses.json: %v", err)
		}
	}
	if s.stats != nil && statsPath != "" {
		if err := s.stats.Save(statsPath); err != nil {
			log.Printf("stats: %v", err)
		}
	}
	log.Print("Stopping")
	os.Exit(0)
}

// sampleControl samples the machine, and with statistics on (-statistic-dir)
// the cameras and viewers connected, every controlSampleInterval.
func (s *Server) sampleControl() {
	for {
		load, measured := s.host.Sample()
		if s.history == nil {
			time.Sleep(controlSampleInterval)
			continue
		}
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
		gauges := map[string]float64{"cameras": float64(cameras), "viewers": float64(viewers)}
		if measured {
			for gauge, v := range load.Gauges() {
				gauges[gauge] = v
			}
		}
		s.history.Sample(history.SourceControl, name, region, gauges)
		time.Sleep(controlSampleInterval)
	}
}

// addEvent records an event in the connection log (-event-dir).
func (s *Server) addEvent(e events.Event) {
	if s.events != nil {
		s.events.Add(e)
	}
}

// refusalLimiter keeps one refusal event per source address and reason a
// minute, so a flood of refused connections can't crowd the log.
type refusalLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (l *refusalLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil || len(l.last) > 10000 {
		l.last = map[string]time.Time{}
	}
	if at, ok := l.last[key]; ok && now.Sub(at) < time.Minute {
		return false
	}
	l.last[key] = now
	return true
}

// refusalEvent records a connection turned away: why, from where, and the
// client ID it asked for.
func (s *Server) refusalEvent(r *http.Request, clientID, reason, detail string) {
	if s.events == nil {
		return
	}
	from := r.RemoteAddr
	if host, _, err := net.SplitHostPort(from); err == nil {
		from = host
	}
	if !s.refusals.allow(from+"\x00"+reason, time.Now()) {
		return
	}
	e := events.Event{Type: events.Refused, Result: reason, From: from, Detail: detail}
	if strings.HasPrefix(clientID, "Player") {
		e.Viewer = clientID
	} else if admission.ValidDeviceID(clientID) {
		e.Camera = clientID
	}
	s.addEvent(e)
}

// admissionReason is a ticket check's error, which never carries ticket
// contents (the checks only say "invalid ticket" and the like).
func admissionReason(err error) string {
	text := err.Error()
	if len(text) > 64 {
		text = text[:64]
	}
	return text
}

// relayNames names the relays handed out for a request: the relay's name
// when this server knows it, else host:port.
func (s *Server) relayNames(servers []map[string]interface{}) []string {
	var names []string
	known := s.relayAddresses()
	for _, server := range servers {
		host, _ := server["host"].(string)
		port, _ := server["port"].(int)
		if f, ok := server["port"].(float64); ok {
			port = int(f)
		}
		name := net.JoinHostPort(host, strconv.Itoa(port))
		for _, r := range known {
			if r.name != "" && r.host == host && r.port == port {
				name = r.name
			}
		}
		names = append(names, name)
	}
	return names
}
