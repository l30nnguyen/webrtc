// Package firmware picks which camera firmware version a download gets
// (roll-out.md R7, server/design/firmware-channels.md).
//
// cfg/firmware-channels.json names, per model (HCX130, HCX330L, GIRAFFE_HD:
// the plain /downloads/<MODEL>/ files) and per platform (anycloud: the signed
// /v1/firmware/<PLATFORM>/matrix package), a stable version and optionally a
// canary version. Cameras in the canary group get the canary version, every
// other camera the stable one:
//
//	{
//	  "firmware": {
//	    "HCX130":   {"stable": "2.2.2", "canary": "2.2.3", "previous": "2.2.1"},
//	    "anycloud": {"stable": "2.2.2", "canary": "2.2.3"}
//	  },
//	  "canary": {"ips": ["115.78.12.186"], "tenants": ["test01"], "sids": ["3fa1c2d4e5f6"]}
//	}
//
// The /downloads route only knows the camera's address, so its canary group
// is "ips" (an address or a CIDR prefix). The /v1/firmware route also knows
// the SID, so "tenants" and "sids" (digest prefix of at least 12 hex, as the
// logs print it) count there as well.
//
// Without the file, or for a name it does not list, Pick returns "" and the
// caller serves the unversioned file as before. The file is read again when
// it changes; a file that does not parse is refused and the last good one
// stays in use.
package firmware

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Channel names returned by Pick.
const (
	Stable = "stable"
	Canary = "canary"
)

var (
	versionPattern = regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}(-[a-z0-9]{1,16})?$`)
	namePattern    = regexp.MustCompile(`^[A-Za-z0-9_]{1,16}$`)
	digestPattern  = regexp.MustCompile(`^[0-9a-f]{12,64}$`)
	tenantPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// Entry is one model's or platform's versions.
type Entry struct {
	Stable   string `json:"stable"`
	Canary   string `json:"canary,omitempty"`
	Previous string `json:"previous,omitempty"`
}

// Group lists the cameras that get canary versions.
type Group struct {
	IPs     []string `json:"ips,omitempty"`
	Tenants []string `json:"tenants,omitempty"`
	SIDs    []string `json:"sids,omitempty"`
}

// File is cfg/firmware-channels.json.
type File struct {
	Firmware map[string]Entry `json:"firmware"`
	Canary   Group            `json:"canary"`
}

// Who describes the camera asking for a download.
type Who struct {
	Addr      netip.Addr
	Tenant    string // empty on /downloads
	SIDDigest string // full hex digest; empty on /downloads
}

// ValidVersion reports whether v may name a version folder.
func ValidVersion(v string) bool { return versionPattern.MatchString(v) }

// Parse checks a channels file.
func Parse(data []byte) (*File, []netip.Prefix, error) {
	var f File
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, nil, fmt.Errorf("firmware channels: %w", err)
	}
	for name, e := range f.Firmware {
		if !namePattern.MatchString(name) {
			return nil, nil, fmt.Errorf("firmware channels: bad name %q", name)
		}
		if !ValidVersion(e.Stable) {
			return nil, nil, fmt.Errorf("firmware channels: %s: bad stable version %q", name, e.Stable)
		}
		for _, v := range []string{e.Canary, e.Previous} {
			if v != "" && !ValidVersion(v) {
				return nil, nil, fmt.Errorf("firmware channels: %s: bad version %q", name, v)
			}
		}
	}
	var prefixes []netip.Prefix
	for _, s := range f.Canary.IPs {
		if p, err := netip.ParsePrefix(s); err == nil {
			prefixes = append(prefixes, p.Masked())
		} else if a, err := netip.ParseAddr(s); err == nil {
			prefixes = append(prefixes, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
		} else {
			return nil, nil, fmt.Errorf("firmware channels: bad canary address %q", s)
		}
	}
	for _, t := range f.Canary.Tenants {
		if !tenantPattern.MatchString(t) {
			return nil, nil, fmt.Errorf("firmware channels: bad canary tenant %q", t)
		}
	}
	for _, d := range f.Canary.SIDs {
		if !digestPattern.MatchString(d) {
			return nil, nil, fmt.Errorf("firmware channels: canary sid %q must be 12-64 lowercase hex", d)
		}
	}
	return &f, prefixes, nil
}

// Channels reads cfg/firmware-channels.json on demand.
type Channels struct {
	path string
	logf func(string, ...any)

	mu       sync.Mutex
	modTime  time.Time
	size     int64
	file     *File
	prefixes []netip.Prefix
}

// New returns Channels for path; logf receives refusals of a bad file.
func New(path string, logf func(string, ...any)) *Channels {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Channels{path: path, logf: logf}
}

// load re-reads the file when its size or time changed. Called with mu held.
func (c *Channels) load() {
	info, err := os.Stat(c.path)
	if err != nil {
		if c.file != nil && os.IsNotExist(err) {
			c.logf("firmware channels: %s removed; serving unversioned files", c.path)
		}
		if os.IsNotExist(err) {
			c.file, c.prefixes, c.modTime, c.size = nil, nil, time.Time{}, 0
		}
		return
	}
	if info.ModTime().Equal(c.modTime) && info.Size() == c.size {
		return
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	f, prefixes, err := Parse(data)
	c.modTime, c.size = info.ModTime(), info.Size()
	if err != nil {
		c.logf("%v (keeping the last good channels)", err)
		return
	}
	c.file, c.prefixes = f, prefixes
}

// Pick returns the version and channel for name (a model or platform), or
// "" when the channels file does not list it.
func (c *Channels) Pick(name string, who Who) (version, channel string) {
	if c == nil {
		return "", ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	if c.file == nil {
		return "", ""
	}
	e, ok := c.file.Firmware[name]
	if !ok {
		return "", ""
	}
	if e.Canary != "" && c.inCanary(who) {
		return e.Canary, Canary
	}
	return e.Stable, Stable
}

func (c *Channels) inCanary(who Who) bool {
	if who.Addr.IsValid() {
		a := who.Addr.Unmap()
		for _, p := range c.prefixes {
			if p.Contains(a) {
				return true
			}
		}
	}
	if who.Tenant != "" {
		for _, t := range c.file.Canary.Tenants {
			if t == who.Tenant {
				return true
			}
		}
	}
	if who.SIDDigest != "" {
		for _, d := range c.file.Canary.SIDs {
			if strings.HasPrefix(who.SIDDigest, d) {
				return true
			}
		}
	}
	return false
}

// Resolve returns the file to serve for <base>/<name>/<file>: the picked
// version's copy <base>/<name>/<version>/<file>, else the stable copy when a
// canary copy is missing, else the unversioned <base>/<name>/<file>. version
// and channel are empty when the unversioned file is served.
func (c *Channels) Resolve(base, name, file string, who Who) (path, version, channel string) {
	flat := base + "/" + name + "/" + file
	v, ch := c.Pick(name, who)
	if v == "" {
		return flat, "", ""
	}
	if p := base + "/" + name + "/" + v + "/" + file; regular(p) {
		return p, v, ch
	}
	if ch == Canary {
		c.mu.Lock()
		stable := ""
		if c.file != nil {
			stable = c.file.Firmware[name].Stable
		}
		c.mu.Unlock()
		if p := base + "/" + name + "/" + stable + "/" + file; stable != "" && regular(p) {
			c.logf("firmware channels: %s %s has no %s; serving stable %s", name, v, file, stable)
			return p, stable, Stable
		}
	}
	c.logf("firmware channels: %s %s has no %s; serving the unversioned file", name, v, file)
	return flat, "", ""
}

func regular(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
