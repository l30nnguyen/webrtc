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
			{Name: "de00", Address: europe, Region: "DE", Capacity: 10, TokenSHA256: tokenHash("de-token")},
			{Name: "sg00", Address: asia, Region: "SG", Countries: []string{"VN"}, Capacity: 10, TokenSHA256: tokenHash("sg1-token")},
		},
		Routes: routes,
	}
}

func TestRoutesPickTheRegion(t *testing.T) {
	r, err := NewFromConfig(routedConfig(map[string]string{"it": "DE", "VN": "DE", "*": "US"}), self, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range []struct{ address, token string }{{europe, "de-token"}, {asia, "sg1-token"}} {
		if err := r.Accept(node.token, Report{Address: node.address}); err != nil {
			t.Fatal(err)
		}
	}
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
	if err := r.Accept("sg1-token", Report{Address: asia}); err != nil {
		t.Fatal(err)
	}
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
	if err := r.Accept("sg1-token", Report{Address: asia, Cameras: []string{camA}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Accept("de-token", Report{Address: europe}); err != nil {
		t.Fatal(err)
	}
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
		"missing token":        func(c *Config) { c.Nodes[1].TokenSHA256 = "" },
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
