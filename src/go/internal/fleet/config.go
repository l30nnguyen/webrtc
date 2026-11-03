package fleet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strings"

	"signaling-server/internal/admission"
)

// Config is Center's fleet configuration file (config.json): the control
// server nodes and the country routing rules. The phase 2 nodes file
// ({"nodes": [...]}) is a valid Config.
type Config struct {
	// Version is stamped by "matrix-fleet apply"; operators do not write it.
	Version int    `json:"version,omitempty"`
	Nodes   []Node `json:"nodes"`
	// Routes maps an ISO 3166 alpha-2 country code, or "*" for every other
	// country, to the region whose nodes take its cameras.
	Routes map[string]string `json:"routes,omitempty"`
	// Relays are the relay stream servers nodes hand to cameras and viewers.
	Relays []Relay `json:"relays,omitempty"`
}

// Relay is one relay stream server (coturn with use-auth-secret). It carries
// no secret: each node keeps the shared secrets of the relays it hands out.
type Relay struct {
	Name   string `json:"name"`
	Region string `json:"region"`
	Host   string `json:"host"`
	Port   int    `json:"port,omitempty"`
	// CapacityMbps is the relayed bandwidth the server is filled to.
	CapacityMbps int `json:"capacity_mbps,omitempty"`
	// Metrics is the server's Prometheus endpoint, read by the nodes of its
	// region for its load (optional).
	Metrics string `json:"metrics,omitempty"`
	// TCPPort and TLSPort are the relay's TCP and TLS ports, handed to
	// viewers that can't reach it over UDP (0: the relay has none). TLSHost
	// is the name its certificate is for; it defaults to Host, which must
	// then be a host name.
	TCPPort int    `json:"tcp_port,omitempty"`
	TLSPort int    `json:"tls_port,omitempty"`
	TLSHost string `json:"tls_host,omitempty"`
}

// CertificateHost is the name viewers check the relay's TLS certificate for.
func (r Relay) CertificateHost() string {
	if r.TLSHost != "" {
		return r.TLSHost
	}
	return r.Host
}

const (
	// DefaultRelayPort is the relay stream server port when a relay names none.
	DefaultRelayPort = 3478
	// DefaultRelayCapacityMbps is used when a relay names no capacity.
	DefaultRelayCapacityMbps = 1000
)

// MaxConfigBytes bounds the configuration file.
const MaxConfigBytes = 1 << 20

// ParseConfig decodes a configuration file strictly: unknown fields are
// errors, so a misspelled key is never silently ignored.
func ParseConfig(data []byte) (Config, error) {
	var c Config
	if len(data) > MaxConfigBytes {
		return c, fmt.Errorf("larger than %d bytes", MaxConfigBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return c, err
	}
	if decoder.More() {
		return c, fmt.Errorf("unexpected data after the JSON object")
	}
	return c, nil
}

// LoadConfig reads and parses a configuration file.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return ParseConfig(data)
}

// Normalize upper-cases country codes and fills default capacities.
func (c *Config) Normalize() {
	for i := range c.Nodes {
		n := &c.Nodes[i]
		if n.Capacity == 0 {
			n.Capacity = DefaultCapacity
		}
		for j, code := range n.Countries {
			n.Countries[j] = strings.ToUpper(code)
		}
	}
	for i := range c.Relays {
		r := &c.Relays[i]
		if r.Port == 0 {
			r.Port = DefaultRelayPort
		}
		if r.CapacityMbps == 0 {
			r.CapacityMbps = DefaultRelayCapacityMbps
		}
	}
	if len(c.Routes) > 0 {
		routes := make(map[string]string, len(c.Routes))
		for country, region := range c.Routes {
			routes[strings.ToUpper(country)] = region
		}
		c.Routes = routes
	}
}

// Validate checks the configuration for the Center whose own node is self
// (that node needs no report token). It reports the first problem found.
func (c Config) Validate(self string) error {
	addresses := map[string]bool{}
	names := map[string]bool{}
	regions := map[string]bool{}
	for _, n := range c.Nodes {
		if admission.ValidateAudience(n.Address) != nil {
			return fmt.Errorf("node address %q must be an exact wss://host[:port]/ URL", n.Address)
		}
		if addresses[n.Address] {
			return fmt.Errorf("node %s is listed twice", n.Address)
		}
		addresses[n.Address] = true
		if n.Name != "" {
			if !validName(n.Name) {
				return fmt.Errorf("node %s: name %q must be 1-32 characters of [a-z0-9-]", n.Address, n.Name)
			}
			if names[n.Name] {
				return fmt.Errorf("node name %q is used twice", n.Name)
			}
			names[n.Name] = true
		}
		if n.Region != "" && !validName(strings.ToLower(n.Region)) {
			return fmt.Errorf("node %s: region %q must be 1-32 characters of [A-Za-z0-9-]", n.Address, n.Region)
		}
		if n.Capacity < 0 {
			return fmt.Errorf("node %s has a negative capacity", n.Address)
		}
		for _, code := range n.Countries {
			if !validCountry(strings.ToUpper(code)) {
				return fmt.Errorf("node %s: country %q must be a two-letter ISO 3166 code", n.Address, code)
			}
		}
		if n.PublicKey != "" {
			if n.Address == self {
				return fmt.Errorf("node %s is this Center's own node and takes no public_key", n.Address)
			}
			if _, ok := n.LinkKey(); !ok {
				return fmt.Errorf("node %s: public_key must be a raw Ed25519 key in base64url (43 characters)", n.Address)
			}
		}
		if n.Address == self && len(n.LinkFrom) > 0 {
			return fmt.Errorf("node %s is this Center's own node and takes no link_from", n.Address)
		}
		for _, item := range n.LinkFrom {
			if _, err := ParsePrefix(item); err != nil {
				return fmt.Errorf("node %s: link_from %q must be an IP address or CIDR", n.Address, item)
			}
		}
		if n.Region != "" {
			regions[n.Region] = true
		}
	}
	relayNames := map[string]bool{}
	for _, r := range c.Relays {
		if !validName(r.Name) {
			return fmt.Errorf("relay name %q must be 1-32 characters of [a-z0-9-]", r.Name)
		}
		if relayNames[r.Name] {
			return fmt.Errorf("relay name %q is used twice", r.Name)
		}
		relayNames[r.Name] = true
		if !validName(strings.ToLower(r.Region)) {
			return fmt.Errorf("relay %s: region %q must be 1-32 characters of [A-Za-z0-9-]", r.Name, r.Region)
		}
		if !validHost(r.Host) {
			return fmt.Errorf("relay %s: host %q must be a host name or IP address", r.Name, r.Host)
		}
		if r.Port < 0 || r.Port > 65535 || r.TCPPort < 0 || r.TCPPort > 65535 || r.TLSPort < 0 || r.TLSPort > 65535 {
			return fmt.Errorf("relay %s: a port is out of range", r.Name)
		}
		if r.TLSHost != "" && (r.TLSPort == 0 || !validHost(r.TLSHost)) {
			return fmt.Errorf("relay %s: tls_host %q needs tls_port and must be a host name", r.Name, r.TLSHost)
		}
		if _, err := netip.ParseAddr(r.CertificateHost()); r.TLSPort != 0 && err == nil {
			return fmt.Errorf("relay %s: tls_port needs a host name for the certificate: set tls_host", r.Name)
		}
		if r.CapacityMbps < 0 {
			return fmt.Errorf("relay %s has a negative capacity", r.Name)
		}
		if r.Metrics != "" {
			u, err := url.Parse(r.Metrics)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
				return fmt.Errorf("relay %s: metrics must be an http(s)://host:port/path URL", r.Name)
			}
		}
	}
	for country, region := range c.Routes {
		if country != "*" && !validCountry(strings.ToUpper(country)) {
			return fmt.Errorf("route %q: the key must be a two-letter ISO 3166 code or \"*\"", country)
		}
		if !regions[region] {
			return fmt.Errorf("route %s -> %s: no node has region %q", country, region, region)
		}
	}
	return nil
}

func validName(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// validHost accepts an IP address or a DNS host name.
func validHost(s string) bool {
	if _, err := netip.ParseAddr(s); err == nil {
		return true
	}
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

func validCountry(s string) bool {
	return len(s) == 2 && s[0] >= 'A' && s[0] <= 'Z' && s[1] >= 'A' && s[1] <= 'Z'
}

// Warnings lists what is valid but probably not intended.
func (c Config) Warnings(self string) []string {
	var warnings []string
	for _, n := range c.Nodes {
		if n.Address != self && n.PublicKey == "" {
			warnings = append(warnings, fmt.Sprintf("node %s has no public_key: it cannot connect, so it gets no cameras", n.Address))
		}
		if n.TokenSHA256 != "" {
			warnings = append(warnings, fmt.Sprintf("node %s: token_sha256 is no longer used (nodes log in with public_key)", n.Address))
		}
	}
	return warnings
}

// Diff describes what changes from old to new, one line per change.
func Diff(old, new Config) []string {
	old.Normalize()
	new.Normalize()
	var lines []string
	oldNodes := map[string]Node{}
	for _, n := range old.Nodes {
		oldNodes[n.Address] = n
	}
	newNodes := map[string]bool{}
	for _, n := range new.Nodes {
		newNodes[n.Address] = true
		before, ok := oldNodes[n.Address]
		switch {
		case !ok:
			lines = append(lines, "+ node "+describeNode(n))
		case describeNode(before) != describeNode(n) || before.PublicKey != n.PublicKey:
			lines = append(lines, "~ node "+describeNode(before)+"  =>  "+describeNode(n))
			if before.PublicKey != n.PublicKey {
				lines = append(lines, "~ node "+n.Address+": new public key (its link logs in again)")
			}
		}
	}
	for _, n := range old.Nodes {
		if !newNodes[n.Address] {
			lines = append(lines, "- node "+describeNode(n))
		}
	}
	countries := map[string]bool{}
	for country := range old.Routes {
		countries[country] = true
	}
	for country := range new.Routes {
		countries[country] = true
	}
	keys := make([]string, 0, len(countries))
	for country := range countries {
		keys = append(keys, country)
	}
	sort.Strings(keys)
	for _, country := range keys {
		before, hadBefore := old.Routes[country]
		after, hasAfter := new.Routes[country]
		switch {
		case !hadBefore:
			lines = append(lines, fmt.Sprintf("+ route %s -> %s", country, after))
		case !hasAfter:
			lines = append(lines, fmt.Sprintf("- route %s -> %s", country, before))
		case before != after:
			lines = append(lines, fmt.Sprintf("~ route %s -> %s  =>  %s", country, before, after))
		}
	}
	oldRelays := map[string]Relay{}
	for _, r := range old.Relays {
		oldRelays[r.Name] = r
	}
	newRelays := map[string]bool{}
	for _, r := range new.Relays {
		newRelays[r.Name] = true
		before, ok := oldRelays[r.Name]
		switch {
		case !ok:
			lines = append(lines, "+ relay "+describeRelay(r))
		case before != r:
			lines = append(lines, "~ relay "+describeRelay(before)+"  =>  "+describeRelay(r))
		}
	}
	for _, r := range old.Relays {
		if !newRelays[r.Name] {
			lines = append(lines, "- relay "+describeRelay(r))
		}
	}
	return lines
}

func describeRelay(r Relay) string {
	s := fmt.Sprintf("%s %s:%d region=%s capacity=%dMbps", r.Name, r.Host, r.Port, r.Region, r.CapacityMbps)
	if r.TCPPort != 0 {
		s += fmt.Sprintf(" tcp=%d", r.TCPPort)
	}
	if r.TLSPort != 0 {
		s += fmt.Sprintf(" tls=%s:%d", r.CertificateHost(), r.TLSPort)
	}
	if r.Metrics != "" {
		s += " metrics=" + r.Metrics
	}
	return s
}

func describeNode(n Node) string {
	s := n.Address
	if n.Name != "" {
		s = n.Name + " " + s
	}
	s += fmt.Sprintf(" region=%s capacity=%d", n.Region, n.Capacity)
	if len(n.Countries) > 0 {
		s += " countries=" + strings.Join(n.Countries, ",")
	}
	if len(n.LinkFrom) > 0 {
		s += " link_from=" + strings.Join(n.LinkFrom, ",")
	}
	return s
}

// ParsePrefix reads an IP address (as a single-address prefix) or a CIDR.
func ParsePrefix(text string) (netip.Prefix, error) {
	if addr, err := netip.ParseAddr(text); err == nil {
		addr = addr.Unmap()
		return netip.PrefixFrom(addr, addr.BitLen()), nil
	}
	prefix, err := netip.ParsePrefix(text)
	if err != nil {
		return netip.Prefix{}, err
	}
	return prefix.Masked(), nil
}
