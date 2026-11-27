package main

import (
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"signaling-server/internal/events"
)

// maxStepMillis bounds a reported step time; a larger value is "not measured".
const maxStepMillis = 600000

// Setup timing seen by the server (ST-5). A viewer connects, sends its
// request, gets the camera's offer, sends its answer: the "answered" event
// holds the three gaps.

func (c *Client) requestForwarded(now time.Time) {
	c.timingMu.Lock()
	c.requestAt, c.offerAt = now, time.Time{}
	c.timingMu.Unlock()
}

func (c *Client) offerForwarded(now time.Time) {
	c.timingMu.Lock()
	if !c.requestAt.IsZero() && c.offerAt.IsZero() {
		c.offerAt = now
	}
	c.timingMu.Unlock()
}

// answerForwarded returns the request's steps once, at its first answer.
func (c *Client) answerForwarded(now time.Time) map[string]int {
	c.timingMu.Lock()
	defer c.timingMu.Unlock()
	if c.requestAt.IsZero() || c.offerAt.IsZero() {
		return nil
	}
	steps := map[string]int{
		"offer_ms":  int(c.offerAt.Sub(c.requestAt).Milliseconds()),
		"answer_ms": int(now.Sub(c.offerAt).Milliseconds()),
	}
	if !c.connectedAt.IsZero() && !c.connectedAt.After(c.requestAt) {
		steps["connect_ms"] = int(c.requestAt.Sub(c.connectedAt).Milliseconds())
	}
	c.requestAt, c.offerAt = time.Time{}, time.Time{}
	for k, v := range steps {
		if v < 0 || v > maxStepMillis {
			delete(steps, k)
		}
	}
	return steps
}

// reportSteps takes the named step times from a camera message; values that
// are missing, negative or too large are left out.
func reportSteps(msg map[string]interface{}, keys ...string) map[string]int {
	var steps map[string]int
	for _, key := range keys {
		v, ok := msg[key].(float64)
		if !ok || v != v || v < 0 || v > maxStepMillis {
			continue
		}
		if steps == nil {
			steps = map[string]int{}
		}
		steps[key] = int(v)
	}
	return steps
}

// stopClients is the first part of a stop (R4): refuse new connections, then
// tell every connected client the server is restarting (WebSocket close
// 1012), so cameras 2.2.5+ come back after a short random delay instead of
// their failure backoff, and wait briefly for the clients to leave.
func (s *Server) stopClients(wait time.Duration) {
	s.stopping.Store(true)
	s.mu.RLock()
	clients := make([]*Client, 0, len(s.clients))
	for _, c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.RUnlock()
	if len(clients) == 0 {
		return
	}
	frame := websocket.FormatCloseMessage(websocket.CloseServiceRestart, "restarting")
	deadline := time.Now().Add(wait)
	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			_ = c.Conn.WriteControl(websocket.CloseMessage, frame, deadline)
		}(c)
	}
	wg.Wait()
	// Clients answer the close and their read pumps remove them.
	for time.Now().Before(deadline) {
		s.mu.RLock()
		left := len(s.clients)
		s.mu.RUnlock()
		if left == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.mu.RLock()
	left := len(s.clients)
	s.mu.RUnlock()
	log.Printf("Stopping: told %d clients to reconnect, %d still connected", len(clients), left)
}

// setupStep is one row of the setup time table on /admin/statistic.
type setupStep struct {
	Label, Who    string
	N             int
	P50, P90, P95 string
}

type setupTable struct {
	Steps  []setupStep
	ByPath []setupStep
}

// setupTiming sums the step times in the connection log from from to to.
func (s *Server) setupTiming(from, to time.Time) setupTable {
	values := map[string][]int{}
	add := func(key string, v int) { values[key] = append(values[key], v) }
	s.events.Scan(from, to, func(e events.Event) {
		switch {
		case e.Type == events.Request && e.Result == "answered":
			for k, v := range e.Steps {
				add("server_"+k, v)
			}
		case e.Type == events.Report && e.Role == "camera" && e.Result == "connected":
			if e.SetupMillis > 0 {
				add("camera_setup_ms", e.SetupMillis)
				if e.Path != "" {
					add("path_"+e.Path, e.SetupMillis)
				}
			}
			for k, v := range e.Steps {
				add("camera_"+k, v)
			}
		case e.Type == events.Report && e.Role == "camera":
			for k, v := range e.Steps {
				add("camera_"+k, v)
			}
		}
	})
	row := func(label, who, key string) setupStep {
		list := values[key]
		r := setupStep{Label: label, Who: who, N: len(list), P50: "-", P90: "-", P95: "-"}
		if len(list) == 0 {
			return r
		}
		sort.Ints(list)
		pick := func(p float64) string {
			i := int(float64(len(list)) * p)
			if i >= len(list) {
				i = len(list) - 1
			}
			return seconds(list[i])
		}
		r.P50, r.P90, r.P95 = pick(0.5), pick(0.9), pick(0.95)
		return r
	}
	return setupTable{
		Steps: []setupStep{
			row("Viewer connected → request sent", "server", "server_connect_ms"),
			row("Password check", "camera", "camera_auth_ms"),
			row("Request → offer sent (includes the relay address)", "camera", "camera_offer_ms"),
			row("Request → offer passed to the viewer", "server", "server_offer_ms"),
			row("Offer → viewer's answer", "server", "server_answer_ms"),
			row("Request → answer received", "camera", "camera_answer_ms"),
			row("Request → connected", "camera", "camera_setup_ms"),
			row("Request → data channel open", "camera", "camera_channel_ms"),
			row("Request → first video sent", "camera", "camera_video_ms"),
		},
		ByPath: []setupStep{
			row("LAN", "camera", "path_lan"),
			row("Direct", "camera", "path_direct"),
			row("Relay", "camera", "path_relay"),
		},
	}
}

func seconds(ms int) string {
	return fmt.Sprintf("%.1f s", float64(ms)/1000)
}
