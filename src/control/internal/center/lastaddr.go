package center

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
)

// LastAddresses remembers the source address each camera last verified
// from. A takeover compares the new camera's address with the old holder's:
// the same network is most likely the same camera after a factory reset; a
// different one may be a leaked SID, so Center raises an alert (roll-out.md
// R1). It is kept in a file so a restart keeps it.
type LastAddresses struct {
	mu    sync.Mutex
	path  string // "" keeps it in memory only
	addrs map[string]netip.Addr
	dirty bool
}

// OpenLastAddresses loads path (a missing file is empty); "" means memory
// only. On an unreadable file it returns an empty store for path (the next
// Save replaces the file) together with the error.
func OpenLastAddresses(path string) (*LastAddresses, error) {
	l := &LastAddresses{path: path, addrs: map[string]netip.Addr{}}
	if path == "" {
		return l, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return l, err
	}
	for device, text := range raw {
		if addr, err := netip.ParseAddr(text); err == nil {
			l.addrs[device] = addr
		}
	}
	return l, nil
}

// Set records the address a camera verified from.
func (l *LastAddresses) Set(device string, addr netip.Addr) {
	if !addr.IsValid() {
		return
	}
	addr = addr.Unmap()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.addrs[device] != addr {
		l.addrs[device] = addr
		l.dirty = true
	}
}

// Get is the address a camera last verified from.
func (l *LastAddresses) Get(device string) (netip.Addr, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	addr, ok := l.addrs[device]
	return addr, ok
}

// Save writes the file when something changed (owner-only, replaced
// atomically). Run it every minute and at stop.
func (l *LastAddresses) Save() error {
	l.mu.Lock()
	if l.path == "" || !l.dirty {
		l.mu.Unlock()
		return nil
	}
	raw := make(map[string]string, len(l.addrs))
	for device, addr := range l.addrs {
		raw[device] = addr.String()
	}
	l.dirty = false
	l.mu.Unlock()
	data, err := json.Marshal(raw)
	if err == nil {
		tmp := filepath.Join(filepath.Dir(l.path), "."+filepath.Base(l.path)+".tmp")
		if err = os.WriteFile(tmp, data, 0o600); err == nil {
			err = os.Rename(tmp, l.path)
		}
	}
	if err != nil {
		l.mu.Lock()
		l.dirty = true
		l.mu.Unlock()
	}
	return err
}

// sameNetwork reports whether two addresses are in the same /24 (IPv4) or
// /64 (IPv6): home lines often change address inside the provider's block.
func sameNetwork(a, b netip.Addr) bool {
	a, b = a.Unmap(), b.Unmap()
	if !a.IsValid() || !b.IsValid() || a.Is4() != b.Is4() {
		return false
	}
	bits := 64
	if a.Is4() {
		bits = 24
	}
	pa, err1 := a.Prefix(bits)
	pb, err2 := b.Prefix(bits)
	return err1 == nil && err2 == nil && pa == pb
}

// TakeoverInfo describes one takeover for OnTakeover.
type TakeoverInfo struct {
	Tenant, FromDevice, ToDevice string
	// OldAddress is where the old holder last verified from (invalid when
	// unknown), NewAddress where the new camera verifies from now.
	OldAddress, NewAddress netip.Addr
	OldCountry, NewCountry string
	// SameNetwork: both addresses known and in the same /24 or /64. When
	// false Center logs an ALERT: the SID may have leaked.
	SameNetwork bool
}
