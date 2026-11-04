package fleet

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"net/netip"
	"strings"
	"testing"
	"time"

	"signaling-server/internal/host"
)

const (
	self  = "wss://center.example.test:443/"
	asia  = "wss://sg1.example.test:443/"
	asia2 = "wss://sg2.example.test:443/"
)

var camA = strings.Repeat("A", 43)
var camB = strings.Repeat("B", 43)

// testKey is a node's link public key, as config.json carries it.
func testKey(seed byte) string {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
	return base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
}

// linked logs a node in with its full state and returns the link.
func linked(t *testing.T, r *Registry, report Report) uint64 {
	t.Helper()
	link, err := r.Linked(report, 0)
	if err != nil {
		t.Fatal(err)
	}
	return link
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func testRegistry(t *testing.T, local *Report) (*Registry, *clock) {
	t.Helper()
	c := &clock{time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)}
	r, err := New([]Node{
		{Address: self, Region: "us", Countries: []string{"us"}, Capacity: 10},
		{Address: asia, Region: "sg", Countries: []string{"VN", "SG"}, Capacity: 10, PublicKey: testKey(1)},
		{Address: asia2, Region: "sg", Countries: []string{"VN"}, Capacity: 10, PublicKey: testKey(2)},
	}, self, func() Report { return *local }, c.now)
	if err != nil {
		t.Fatal(err)
	}
	return r, c
}

func TestAssignByCountryLoadAndHealth(t *testing.T) {
	local := &Report{Address: self}
	r, _ := testRegistry(t, local)

	// Remote nodes have not linked yet: everyone goes to the Center node.
	if got := r.Assign("", "VN"); got != self {
		t.Fatalf("before any report = %s", got)
	}
	sg1 := linked(t, r, Report{Address: asia, Cameras: []string{camA}})
	linked(t, r, Report{Address: asia2})
	// Vietnam prefers the regional nodes; sg2 is empty, so it wins.
	if got := r.Assign("", "VN"); got != asia2 {
		t.Fatalf("VN = %s", got)
	}
	// A camera already on sg1 stays there.
	if got := r.Assign(camA, "VN"); got != asia {
		t.Fatalf("sticky = %s", got)
	}
	// Unknown country: least loaded of the whole fleet.
	if got := r.Assign("", ""); got != self {
		t.Fatalf("no country = %s", got)
	}
	// sg1's link ends: it is skipped and its cameras count as offline.
	r.Unlinked(asia, sg1)
	if got := r.Assign(camA, "SG"); got == asia {
		t.Fatal("assigned to a stale node")
	}
	if _, online := r.Locate(camA); online {
		t.Fatal("camera on a stale node still located")
	}
}

func TestFullNodesAreSkipped(t *testing.T) {
	local := &Report{Address: self}
	r, _ := testRegistry(t, local)
	linked(t, r, Report{Address: asia2})
	for i := 0; i < 10; i++ { // capacity 10, counted through pending assignments
		if got := r.Assign("", "US"); got != self {
			t.Fatalf("assignment %d = %s", i, got)
		}
	}
	if got := r.Assign("", "US"); got != asia2 {
		t.Fatalf("Center node full, got %s", got)
	}
}

func TestOnlyListedNodesLink(t *testing.T) {
	local := &Report{Address: self}
	r, _ := testRegistry(t, local)
	for _, address := range []string{"wss://unknown.example.test/", self} {
		if _, err := r.Linked(Report{Address: address}, 0); err != ErrUnknownNode {
			t.Fatalf("link from %s: %v", address, err)
		}
	}
	if _, ok := r.LinkKey(self); ok {
		t.Fatal("the Center node has a link key")
	}
	if key, ok := r.LinkKey(asia); !ok || base64.RawURLEncoding.EncodeToString(key) != testKey(1) {
		t.Fatal("sg1 key not found")
	}
}

func TestStaleLinkCallsAreIgnored(t *testing.T) {
	local := &Report{Address: self}
	r, _ := testRegistry(t, local)
	old := linked(t, r, Report{Address: asia})
	current := linked(t, r, Report{Address: asia}) // reconnected
	r.Camera(asia, old, camA, true)
	if _, online := r.Locate(camA); online {
		t.Fatal("a replaced link added a camera")
	}
	r.Unlinked(asia, old)
	if !r.Current(asia, current) {
		t.Fatal("the old link's end dropped the new link")
	}
	r.Camera(asia, current, camA, true)
	if node, online := r.Locate(camA); !online || node != asia {
		t.Fatal("camera event on the live link lost")
	}
	r.ConfigAck(asia, current, 4)
	for _, status := range r.Status() {
		if status.Address == asia && (status.ConfigVersion != 4 || status.Cameras != 1 || !status.Healthy) {
			t.Fatalf("status %+v", status)
		}
	}
}

func TestANewKeyDropsTheLink(t *testing.T) {
	local := &Report{Address: self}
	r, _ := testRegistry(t, local)
	link := linked(t, r, Report{Address: asia, Cameras: []string{camA}})
	config := r.Config()
	config.Nodes[1].PublicKey = testKey(9)
	if err := r.Apply(config); err != nil {
		t.Fatal(err)
	}
	if r.Current(asia, link) {
		t.Fatal("link survived a key change")
	}
	if _, online := r.Locate(camA); online {
		t.Fatal("cameras of a dropped link still located")
	}
}

func TestLocateFollowsReportsAndTheCenterNode(t *testing.T) {
	local := &Report{Address: self, Cameras: []string{camB}}
	r, _ := testRegistry(t, local)
	if node, online := r.Locate(camB); !online || node != self {
		t.Fatalf("camera on the Center node = %s %v", node, online)
	}
	sg1 := linked(t, r, Report{Address: asia, Cameras: []string{camA}})
	if node, online := r.Locate(camA); !online || node != asia {
		t.Fatalf("camera on sg1 = %s %v", node, online)
	}
	r.Camera(asia, sg1, camA, false) // camA left sg1
	if _, online := r.Locate(camA); online {
		t.Fatal("camera that left is still located")
	}
}

func TestNodesFileValidation(t *testing.T) {
	for name, nodes := range map[string][]Node{
		"bad address": {{Address: "https://x.example.test/"}},
		"bad key":     {{Address: asia, PublicKey: "short"}},
		"self key":    {{Address: self, PublicKey: testKey(1)}},
		"twice":       {{Address: self}, {Address: self}},
	} {
		if _, err := New(nodes, self, nil, nil); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestCountryRanges(t *testing.T) {
	c, err := ReadCountries(strings.NewReader("1.0.0.0,1.0.0.255,AU\n14.160.0.0,14.191.255.255,VN\n2001:ee0::,2001:ee0:ffff:ffff:ffff:ffff:ffff:ffff,VN\n3.0.0.0,3.255.255.255,ZZZ\n"))
	if err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]string{
		"14.161.1.2": "VN", "1.0.0.7": "AU", "1.0.1.0": "", "2001:ee0::5": "VN", "::ffff:14.161.1.2": "VN", "3.1.1.1": "",
	} {
		if got := c.Lookup(netip.MustParseAddr(ip)); got != want {
			t.Fatalf("%s = %q, want %q", ip, got, want)
		}
	}
}

func TestRelaysMergeTheLatestMeasurement(t *testing.T) {
	local := &Report{Address: self}
	r, _ := testRegistry(t, local)
	config := r.Config()
	config.Relays = []Relay{{Name: "sg-relay", Region: "sg", Host: "relay.example.test"}, {Name: "us-relay", Region: "us", Host: "us.example.test"}}
	if err := r.Apply(config); err != nil {
		t.Fatal(err)
	}
	link1 := linked(t, r, Report{Address: asia})
	link2 := linked(t, r, Report{Address: asia2})
	r.NodeStats(asia, link1, 0, nil, []RelayStatus{{Name: "sg-relay", Healthy: false, At: "2026-10-27T00:00:00Z"}}, nil, "")
	r.NodeStats(asia2, link2, 0, nil, []RelayStatus{{Name: "sg-relay", Healthy: true, Mbps: 5, At: "2026-10-27T00:00:15Z"}}, nil, "")
	got := r.Relays(nil)
	if len(got) != 2 || got[0].Name != "sg-relay" || !got[0].Healthy || got[0].MeasuredBy != asia2 {
		t.Fatalf("relays %+v", got)
	}
	if got[1].Name != "us-relay" || got[1].At != "" {
		t.Fatalf("unmeasured relay %+v", got[1])
	}
	// A node that goes down no longer counts.
	r.Unlinked(asia2, link2)
	if got := r.Relays(nil); got[0].MeasuredBy != asia || got[0].Healthy {
		t.Fatalf("after unlink %+v", got[0])
	}
}

func TestStatusShowsTheMachineOfLinkedNodesOnly(t *testing.T) {
	r, _ := testRegistry(t, &Report{Address: self})
	link := linked(t, r, Report{Address: asia, Version: "2.6.0", Machine: &host.Load{CPU: 12, MemMB: 961}})
	find := func() NodeStatus {
		for _, s := range r.Status() {
			if s.Address == asia {
				return s
			}
		}
		return NodeStatus{}
	}
	if s := find(); s.Version != "2.6.0" || s.Machine == nil || s.Machine.CPU != 12 {
		t.Fatalf("after hello: %+v", s)
	}
	// A stats message without a measurement keeps the last one.
	r.NodeStats(asia, link, 3, nil, nil, nil, "")
	if s := find(); s.Clients != 3 || s.Machine == nil || s.Version != "2.6.0" {
		t.Fatalf("after stats: %+v", s)
	}
	r.NodeStats(asia, link, 3, nil, nil, &host.Load{CPU: 80}, "2.6.1")
	if s := find(); s.Machine.CPU != 80 || s.Version != "2.6.1" {
		t.Fatalf("after new stats: %+v", s)
	}
	// Down: nothing stale is shown.
	r.Unlinked(asia, link)
	if s := find(); s.Healthy || s.Machine != nil || s.Version != "" {
		t.Fatalf("down: %+v", s)
	}
}
