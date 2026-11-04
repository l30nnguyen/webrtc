package center

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"signaling-server/internal/activation"
	"signaling-server/internal/fleet"
	"signaling-server/internal/sid"
)

type takeoverEnv struct {
	*env
	clock     *time.Time
	link      uint64
	takeovers []string
}

// newTakeoverEnv is a Center with a fleet of its own node and remoteNode
// (linked, with no cameras), the fleet and Center on one clock.
func newTakeoverEnv(t *testing.T, mutate ...func(*Config)) *takeoverEnv {
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	te := &takeoverEnv{clock: &now}
	clock := func() time.Time { return *te.clock }
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	registry, err := fleet.New([]fleet.Node{
		{Address: mainAddress, Capacity: 100},
		{Address: remoteNode, Capacity: 100, PublicKey: base64.RawURLEncoding.EncodeToString(key)},
	}, mainAddress, func() fleet.Report { return fleet.Report{Address: mainAddress} }, clock)
	if err != nil {
		t.Fatal(err)
	}
	te.env = newEnv(t, append([]func(*Config){func(c *Config) {
		c.Fleet, c.Now = registry, clock
		c.OnTakeover = func(tenant, from, to string) { te.takeovers = append(te.takeovers, from+">"+to) }
	}}, mutate...)...)
	te.env.fleet = registry
	if te.link, err = registry.Linked(fleet.Report{Address: remoteNode}, 0); err != nil {
		t.Fatal(err)
	}
	return te
}

func (te *takeoverEnv) wait(d time.Duration) { *te.clock = te.clock.Add(d) }

// online puts a camera on, or takes it off, the remote node.
func (te *takeoverEnv) online(d device, on bool) {
	te.fleet.Camera(remoteNode, te.link, activation.DeviceID(d.public), on)
}

func (te *takeoverEnv) expect(d device, sidText string, want int) {
	te.t.Helper()
	if response, _ := te.verify(d, sidText); response.Code != want {
		te.t.Fatalf("verify = %d %s, want %d; logs=%s", response.Code, response.Body.String(), want, te.logs.joined())
	}
}

func TestTakeoverOfAnOfflineCamerasSID(t *testing.T) {
	te := newTakeoverEnv(t)
	first, second := newDevice(), newDevice()
	sidText := te.issue("acme01", sid.FlagPublisher, te.now.Add(-time.Hour))
	te.mustVerify(first, sidText)
	te.online(first, true)

	// Online: the SID stays with its camera.
	te.wait(time.Hour)
	te.expect(second, sidText, http.StatusUnauthorized)
	if !strings.Contains(te.logs.joined(), "ALERT SID") || !strings.Contains(te.logs.joined(), "online") {
		t.Fatalf("no alert for a SID in use; logs=%s", te.logs.joined())
	}

	// Offline for less than the takeover time: retry.
	te.online(first, false)
	te.wait(DefaultTakeoverAfter - time.Second)
	te.expect(second, sidText, http.StatusServiceUnavailable)

	// Then the new camera takes it over, and the old key is out for good.
	te.wait(time.Second)
	te.mustVerify(second, sidText)
	record, _ := te.store.Get(Digest(sidText))
	if record.DeviceID != activation.DeviceID(second.public) || !record.Held(activation.DeviceID(first.public)) {
		t.Fatalf("record after takeover = %+v", record)
	}
	if len(te.takeovers) != 1 || te.takeovers[0] != activation.DeviceID(first.public)+">"+activation.DeviceID(second.public) {
		t.Fatalf("takeovers reported = %v", te.takeovers)
	}
	blocklist, _ := os.ReadFile(te.blocklistPath)
	if !strings.Contains(string(blocklist), "device:"+activation.DeviceID(first.public)) {
		t.Fatalf("old device not revoked; blocklist=%q", blocklist)
	}
	te.mustVerify(second, sidText)
	te.wait(time.Hour)
	te.expect(first, sidText, http.StatusUnauthorized)
}

func TestTakeoverWaitsWhileANodeIsDown(t *testing.T) {
	te := newTakeoverEnv(t)
	first, second := newDevice(), newDevice()
	sidText := te.issue("acme01", sid.FlagPublisher, te.now.Add(-time.Hour))
	te.mustVerify(first, sidText)
	te.online(first, true)

	// The node's link drops: the camera may still be on it.
	te.fleet.Unlinked(remoteNode, te.link)
	te.wait(time.Hour)
	te.expect(second, sidText, http.StatusServiceUnavailable)

	// The node is back without the camera: it left now at the latest.
	var err error
	if te.link, err = te.fleet.Linked(fleet.Report{Address: remoteNode}, 0); err != nil {
		t.Fatal(err)
	}
	te.wait(DefaultTakeoverAfter - time.Second)
	te.expect(second, sidText, http.StatusServiceUnavailable)
	te.wait(time.Second)
	te.mustVerify(second, sidText)
}

func TestTakeoverOfACameraNotSeenSinceStart(t *testing.T) {
	te := newTakeoverEnv(t)
	first, second := newDevice(), newDevice()
	sidText := te.issue("acme01", sid.FlagPublisher, te.now.Add(-time.Hour))
	te.mustVerify(first, sidText) // never connects to a node
	te.expect(second, sidText, http.StatusServiceUnavailable)
	te.wait(DefaultTakeoverAfter)
	te.mustVerify(second, sidText)
}

func TestNoTakeoverWhenNotAllowed(t *testing.T) {
	for name, test := range map[string]struct {
		tenant string
		flags  uint16
		mutate func(*Config)
	}{
		"tenant without takeover": {tenant: "strict01", flags: sid.FlagPublisher},
		"turned off":              {tenant: "acme01", flags: sid.FlagPublisher, mutate: func(c *Config) { c.TakeoverAfter = -1 }},
		"player SID":              {tenant: "acme01", flags: sid.FlagViewer},
	} {
		t.Run(name, func(t *testing.T) {
			mutate := []func(*Config){func(c *Config) {
				c.Tenants = StaticTenants{"acme01": {}, "strict01": {NoTakeover: true}}
			}}
			if test.mutate != nil {
				mutate = append(mutate, test.mutate)
			}
			te := newTakeoverEnv(t, mutate...)
			first, second := newDevice(), newDevice()
			sidText := te.issue(test.tenant, test.flags, te.now.Add(-time.Hour))
			te.mustVerify(first, sidText)
			te.wait(time.Hour)
			te.expect(second, sidText, http.StatusUnauthorized)
			if len(te.takeovers) != 0 {
				t.Fatalf("takeovers = %v", te.takeovers)
			}
		})
	}
}

func TestReplaceNeedsTheCurrentHolder(t *testing.T) {
	store, err := activation.Open(t.TempDir() + "/activations.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Unix(1_800_000_000, 0)
	digest := strings.Repeat("ab", 32)
	store.Bind(activation.Record{Digest: digest, Tenant: "acme01", Role: "camera", DeviceID: "a", DevicePub: "pa"}, now)
	if _, ok, _ := store.Replace(activation.Record{Digest: digest, Tenant: "acme01", Role: "camera", DeviceID: "c", DevicePub: "pc"}, "b", now); ok {
		t.Fatal("replaced a SID held by another device")
	}
	got, ok, err := store.Replace(activation.Record{Digest: digest, Tenant: "acme01", Role: "camera", DeviceID: "b", DevicePub: "pb"}, "a", now)
	if err != nil || !ok || got.DeviceID != "b" || len(got.Former) != 1 || got.Former[0] != "a" {
		t.Fatalf("replace = %+v %v %v", got, ok, err)
	}
	if _, result, _ := store.Bind(activation.Record{Digest: digest, Tenant: "acme01", Role: "camera", DeviceID: "a", DevicePub: "pa"}, now); result != activation.BindExisting {
		t.Fatalf("bind after replace = %v", result)
	}
}
