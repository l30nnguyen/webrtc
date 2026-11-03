package main

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/pion/turn/v5"
)

// stats are the relay's live counters. Traffic is counted on the relay
// sockets (between the relay and the peers), so it moves while a session runs.
type stats struct {
	allocations      atomic.Int64 // current
	allocationsTotal atomic.Int64
	authFailures     atomic.Int64
	quotaRejects     atomic.Int64
	peersDenied      atomic.Int64
	rxBytes, txBytes atomic.Int64

	tlsPort int

	mu      sync.Mutex
	perUser map[string]int
	byProto map[string]int // by the client's transport: udp, tcp or tls
}

func newStats(tlsPort int) *stats {
	return &stats{tlsPort: tlsPort, perUser: map[string]int{}, byProto: map[string]int{}}
}

// transport is how a client reached the relay. pion's own protocol argument
// is the relayed transport, which is always UDP here.
func (s *stats) transport(src, dst net.Addr) string {
	if _, ok := src.(*net.UDPAddr); ok {
		return "udp"
	}
	if tcp, ok := dst.(*net.TCPAddr); ok && s.tlsPort != 0 && tcp.Port == s.tlsPort {
		return "tls"
	}
	return "tcp"
}

func (s *stats) quota(maxAll, maxPerUser int) turn.QuotaHandler {
	// pion passes the userID the auth handler returned.
	return func(userID, realm string, srcAddr net.Addr) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		// A fixed user is shared by every session (as violet's), so only the
		// total limits it; minted names are limited per camera too.
		minted := !strings.HasPrefix(userID, fixedPrefix)
		if (maxAll > 0 && s.allocations.Load() >= int64(maxAll)) ||
			(minted && maxPerUser > 0 && s.perUser[userID] >= maxPerUser) {
			s.quotaRejects.Add(1)
			return false
		}
		return true
	}
}

func (s *stats) events() turn.EventHandler {
	return turn.EventHandler{
		OnAuth: func(srcAddr, dstAddr net.Addr, protocol, username, realm, method string, verdict bool) {
			if !verdict {
				s.authFailures.Add(1)
			}
		},
		OnAllocationCreated: func(srcAddr, dstAddr net.Addr, protocol, userID, realm string, relayAddr net.Addr, requestedPort int) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.allocations.Add(1)
			s.allocationsTotal.Add(1)
			s.perUser[userID]++
			s.byProto[s.transport(srcAddr, dstAddr)]++
		},
		OnAllocationDeleted: func(srcAddr, dstAddr net.Addr, protocol, userID, realm string) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.allocations.Add(-1)
			if s.perUser[userID]--; s.perUser[userID] <= 0 {
				delete(s.perUser, userID)
			}
			if p := s.transport(srcAddr, dstAddr); s.byProto[p] > 0 {
				s.byProto[p]--
			}
		},
	}
}

// transports counts the current allocations by the client's transport.
func (s *stats) transports() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int64{"udp": 0, "tcp": 0, "tls": 0}
	for p, n := range s.byProto {
		out[p] = int64(n)
	}
	return out
}

// handler serves the counters as Prometheus text. turn_total_* are the names
// the node monitor reads (as from coturn); the rest are for people.
func (s *stats) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		byProto := make(map[string]int, len(s.byProto))
		for p, n := range s.byProto {
			byProto[p] = n
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "# TYPE turn_total_allocations gauge\nturn_total_allocations %d\n", s.allocations.Load())
		fmt.Fprintf(w, "# TYPE turn_total_traffic_rcvb counter\nturn_total_traffic_rcvb %d\n", s.rxBytes.Load())
		fmt.Fprintf(w, "# TYPE turn_total_traffic_sentb counter\nturn_total_traffic_sentb %d\n", s.txBytes.Load())
		fmt.Fprint(w, "# TYPE matrix_relay_allocations gauge\n")
		for _, p := range []string{"udp", "tcp", "tls"} {
			fmt.Fprintf(w, "matrix_relay_allocations{transport=%q} %d\n", p, byProto[p])
		}
		fmt.Fprintf(w, "# TYPE matrix_relay_allocations_total counter\nmatrix_relay_allocations_total %d\n", s.allocationsTotal.Load())
		fmt.Fprintf(w, "# TYPE matrix_relay_auth_failures_total counter\nmatrix_relay_auth_failures_total %d\n", s.authFailures.Load())
		fmt.Fprintf(w, "# TYPE matrix_relay_quota_rejects_total counter\nmatrix_relay_quota_rejects_total %d\n", s.quotaRejects.Load())
		fmt.Fprintf(w, "# TYPE matrix_relay_peers_denied_total counter\nmatrix_relay_peers_denied_total %d\n", s.peersDenied.Load())
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	return mux
}

// countingGenerator counts the traffic of the relay sockets it allocates.
type countingGenerator struct {
	next  turn.RelayAddressGenerator
	stats *stats
}

func (g *countingGenerator) Validate() error { return g.next.Validate() }

func (g *countingGenerator) AllocatePacketConn(c turn.AllocateListenerConfig) (net.PacketConn, net.Addr, error) {
	conn, addr, err := g.next.AllocatePacketConn(c)
	if err != nil {
		return nil, nil, err
	}
	return &countingConn{PacketConn: conn, stats: g.stats}, addr, nil
}

func (g *countingGenerator) AllocateListener(c turn.AllocateListenerConfig) (net.Listener, net.Addr, error) {
	return g.next.AllocateListener(c)
}

func (g *countingGenerator) AllocateConn(c turn.AllocateConnConfig) (net.Conn, error) {
	return g.next.AllocateConn(c)
}

type countingConn struct {
	net.PacketConn
	stats *stats
}

func (c *countingConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(p)
	c.stats.rxBytes.Add(int64(n))
	return n, addr, err
}

func (c *countingConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	n, err := c.PacketConn.WriteTo(p, addr)
	c.stats.txBytes.Add(int64(n))
	return n, err
}

// peerFilter refuses permissions to addresses inside the relay host's own
// networks (loopback, private, link-local such as the cloud metadata
// address), so the relay can't be used to reach them.
func peerFilter(allowPrivate bool, s *stats) turn.PermissionHandler {
	return func(clientAddr net.Addr, peer net.IP) bool {
		if allowPrivate {
			return true
		}
		if peer.IsLoopback() || peer.IsPrivate() || peer.IsLinkLocalUnicast() || peer.IsUnspecified() ||
			peer.IsMulticast() || peer.IsLinkLocalMulticast() {
			s.peersDenied.Add(1)
			return false
		}
		return true
	}
}
