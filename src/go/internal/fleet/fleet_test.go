package fleet

import (
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
	"strings"
	"testing"
	"time"
)

const (
	self  = "wss://center.example.test:443/"
	asia  = "wss://sg1.example.test:443/"
	asia2 = "wss://sg2.example.test:443/"
)

var camA = strings.Repeat("A", 43)
var camB = strings.Repeat("B", 43)

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func testRegistry(t *testing.T, local *Report) (*Registry, *clock) {
	t.Helper()
	c := &clock{time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)}
	r, err := New([]Node{
		{Address: self, Region: "us", Countries: []string{"us"}, Capacity: 10},
		{Address: asia, Region: "sg", Countries: []string{"VN", "SG"}, Capacity: 10, TokenSHA256: tokenHash("sg1-token")},
		{Address: asia2, Region: "sg", Countries: []string{"VN"}, Capacity: 10, TokenSHA256: tokenHash("sg2-token")},
	}, self, func() Report { return *local }, c.now)
	if err != nil {
		t.Fatal(err)
	}
	return r, c
}

func TestAssignByCountryLoadAndHealth(t *testing.T) {
	local := &Report{Address: self}
	r, c := testRegistry(t, local)

	// Remote nodes have not reported yet: everyone goes to the Center node.
	if got := r.Assign("", "VN"); got != self {
		t.Fatalf("before any report = %s", got)
	}
	if err := r.Accept("sg1-token", Report{Address: asia, Cameras: []string{camA}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Accept("sg2-token", Report{Address: asia2}); err != nil {
		t.Fatal(err)
	}
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
	// sg1 stops reporting: it is skipped and its cameras count as offline.
	c.t = c.t.Add(StaleAfter + time.Second)
	if err := r.Accept("sg2-token", Report{Address: asia2}); err != nil {
		t.Fatal(err)
	}
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
	_ = r.Accept("sg2-token", Report{Address: asia2})
	for i := 0; i < 10; i++ { // capacity 10, counted through pending assignments
		if got := r.Assign("", "US"); got != self {
			t.Fatalf("assignment %d = %s", i, got)
		}
	}
	if got := r.Assign("", "US"); got != asia2 {
		t.Fatalf("Center node full, got %s", got)
	}
}

func TestReportsNeedTheNodeToken(t *testing.T) {
	local := &Report{Address: self}
	r, _ := testRegistry(t, local)
	for _, tc := range []struct{ token, address string }{
		{"sg2-token", asia}, {"", asia}, {"x", "wss://unknown.example.test/"}, {"", self},
	} {
		if err := r.Accept(tc.token, Report{Address: tc.address}); err != ErrUnauthorized {
			t.Fatalf("report %+v accepted: %v", tc, err)
		}
	}
}

func TestLocateFollowsReportsAndTheCenterNode(t *testing.T) {
	local := &Report{Address: self, Cameras: []string{camB}}
	r, _ := testRegistry(t, local)
	if node, online := r.Locate(camB); !online || node != self {
		t.Fatalf("camera on the Center node = %s %v", node, online)
	}
	_ = r.Accept("sg1-token", Report{Address: asia, Cameras: []string{camA}})
	if node, online := r.Locate(camA); !online || node != asia {
		t.Fatalf("camera on sg1 = %s %v", node, online)
	}
	_ = r.Accept("sg1-token", Report{Address: asia}) // camA left sg1
	if _, online := r.Locate(camA); online {
		t.Fatal("camera that left is still located")
	}
}

func TestNodesFileValidation(t *testing.T) {
	for name, nodes := range map[string][]Node{
		"bad address": {{Address: "https://x.example.test/"}},
		"no token":    {{Address: asia}},
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
