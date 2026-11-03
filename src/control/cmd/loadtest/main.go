// Command loadtest loads a control server for the capacity test
// (tools/capacity): idle cameras holding their connection, or full viewing
// sessions (a new TLS connection, request, 3 KB offer and answer, reports).
// Run it against a test server only; it needs one without ticket admission.
package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

var dialer = &websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, HandshakeTimeout: 15 * time.Second}
var sdp = strings.Repeat("a", 3000)

func dial(base, id string) (*websocket.Conn, error) {
	c, _, err := dialer.Dial(base+"/"+id, http.Header{})
	return c, err
}

// serveCamera answers each request with a 3 KB offer and a report, like a camera.
func serveCamera(c *websocket.Conn) {
	c.SetPingHandler(func(data string) error {
		return c.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		var msg map[string]any
		json.Unmarshal(data, &msg)
		if msg["type"] == "request" {
			c.WriteJSON(map[string]any{"id": msg["id"], "type": "offer", "sdp": sdp,
				"candidates": []any{map[string]any{"url": "203.0.113.7:3478?transport=udp", "username": "1:x", "password": "p"}}})
			c.WriteJSON(map[string]any{"type": "report", "result": "connected", "path": "direct", "relay_offered": true, "relay_ready": true, "setup_ms": 900})
		}
	}
}

func main() {
	mode := flag.String("mode", "idle", "idle (hold the cameras until SIGTERM) or sessions")
	base := flag.String("url", "wss://127.0.0.1:18443", "control server")
	n := flag.Int("n", 1000, "cameras")
	workers := flag.Int("workers", 16, "sessions: viewers working in parallel")
	dur := flag.Duration("dur", 20*time.Second, "sessions: how long")
	flag.Parse()
	var wg sync.WaitGroup
	var failedCams atomic.Int64
	start := time.Now()
	sem := make(chan struct{}, 32)
	for i := 0; i < *n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			c, err := dial(*base, fmt.Sprintf("cam%06d", i))
			<-sem
			wg.Done()
			if err != nil {
				failedCams.Add(1)
				return
			}
			serveCamera(c)
		}(i)
	}
	wg.Wait()
	fmt.Printf("connected %d cameras (%d failed) in %s\n", *n-int(failedCams.Load()), failedCams.Load(), time.Since(start).Round(time.Millisecond))
	if *mode == "idle" {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
		<-stop
		return
	}
	var done, failed atomic.Int64
	end := time.Now().Add(*dur)
	var viewers sync.WaitGroup
	for w := 0; w < *workers; w++ {
		viewers.Add(1)
		go func(w int) {
			defer viewers.Done()
			for k := 0; time.Now().Before(end); k++ {
				c, err := dial(*base, fmt.Sprintf("Player%d_%d", w, k))
				if err != nil {
					failed.Add(1)
					continue
				}
				c.WriteJSON(map[string]any{"type": "request", "id": fmt.Sprintf("cam%06d", (w*7919+k)%*n)})
				c.SetReadDeadline(time.Now().Add(5 * time.Second))
				_, data, err := c.ReadMessage()
				if err != nil || !strings.Contains(string(data), `"offer"`) {
					failed.Add(1)
					c.Close()
					continue
				}
				c.WriteJSON(map[string]any{"id": "x", "type": "answer", "sdp": sdp})
				c.WriteJSON(map[string]any{"type": "report", "result": "connected", "path": "relay", "relay_protocol": "udp", "relay_offered": true, "relay_ready": true, "setup_ms": 1500})
				c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				c.Close()
				done.Add(1)
			}
		}(w)
	}
	viewers.Wait()
	fmt.Printf("sessions %d (%.0f/s), failed %d\n", done.Load(), float64(done.Load())/dur.Seconds(), failed.Load())
}
