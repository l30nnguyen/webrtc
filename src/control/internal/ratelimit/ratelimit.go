// Package ratelimit limits requests and open connections per source address.
//
// IPv4 addresses are limited one by one; IPv6 addresses by their /64, which
// one subscriber usually holds whole. Limits are meant to stop one source from
// flooding the public port, not to shape normal traffic: many cameras can sit
// behind one carrier NAT address, so the defaults are generous.
package ratelimit

import (
	"net/netip"
	"sync"
	"time"
)

// Key is the address a limit applies to: the IPv4 address, or the IPv6 /64.
func Key(addr netip.Addr) netip.Addr {
	addr = addr.Unmap()
	if addr.Is6() {
		prefix, _ := addr.Prefix(64)
		return prefix.Addr()
	}
	return addr
}

// MaxTracked bounds how many sources a limiter remembers. When it is full of
// recently active sources, a new source is refused until entries go idle.
const MaxTracked = 200000

// Limiter is a token bucket per source: Rate tokens a second, at most Burst
// saved up. A zero Rate means no limit.
type Limiter struct {
	Rate  float64
	Burst float64
	Now   func() time.Time // for tests

	mu        sync.Mutex
	buckets   map[netip.Addr]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func (l *Limiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Allow takes one token for addr's source and reports whether there was one.
func (l *Limiter) Allow(addr netip.Addr) bool {
	if l == nil || l.Rate <= 0 {
		return true
	}
	key := Key(addr)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = map[netip.Addr]*bucket{}
	}
	if now.Sub(l.lastSweep) > time.Minute || len(l.buckets) >= MaxTracked {
		l.sweepLocked(now)
	}
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= MaxTracked {
			return false
		}
		b = &bucket{tokens: l.Burst, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.Rate
	if b.tokens > l.Burst {
		b.tokens = l.Burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweepLocked forgets sources whose bucket has refilled: they are idle and
// would start full anyway.
func (l *Limiter) sweepLocked(now time.Time) {
	l.lastSweep = now
	for key, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.Rate >= l.Burst {
			delete(l.buckets, key)
		}
	}
}

// Tracked is the number of sources remembered.
func (l *Limiter) Tracked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Conns counts open connections per source, at most Max each. A zero Max
// means no limit.
type Conns struct {
	Max int

	mu     sync.Mutex
	counts map[netip.Addr]int
}

// Acquire counts a new connection from addr, or reports false when its
// source is at the limit. Every true must be matched by one Release.
func (c *Conns) Acquire(addr netip.Addr) bool {
	if c == nil || c.Max <= 0 {
		return true
	}
	key := Key(addr)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = map[netip.Addr]int{}
	}
	if c.counts[key] >= c.Max {
		return false
	}
	c.counts[key]++
	return true
}

// Release ends a connection counted by Acquire.
func (c *Conns) Release(addr netip.Addr) {
	if c == nil || c.Max <= 0 {
		return
	}
	key := Key(addr)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts[key] <= 1 {
		delete(c.counts, key)
		return
	}
	c.counts[key]--
}
