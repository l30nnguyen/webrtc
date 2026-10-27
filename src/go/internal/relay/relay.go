// Package relay picks relay stream servers for a viewing session and mints
// their credentials.
//
// Credentials follow coturn's use-auth-secret scheme: the user name is
// "<expiry unix time>:<id>" and the password base64(HMAC-SHA1(secret, user)).
// The relay server checks both with the shared secret, so no password is
// stored anywhere and each one stops working at its expiry.
package relay

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"strconv"
	"time"

	"signaling-server/internal/fleet"
)

// CredentialLifetime is how long minted credentials work. A session keeps
// its relay allocation alive with them, so they must outlast long viewing.
const CredentialLifetime = 24 * time.Hour

// Credentials mints a user name and password for id, valid until expiry.
func Credentials(secret []byte, id string, expiry time.Time) (user, password string) {
	user = strconv.FormatInt(expiry.Unix(), 10) + ":" + id
	mac := hmac.New(sha1.New, secret)
	mac.Write([]byte(user))
	return user, base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Load reports a relay's measured load (relayed Mbps over capacity) and
// health. known is false when the relay's load is not measured.
type Load func(name string) (fraction float64, healthy, known bool)

// Choose orders the relays for one session: the best relay of region first,
// then the best of another region as a backup. A relay known to be down or
// full is skipped. Among relays with a measured load the least loaded wins;
// otherwise the pick is random, weighted by capacity, to spread sessions.
// intn(n) returns a random number in [0, n).
func Choose(relays []fleet.Relay, region string, load Load, intn func(int) int) []fleet.Relay {
	var local, other []fleet.Relay
	for _, r := range relays {
		if load != nil {
			if fraction, healthy, known := load(r.Name); known && (!healthy || fraction >= 1) {
				continue
			}
		}
		if r.Region == region {
			local = append(local, r)
		} else {
			other = append(other, r)
		}
	}
	var chosen []fleet.Relay
	if best, ok := pick(local, load, intn); ok {
		chosen = append(chosen, best)
	} else if best, ok := pick(other, load, intn); ok {
		chosen = append(chosen, best)
		other = without(other, best.Name)
	}
	if backup, ok := pick(other, load, intn); ok {
		chosen = append(chosen, backup)
	}
	return chosen
}

func pick(relays []fleet.Relay, load Load, intn func(int) int) (fleet.Relay, bool) {
	if len(relays) == 0 {
		return fleet.Relay{}, false
	}
	if load != nil {
		best, bestFraction, measured := fleet.Relay{}, 2.0, false
		for _, r := range relays {
			if fraction, _, known := load(r.Name); known && fraction < bestFraction {
				best, bestFraction, measured = r, fraction, true
			}
		}
		if measured {
			return best, true
		}
	}
	total := 0
	for _, r := range relays {
		total += capacity(r)
	}
	n := intn(total)
	for _, r := range relays {
		if n -= capacity(r); n < 0 {
			return r, true
		}
	}
	return relays[len(relays)-1], true
}

func capacity(r fleet.Relay) int {
	if r.CapacityMbps > 0 {
		return r.CapacityMbps
	}
	return fleet.DefaultRelayCapacityMbps
}

func without(relays []fleet.Relay, name string) []fleet.Relay {
	out := make([]fleet.Relay, 0, len(relays))
	for _, r := range relays {
		if r.Name != name {
			out = append(out, r)
		}
	}
	return out
}
