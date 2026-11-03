package main

import (
	"fmt"
	"net/http"
	"net/netip"
	"strings"

	"signaling-server/internal/ratelimit"
	"signaling-server/internal/stats"
)

// publicLimits are the per-source limits on the public port (-rate-*).
type publicLimits struct {
	api     ratelimit.Limiter // Center calls: activation, viewer tickets, lookups
	connect ratelimit.Limiter // new signaling connections
	web     ratelimit.Limiter // everything else: player, downloads, health
	conns   ratelimit.Conns   // open signaling connections
	exempt  []netip.Prefix
}

// limitSource is the address limits apply to; ok is false when limits are
// off or the source is exempt.
func (s *Server) limitSource(r *http.Request) (netip.Addr, bool) {
	if s.limits == nil {
		return netip.Addr{}, false
	}
	addrPort, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	addr := addrPort.Addr().Unmap()
	for _, prefix := range s.limits.exempt {
		if prefix.Contains(addr) {
			return netip.Addr{}, false
		}
	}
	return addr, true
}

// allowRequest takes a token from the request's class and answers 429 when
// its source has none left.
func (s *Server) allowRequest(w http.ResponseWriter, r *http.Request) bool {
	source, limited := s.limitSource(r)
	if !limited {
		return true
	}
	limiter := &s.limits.web
	switch {
	case isCenterPath(r.URL.Path):
		limiter = &s.limits.api
	case strings.EqualFold(r.Header.Get("Upgrade"), "websocket"):
		limiter = &s.limits.connect
	}
	if limiter.Allow(source) {
		return true
	}
	s.countStat("rate_limited", (*stats.Counters).RateLimited)
	tooMany(w)
	return false
}

func tooMany(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	http.Error(w, "too many requests", http.StatusTooManyRequests)
}

// parseLimits builds the limits from the -rate-* flags; all zero turns them
// off.
func parseLimits(apiPerSecond, webPerSecond float64, connectPerMinute float64, connsPerSource int, exemptText string) (*publicLimits, error) {
	if apiPerSecond < 0 || webPerSecond < 0 || connectPerMinute < 0 || connsPerSource < 0 {
		return nil, fmt.Errorf("rate limits must not be negative")
	}
	exempt, err := parsePlayerAllow(exemptText)
	if err != nil {
		return nil, fmt.Errorf("-rate-exempt: %w", err)
	}
	if apiPerSecond == 0 && webPerSecond == 0 && connectPerMinute == 0 && connsPerSource == 0 {
		return nil, nil
	}
	return &publicLimits{
		// Bursts cover a burst of reconnects: every camera behind one carrier
		// NAT address coming back after a restart within the cameras' 25-35 s
		// random wait.
		api:     ratelimit.Limiter{Rate: apiPerSecond, Burst: 10 * apiPerSecond},
		connect: ratelimit.Limiter{Rate: connectPerMinute / 60, Burst: connectPerMinute},
		web:     ratelimit.Limiter{Rate: webPerSecond, Burst: 4 * webPerSecond},
		conns:   ratelimit.Conns{Max: connsPerSource},
		exempt:  exempt,
	}, nil
}
