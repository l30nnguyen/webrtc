package ratelimit

import (
	"net/netip"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestTokenBucket(t *testing.T) {
	c := &clock{time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)}
	l := &Limiter{Rate: 2, Burst: 5, Now: c.now}
	a := netip.MustParseAddr("203.0.113.7")
	b := netip.MustParseAddr("203.0.113.8")
	for i := 0; i < 5; i++ {
		if !l.Allow(a) {
			t.Fatalf("burst request %d refused", i)
		}
	}
	if l.Allow(a) {
		t.Fatal("request over the burst allowed")
	}
	if !l.Allow(b) {
		t.Fatal("another source was limited")
	}
	c.t = c.t.Add(time.Second) // two tokens back
	if !l.Allow(a) || !l.Allow(a) || l.Allow(a) {
		t.Fatal("refill is not Rate per second")
	}
}

func TestIPv6IsLimitedPerSlash64(t *testing.T) {
	l := &Limiter{Rate: 1, Burst: 1, Now: (&clock{time.Unix(0, 0)}).now}
	if !l.Allow(netip.MustParseAddr("2001:db8:1:2::1")) {
		t.Fatal("first request refused")
	}
	if l.Allow(netip.MustParseAddr("2001:db8:1:2:ffff::9")) {
		t.Fatal("same /64 not limited together")
	}
	if !l.Allow(netip.MustParseAddr("2001:db8:1:3::1")) {
		t.Fatal("another /64 limited")
	}
	if Key(netip.MustParseAddr("::ffff:198.51.100.1")) != netip.MustParseAddr("198.51.100.1") {
		t.Fatal("mapped IPv4 not unmapped")
	}
}

func TestIdleSourcesAreForgotten(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	l := &Limiter{Rate: 1, Burst: 2, Now: c.now}
	l.Allow(netip.MustParseAddr("192.0.2.1"))
	c.t = c.t.Add(2 * time.Minute)
	l.Allow(netip.MustParseAddr("192.0.2.2")) // triggers the sweep
	if l.Tracked() != 1 {
		t.Fatalf("tracked %d", l.Tracked())
	}
}

func TestZeroRateMeansNoLimit(t *testing.T) {
	var l *Limiter
	if !l.Allow(netip.MustParseAddr("192.0.2.1")) || !(&Limiter{}).Allow(netip.MustParseAddr("192.0.2.1")) {
		t.Fatal("disabled limiter refused")
	}
}

func TestConns(t *testing.T) {
	c := &Conns{Max: 2}
	a := netip.MustParseAddr("192.0.2.1")
	if !c.Acquire(a) || !c.Acquire(a) || c.Acquire(a) {
		t.Fatal("limit of 2 not enforced")
	}
	c.Release(a)
	if !c.Acquire(a) {
		t.Fatal("released slot not reusable")
	}
	c.Release(a)
	c.Release(a)
	c.Release(a) // extra release must not go negative
	if !c.Acquire(a) || !c.Acquire(a) || c.Acquire(a) {
		t.Fatal("count went wrong after releases")
	}
}
