package fleet

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
}

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
		if n.Address != self {
			if raw, err := hex.DecodeString(n.TokenSHA256); err != nil || len(raw) != sha256.Size {
				return fmt.Errorf("node %s needs token_sha256 (64 hex characters)", n.Address)
			}
		}
		if n.Region != "" {
			regions[n.Region] = true
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

func validCountry(s string) bool {
	return len(s) == 2 && s[0] >= 'A' && s[0] <= 'Z' && s[1] >= 'A' && s[1] <= 'Z'
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
		case describeNode(before) != describeNode(n) || before.TokenSHA256 != n.TokenSHA256:
			lines = append(lines, "~ node "+describeNode(before)+"  =>  "+describeNode(n))
			if before.TokenSHA256 != n.TokenSHA256 {
				lines = append(lines, "~ node "+n.Address+": new report token")
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
	return lines
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
	return s
}
