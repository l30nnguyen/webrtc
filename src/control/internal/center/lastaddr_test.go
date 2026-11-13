package center

import (
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"signaling-server/internal/sid"
)

func TestSameNetwork(t *testing.T) {
	for _, test := range []struct {
		a, b string
		want bool
	}{
		{"203.0.113.5", "203.0.113.200", true},
		{"203.0.113.5", "203.0.114.5", false},
		{"::ffff:203.0.113.5", "203.0.113.9", true},
		{"2001:db8:1:2::1", "2001:db8:1:2:ffff::9", true},
		{"2001:db8:1:2::1", "2001:db8:1:3::1", false},
		{"203.0.113.5", "2001:db8::1", false},
	} {
		if got := sameNetwork(netip.MustParseAddr(test.a), netip.MustParseAddr(test.b)); got != test.want {
			t.Errorf("sameNetwork(%s, %s) = %v, want %v", test.a, test.b, got, test.want)
		}
	}
	if sameNetwork(netip.Addr{}, netip.MustParseAddr("203.0.113.5")) {
		t.Error("an unknown address must not count as the same network")
	}
}

func TestLastAddressesPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "last_addresses.json")
	l, err := OpenLastAddresses(path)
	if err != nil {
		t.Fatal(err)
	}
	l.Set("dev1", netip.MustParseAddr("::ffff:198.51.100.7"))
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, %v", info, err)
	}
	again, err := OpenLastAddresses(path)
	if err != nil {
		t.Fatal(err)
	}
	if addr, ok := again.Get("dev1"); !ok || addr != netip.MustParseAddr("198.51.100.7") {
		t.Fatalf("after reopen: %v %v", addr, ok)
	}

	// A broken file starts empty and is replaced by the next save.
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken, err := OpenLastAddresses(path)
	if err == nil || broken == nil {
		t.Fatalf("broken file: store %v, err %v", broken, err)
	}
	broken.Set("dev2", netip.MustParseAddr("192.0.2.9"))
	if err := broken.Save(); err != nil {
		t.Fatal(err)
	}
	if fixed, err := OpenLastAddresses(path); err != nil {
		t.Fatalf("file not replaced: %v", err)
	} else if _, ok := fixed.Get("dev2"); !ok {
		t.Fatal("dev2 missing after save")
	}
}

// takeOverFrom binds a SID on firstAddr, lets the camera go offline long
// enough, and takes it over from secondAddr.
func takeOverFrom(t *testing.T, firstAddr, secondAddr string) *takeoverEnv {
	te := newTakeoverEnv(t)
	first, second := newDevice(), newDevice()
	sidText := te.issue("acme01", sid.FlagPublisher, te.now.Add(-time.Hour))
	te.remoteAddr = firstAddr
	te.mustVerify(first, sidText)
	te.wait(DefaultTakeoverAfter)
	te.remoteAddr = secondAddr
	te.expect(second, sidText, http.StatusOK)
	if len(te.infos) != 1 {
		t.Fatalf("takeovers = %v", te.infos)
	}
	return te
}

func TestTakeoverOnTheSameNetworkIsNoAlert(t *testing.T) {
	te := takeOverFrom(t, "203.0.113.5:4000", "203.0.113.77:5000")
	info := te.infos[0]
	if !info.SameNetwork || info.OldAddress != netip.MustParseAddr("203.0.113.5") || info.NewAddress != netip.MustParseAddr("203.0.113.77") {
		t.Fatalf("info = %+v", info)
	}
	if logs := te.logs.joined(); strings.Contains(logs, "ALERT") || !strings.Contains(logs, "same network") {
		t.Fatalf("logs = %s", logs)
	}
}

func TestTakeoverFromAnotherNetworkAlerts(t *testing.T) {
	te := takeOverFrom(t, "203.0.113.5:4000", "198.51.100.8:5000")
	info := te.infos[0]
	if info.SameNetwork || info.OldAddress != netip.MustParseAddr("203.0.113.5") || info.NewAddress != netip.MustParseAddr("198.51.100.8") {
		t.Fatalf("info = %+v", info)
	}
	logs := te.logs.joined()
	if !strings.Contains(logs, "ALERT SID") || !strings.Contains(logs, "another network: before 203.0.113.5, now 198.51.100.8") {
		t.Fatalf("logs = %s", logs)
	}
}

func TestTakeoverWithUnknownOldAddressAlerts(t *testing.T) {
	te := takeOverFrom(t, "not-an-address", "198.51.100.8:5000")
	if te.infos[0].SameNetwork || te.infos[0].OldAddress.IsValid() {
		t.Fatalf("info = %+v", te.infos[0])
	}
	if logs := te.logs.joined(); !strings.Contains(logs, "before unknown, now 198.51.100.8") {
		t.Fatalf("logs = %s", logs)
	}
}
