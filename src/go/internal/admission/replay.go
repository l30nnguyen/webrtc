package admission

import (
	"sync"
	"time"
)

// ReplayCache makes tickets single-use. A node records each accepted ticket id
// until the ticket would have expired anyway, so a ticket copied from a log or
// a proxy cannot open a second connection.
type ReplayCache struct {
	mu      sync.Mutex
	seen    map[string]int64 // jti -> expiry (unix seconds)
	nextGC  int64
	Now     func() time.Time
	MaxSize int // zero means 1,000,000
}

// Use records the ticket id and reports whether it is the first use. It also
// refuses (false) when the cache is full of unexpired entries rather than
// forgetting some, because forgetting would reopen replay.
func (c *ReplayCache) Use(jti string, expires int64) bool {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	nowSec := now().Unix()
	max := c.MaxSize
	if max <= 0 {
		max = 1_000_000
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = make(map[string]int64)
	}
	if nowSec >= c.nextGC || len(c.seen) >= max {
		for id, exp := range c.seen {
			if exp <= nowSec {
				delete(c.seen, id)
			}
		}
		c.nextGC = nowSec + 30
	}
	if _, used := c.seen[jti]; used || len(c.seen) >= max {
		return false
	}
	c.seen[jti] = expires
	return true
}

// Seen reports whether the ticket id was already used, without recording it.
// A node calls it before the WebSocket upgrade so an obvious replay is refused
// with a normal HTTP error, then calls Use only once the upgrade has worked, so
// a failed handshake does not burn the ticket.
func (c *ReplayCache) Seen(jti string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, used := c.seen[jti]
	return used
}
