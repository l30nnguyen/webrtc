package relay

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"signaling-server/internal/fleet"
)

func TestParseMetrics(t *testing.T) {
	text := `# HELP node_network_receive_bytes_total Network device statistic receive_bytes.
node_network_receive_bytes_total{device="eth0"} 1.5e+06
node_network_receive_bytes_total{device="lo"} 9e+09
node_network_transmit_bytes_total{device="eth0"} 2000000
turn_total_traffic_rcvb 7
turn_total_allocations{type="UDP"} 3
turn_total_allocations{type="TCP"} 1
`
	m, err := ParseMetrics(strings.NewReader(text))
	if err != nil || !m.hasTraffic || m.rx != 1.5e6 || m.tx != 2e6 || m.allocations != 4 {
		t.Fatalf("parsed %+v %v", m, err)
	}
	// Without host counters, coturn's own traffic counters are used.
	m, _ = ParseMetrics(strings.NewReader("turn_total_traffic_rcvb 10\nturn_total_traffic_sentb 20\n"))
	if !m.hasTraffic || m.rx != 10 || m.tx != 20 {
		t.Fatalf("coturn only: %+v", m)
	}
	m, _ = ParseMetrics(strings.NewReader("garbage line\nother_metric 5\n"))
	if m.hasTraffic {
		t.Fatal("traffic from unrelated metrics")
	}
}

// stunResponder answers binding requests like a relay server's port.
func stunResponder(t *testing.T) (string, int) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if n >= 20 && binary.BigEndian.Uint16(buf) == 0x0001 {
				answer := make([]byte, 20)
				binary.BigEndian.PutUint16(answer, 0x0101)
				copy(answer[4:20], buf[4:20])
				conn.WriteTo(answer, from)
			}
		}
	}()
	addr := conn.LocalAddr().(*net.UDPAddr)
	return addr.IP.String(), addr.Port
}

func TestProbe(t *testing.T) {
	host, port := stunResponder(t)
	if err := Probe(context.Background(), host, port); err != nil {
		t.Fatal(err)
	}
	silent, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	if err := Probe(context.Background(), "127.0.0.1", silent.LocalAddr().(*net.UDPAddr).Port); err == nil {
		t.Fatal("silent port probed healthy")
	}
}

func TestMonitorMeasuresBandwidthAndHealth(t *testing.T) {
	host, port := stunResponder(t)
	var sent atomic.Int64
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "node_network_receive_bytes_total{device=\"eth0\"} 0\nnode_network_transmit_bytes_total{device=\"eth0\"} %d\nturn_total_allocations 2\n", sent.Load())
	}))
	defer metrics.Close()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	m := &Monitor{Now: func() time.Time { return now }}
	relays := []fleet.Relay{{Name: "de1", Region: "DE", Host: host, Port: port, CapacityMbps: 100, Metrics: metrics.URL}}

	m.Measure(context.Background(), relays)
	if fraction, healthy, known := m.Load("de1"); !known || !healthy || fraction != 0 {
		t.Fatalf("first measurement %v %v %v", fraction, healthy, known)
	}
	sent.Store(50e6 / 8 * 15) // 50 Mbit/s over 15 s
	now = now.Add(15 * time.Second)
	m.Measure(context.Background(), relays)
	fraction, healthy, known := m.Load("de1")
	if !known || !healthy || fraction < 0.49 || fraction > 0.51 {
		t.Fatalf("load %v %v %v", fraction, healthy, known)
	}
	if s := m.Statuses(); len(s) != 1 || s[0].Allocations != 2 || !s[0].Measured {
		t.Fatalf("statuses %+v", s)
	}
	// An old measurement stops counting.
	now = now.Add(time.Hour)
	if _, _, known := m.Load("de1"); known {
		t.Fatal("stale measurement still known")
	}
}

// streamResponder answers binding requests over TCP, or TLS with config.
func streamResponder(t *testing.T, config *tls.Config) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if config != nil {
		l = tls.NewListener(l, config)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				request := make([]byte, 20)
				if _, err := io.ReadFull(conn, request); err != nil {
					return
				}
				answer := make([]byte, 20)
				binary.BigEndian.PutUint16(answer[0:], 0x0101)
				copy(answer[4:20], request[4:20])
				conn.Write(answer)
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func TestProbeStream(t *testing.T) {
	if err := ProbeStream(context.Background(), "127.0.0.1", streamResponder(t, nil), "", nil); err != nil {
		t.Fatalf("tcp: %v", err)
	}
	server := httptest.NewUnstartedServer(nil)
	server.StartTLS() // only for its certificate, valid for "example.com"
	defer server.Close()
	tlsPort := streamResponder(t, server.TLS)
	roots := &tls.Config{RootCAs: server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
	if err := ProbeStream(context.Background(), "127.0.0.1", tlsPort, "example.com", roots); err != nil {
		t.Fatalf("tls: %v", err)
	}
	if err := ProbeStream(context.Background(), "127.0.0.1", tlsPort, "other.test", roots); err == nil {
		t.Fatal("certificate for another name accepted")
	}
	if err := ProbeStream(context.Background(), "127.0.0.1", tlsPort, "example.com", nil); err == nil {
		t.Fatal("untrusted certificate accepted")
	}
}

func TestMonitorMeasuresTCPAndTLS(t *testing.T) {
	host, port := stunResponder(t)
	m := &Monitor{}
	silent, _ := net.Listen("tcp", "127.0.0.1:0")
	silent.Close()
	m.Measure(context.Background(), []fleet.Relay{{Name: "r", Host: host, Port: port, TCPPort: streamResponder(t, nil), TLSPort: silent.Addr().(*net.TCPAddr).Port, TLSHost: "localhost"}})
	if up, known := m.Transport("r", "tcp"); !up || !known {
		t.Fatalf("tcp %v %v", up, known)
	}
	if up, known := m.Transport("r", "tls"); up || !known {
		t.Fatalf("tls %v %v", up, known)
	}
	if s := m.Statuses(); len(s) != 1 || s[0].TCP != "up" || s[0].TLS != "down" || !s[0].Healthy {
		t.Fatalf("statuses %+v", s)
	}
	m.Measure(context.Background(), []fleet.Relay{{Name: "u", Host: host, Port: port}})
	if _, known := m.Transport("u", "tcp"); known {
		t.Fatal("a relay without a TCP port has a TCP state")
	}
}
