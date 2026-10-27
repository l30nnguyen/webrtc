package relay

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"testing"
	"time"

	"signaling-server/internal/fleet"
)

func TestCredentialsMatchCoturn(t *testing.T) {
	expiry := time.Unix(1761600000, 0)
	user, password := Credentials([]byte("north"), "cam1", expiry)
	if user != "1761600000:cam1" {
		t.Fatalf("user %q", user)
	}
	mac := hmac.New(sha1.New, []byte("north"))
	mac.Write([]byte(user))
	if password != base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
		t.Fatal("password is not base64(HMAC-SHA1(secret, user))")
	}
}

var relays = []fleet.Relay{
	{Name: "de1", Region: "DE", CapacityMbps: 100},
	{Name: "de2", Region: "DE", CapacityMbps: 300},
	{Name: "us1", Region: "US", CapacityMbps: 100},
}

func names(rs []fleet.Relay) string {
	s := ""
	for _, r := range rs {
		s += r.Name + " "
	}
	return s
}

func TestChooseRegionFirstWithABackupElsewhere(t *testing.T) {
	first := func(int) int { return 0 }
	if got := names(Choose(relays, "DE", nil, first)); got != "de1 us1 " {
		t.Fatalf("DE = %s", got)
	}
	// Weighted by capacity: 0..99 de1, 100..399 de2.
	if got := names(Choose(relays, "DE", nil, func(int) int { return 150 })); got != "de2 us1 " {
		t.Fatalf("DE weighted = %s", got)
	}
	// No relay in the region: two from elsewhere, different ones.
	if got := names(Choose(relays, "SG", nil, first)); got != "de1 de2 " {
		t.Fatalf("SG = %s", got)
	}
	if len(Choose(nil, "DE", nil, first)) != 0 {
		t.Fatal("relays from nothing")
	}
}

func TestChooseByMeasuredLoad(t *testing.T) {
	load := func(name string) (float64, bool, bool) {
		switch name {
		case "de1":
			return 0.2, true, true
		case "de2":
			return 0.9, true, true
		case "us1":
			return 0, false, true // down
		}
		return 0, false, false
	}
	if got := names(Choose(relays, "DE", load, func(int) int { return 0 })); got != "de1 " {
		t.Fatalf("DE by load = %s", got)
	}
	full := func(name string) (float64, bool, bool) { return 1.0, true, name != "us1" }
	// de1 and de2 full: us1 (load unknown) is all there is.
	if got := names(Choose(relays, "DE", full, func(int) int { return 0 })); got != "us1 " {
		t.Fatalf("DE full = %s", got)
	}
}
