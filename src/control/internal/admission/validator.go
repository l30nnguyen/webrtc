package admission

import (
	"errors"
	"time"
)

type Validator struct {
	Tickets   TicketVerifier
	Blocklist Blocklist
	// Replay, when set, makes each ticket usable once.
	Replay *ReplayCache
}

func (v Validator) now() time.Time {
	if v.Tickets.Now != nil {
		return v.Tickets.Now()
	}
	return time.Now()
}

var ErrUnavailable = errors.New("admission dependency unavailable")

// Validate authenticates a ticket for deviceID and returns its claims. The
// tenant and role in the claims are authoritative; callers must use them and
// ignore anything the client says about itself.
func (v Validator) Validate(token, deviceID string) (Claims, error) {
	claims, err := v.Check(token, deviceID)
	if err != nil {
		return Claims{}, err
	}
	if !v.Consume(claims) {
		return Claims{}, errors.New("invalid ticket")
	}
	return claims, nil
}

// Check authenticates a ticket like Validate but only looks at the replay
// cache; it does not spend the ticket. Call Consume once the connection is
// really established.
func (v Validator) Check(token, deviceID string) (Claims, error) {
	claims, err := v.Tickets.Verify(token, deviceID)
	if err != nil {
		return Claims{}, err
	}
	blocked, err := v.Blocklist.Blocked(claims.Digest, claims.Tenant)
	if err != nil {
		return Claims{}, ErrUnavailable
	}
	if blocked {
		return Claims{}, errors.New("invalid ticket")
	}
	// A viewer ticket's subject is a throwaway viewer ID; a camera ticket's
	// subject is the device ID that may have been revoked by a release.
	revoked, err := v.Blocklist.DeviceRevoked(claims.Subject, v.now())
	if err != nil {
		return Claims{}, ErrUnavailable
	}
	if revoked {
		return Claims{}, errors.New("invalid ticket")
	}
	if v.Replay != nil && v.Replay.Seen(claims.TokenID) {
		return Claims{}, errors.New("invalid ticket")
	}
	return claims, nil
}

// Consume spends the ticket and reports whether this was its first use. Two
// connections racing with one ticket both pass Check; only one wins here.
func (v Validator) Consume(claims Claims) bool {
	return v.Replay == nil || v.Replay.Use(claims.TokenID, claims.Expires)
}
