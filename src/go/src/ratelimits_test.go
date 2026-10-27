package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"signaling-server/internal/stats"
)

func limitedServer(t *testing.T, api, web, connect float64, conns int, exempt string) *Server {
	t.Helper()
	srv := NewServer("", t.TempDir()+"/")
	srv.SetStats(stats.New(nil))
	limits, err := parseLimits(api, web, connect, conns, exempt)
	if err != nil {
		t.Fatal(err)
	}
	srv.limits = limits
	return srv
}

func get(srv *Server, path, from string) int {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = from
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	return w.Code
}

func TestRequestsOverTheLimitGet429(t *testing.T) {
	srv := limitedServer(t, 1, 1, 0, 0, "198.51.100.0/24")
	// web: rate 1, burst 4
	for i := 0; i < 4; i++ {
		if code := get(srv, "/api/health", "203.0.113.1:5000"); code == http.StatusTooManyRequests {
			t.Fatalf("request %d limited", i)
		}
	}
	if code := get(srv, "/api/health", "203.0.113.1:5001"); code != http.StatusTooManyRequests {
		t.Fatalf("5th request = %d", code)
	}
	// Another source and an exempt source are not affected.
	if code := get(srv, "/api/health", "203.0.113.2:5000"); code == http.StatusTooManyRequests {
		t.Fatal("another source limited")
	}
	for i := 0; i < 10; i++ {
		if code := get(srv, "/api/health", "198.51.100.9:5000"); code == http.StatusTooManyRequests {
			t.Fatal("exempt source limited")
		}
	}
	// Center calls have their own bucket (rate 1, burst 10).
	for i := 0; i < 10; i++ {
		if code := get(srv, "/v1/challenge", "203.0.113.1:5000"); code == http.StatusTooManyRequests {
			t.Fatalf("Center call %d limited by the web bucket", i)
		}
	}
	if code := get(srv, "/v1/challenge", "203.0.113.1:5000"); code != http.StatusTooManyRequests {
		t.Fatalf("11th Center call = %d", code)
	}
	if got := srv.stats.Snapshot(); func() int {
		n := 0
		for _, d := range got {
			n += d.RateLimited
		}
		return n
	}() != 2 {
		t.Fatalf("rate-limited count = %+v", got)
	}
}

func TestOpenConnectionsPerSource(t *testing.T) {
	srv := limitedServer(t, 0, 0, 0, 1, "")
	server := httptest.NewServer(srv)
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/camera1"
	first, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, response, err := websocket.DefaultDialer.Dial(url+"b", nil)
	if err == nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second connection from the same source: %v %v", response, err)
	}
	first.Close()
	eventually(t, "slot released", func() bool {
		conn, _, err := websocket.DefaultDialer.Dial(url+"c", nil)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	})
}

func TestNoLimitsWhenAllZero(t *testing.T) {
	if limits, err := parseLimits(0, 0, 0, 0, ""); err != nil || limits != nil {
		t.Fatalf("parseLimits(0...) = %v, %v", limits, err)
	}
	if _, err := parseLimits(-1, 0, 0, 0, ""); err == nil {
		t.Fatal("negative limit accepted")
	}
}
