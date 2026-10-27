package fleet

import (
	"strings"
	"testing"
)

const europe = "wss://de1.example.test:443/"

func routedConfig(routes map[string]string) Config {
	return Config{
		Nodes: []Node{
			{Name: "us00", Address: self, Region: "US", Capacity: 10},
			{Name: "de00", Address: europe, Region: "DE", Capacity: 10, PublicKey: testKey(3)},
			{Name: "sg00", Address: asia, Region: "SG", Countries: []string{"VN"}, Capacity: 10, PublicKey: testKey(1)},
		},
		Routes: routes,
	}
}

func TestRoutesPickTheRegion(t *testing.T) {
	r, err := NewFromConfig(routedConfig(map[string]string{"it": "DE", "VN": "DE", "*": "US"}), self, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	linked(t, r, Report{Address: europe})
	linked(t, r, Report{Address: asia})
	cases := map[string]string{
		"IT": europe, // routes, lower-case key normalized
		"VN": europe, // routes win over the node's own country list
		"FR": self,   // "*"
		"":   self,   // unknown country: "*" too
	}
	for country, want := range cases {
		if got := r.Assign("", country); got != want {
			t.Errorf("Assign(%q) = %s, want %s", country, got, want)
		}
	}
}

func TestNodeCountriesStillWorkWithoutARoute(t *testing.T) {
	r, err := NewFromConfig(routedConfig(map[string]string{"*": "US"}), self, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	linked(t, r, Report{Address: asia})
	if got := r.Assign("", "VN"); got != asia {
		t.Fatalf("VN = %s", got)
	}
}

func TestRoutedRegionDownFallsBackToTheFleet(t *testing.T) {
	r, err := NewFromConfig(routedConfig(map[string]string{"IT": "DE"}), self, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// de00 never reported, so it is down.
	if got := r.Assign("", "IT"); got != self {
		t.Fatalf("IT with DE down = %s", got)
	}
}

func TestApplyKeepsReportedStateAndMovesOnlyNewConnections(t *testing.T) {
	r, err := NewFromConfig(routedConfig(map[string]string{"IT": "SG"}), self, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	linked(t, r, Report{Address: asia, Cameras: []string{camA}})
	linked(t, r, Report{Address: europe})
	next := routedConfig(map[string]string{"IT": "DE"})
	next.Version = 2
	if err := r.Apply(next); err != nil {
		t.Fatal(err)
	}
	if r.Version() != 2 {
		t.Fatalf("version = %d", r.Version())
	}
	// The connected camera is still found where it is.
	if node, online := r.Locate(camA); !online || node != asia {
		t.Fatalf("Locate = %s %v", node, online)
	}
	// Its next connection follows the new rule.
	if got := r.Assign(camA, "IT"); got != europe {
		t.Fatalf("reconnect = %s", got)
	}

	// A node removed from the config is forgotten, with its cameras.
	without := routedConfig(nil)
	without.Nodes = without.Nodes[:2]
	if err := r.Apply(without); err != nil {
		t.Fatal(err)
	}
	if _, online := r.Locate(camA); online {
		t.Fatal("camera of a removed node still located")
	}
	if r.Known(asia) {
		t.Fatal("removed node still known")
	}
}

func TestInvalidConfigChangesNothing(t *testing.T) {
	r, err := NewFromConfig(routedConfig(map[string]string{"IT": "DE"}), self, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	bad := routedConfig(map[string]string{"IT": "FR"})
	bad.Version = 9
	if err := r.Apply(bad); err == nil {
		t.Fatal("route to a region without nodes accepted")
	}
	if r.Version() != 0 || !r.Known(europe) {
		t.Fatal("rejected config changed the registry")
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]func(*Config){
		"three-letter country": func(c *Config) { c.Routes = map[string]string{"ITA": "DE"} },
		"route to no region":   func(c *Config) { c.Routes = map[string]string{"IT": "GER"} },
		"duplicate name":       func(c *Config) { c.Nodes[1].Name = "us00" },
		"bad name":             func(c *Config) { c.Nodes[1].Name = "EU 00" },
		"bad public key":       func(c *Config) { c.Nodes[1].PublicKey = "AAAA" },
		"bad node country":     func(c *Config) { c.Nodes[2].Countries = []string{"Viet"} },
		"duplicate address":    func(c *Config) { c.Nodes[2].Address = europe },
		"address with path":    func(c *Config) { c.Nodes[1].Address = "wss://de1.example.test/x" },
		"negative capacity":    func(c *Config) { c.Nodes[1].Capacity = -1 },
		"region with a space":  func(c *Config) { c.Nodes[1].Region = "D E" },
	}
	for name, mutate := range cases {
		c := routedConfig(nil)
		mutate(&c)
		if err := c.Validate(self); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := routedConfig(map[string]string{"IT": "DE", "*": "US"}).Validate(self); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestParseConfigIsStrict(t *testing.T) {
	if _, err := ParseConfig([]byte(`{"nodes":[],"route":{"IT":"DE"}}`)); err == nil {
		t.Fatal("misspelled key accepted")
	}
	if _, err := ParseConfig([]byte(`{"nodes":[]} {}`)); err == nil {
		t.Fatal("trailing data accepted")
	}
	c, err := ParseConfig([]byte(`{"nodes":[{"address":"` + self + `","region":"US"}],"routes":{"*":"US"}}`))
	if err != nil || len(c.Nodes) != 1 || c.Routes["*"] != "US" {
		t.Fatalf("ParseConfig = %+v, %v", c, err)
	}
}

func TestDiff(t *testing.T) {
	old := routedConfig(map[string]string{"IT": "SG", "FR": "DE"})
	next := routedConfig(map[string]string{"IT": "DE", "*": "US"})
	next.Nodes = next.Nodes[:2]
	next.Nodes[1].Capacity = 20
	got := strings.Join(Diff(old, next), "\n")
	for _, want := range []string{
		"~ node de00 " + europe + " region=DE capacity=10  =>  de00 " + europe + " region=DE capacity=20",
		"- node sg00 " + asia,
		"+ route * -> US",
		"- route FR -> DE",
		"~ route IT -> SG  =>  DE",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diff misses %q:\n%s", want, got)
		}
	}
}

func TestMovesCountsCamerasANewConfigPlacesElsewhere(t *testing.T) {
	camC := strings.Repeat("C", 43)
	r, err := NewFromConfig(routedConfig(map[string]string{"IT": "SG", "*": "US"}), self, func() Report {
		return Report{Address: self, Cameras: []string{camC}}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	linked(t, r, Report{Address: asia, Cameras: []string{camA, camB}})
	r.Placed(camA, "it") // Italian camera on the SG node
	r.Placed(camB, "VN") // Vietnamese camera on the SG node (node country list)
	r.Placed(camC, "US") // camC on the Center node
	r.Placed("not-a-device", "IT")

	moved, online, unknown, err := r.Moves(routedConfig(map[string]string{"IT": "DE", "*": "US"}))
	if err != nil || moved != 1 || online != 3 || unknown != 0 {
		t.Fatalf("IT -> DE: moved %d online %d unknown %d %v", moved, online, unknown, err)
	}
	// Removing the SG node moves both of its cameras.
	without := routedConfig(map[string]string{"*": "US"})
	without.Nodes = without.Nodes[:2]
	if moved, _, _, _ := r.Moves(without); moved != 2 {
		t.Fatalf("SG removed: moved %d", moved)
	}
	if _, _, _, err := r.Moves(routedConfig(map[string]string{"IT": "XX"})); err == nil {
		t.Fatal("invalid config counted")
	}
}

func TestWarnings(t *testing.T) {
	c := routedConfig(nil)
	c.Nodes[1].PublicKey = ""
	c.Nodes[2].TokenSHA256 = strings.Repeat("ab", 32)
	got := strings.Join(c.Warnings(self), "\n")
	if !strings.Contains(got, europe+" has no public_key") || !strings.Contains(got, "token_sha256 is no longer used") {
		t.Fatalf("warnings:\n%s", got)
	}
	if len(routedConfig(nil).Warnings(self)) != 0 {
		t.Fatal("warnings for a clean config")
	}
}

func TestRelays(t *testing.T) {
	c := routedConfig(nil)
	c.Relays = []Relay{{Name: "turn-de", Region: "DE", Host: "turn-de.example.test", Metrics: "http://10.10.0.5:9641/metrics"}}
	if err := c.Validate(self); err != nil {
		t.Fatal(err)
	}
	c.Normalize()
	if c.Relays[0].Port != DefaultRelayPort || c.Relays[0].CapacityMbps != DefaultRelayCapacityMbps {
		t.Fatalf("defaults not filled: %+v", c.Relays[0])
	}
	for name, relay := range map[string]Relay{
		"bad name":     {Name: "Turn DE", Region: "DE", Host: "h.example"},
		"no region":    {Name: "t", Host: "h.example"},
		"bad host":     {Name: "t", Region: "DE", Host: "h_x.example"},
		"url as host":  {Name: "t", Region: "DE", Host: "turn:h.example"},
		"bad port":     {Name: "t", Region: "DE", Host: "h.example", Port: 70000},
		"bad metrics":  {Name: "t", Region: "DE", Host: "h.example", Metrics: "ftp://h/metrics"},
		"negative cap": {Name: "t", Region: "DE", Host: "h.example", CapacityMbps: -1},
	} {
		bad := routedConfig(nil)
		bad.Relays = []Relay{relay}
		if err := bad.Validate(self); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	twice := routedConfig(nil)
	twice.Relays = []Relay{{Name: "t", Region: "DE", Host: "a.example"}, {Name: "t", Region: "DE", Host: "b.example"}}
	if twice.Validate(self) == nil {
		t.Error("duplicate relay name accepted")
	}
	next := c
	next.Relays = []Relay{{Name: "turn-de", Region: "DE", Host: "turn-de2.example.test"}}
	if got := strings.Join(Diff(c, next), "\n"); !strings.Contains(got, "~ relay turn-de turn-de.example.test:3478") {
		t.Fatalf("diff:\n%s", got)
	}
}
