// Package sid creates and verifies signed SIDs (version 2).
//
// A SID is device-agnostic: it names a tenant, an issue time and a license
// role, never a device. A device binds itself to a SID the first time it is
// used (see internal/center); the SID layout carries nothing about it.
//
// Payload (24 bytes) followed by the 64-byte Ed25519 signature over it:
//
//	1B version (=2) | 4B issued (unix, BE) | 2B flags (BE) | 8B tenant | 9B nonce
//
// The tenant is 6-8 ASCII characters [a-z0-9], right-padded with zero bytes.
package sid

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"time"
)

const (
	Version       = 2
	PayloadSize   = 24
	SignatureSize = ed25519.SignatureSize
	RawSize       = PayloadSize + SignatureSize
	EncodedSize   = 118
	TenantSize    = 8
	NonceSize     = 9

	// FlagPublisher marks a SID that may stream media (a camera or any
	// device publishing video); FlagViewer marks a SID that may only watch.
	// Exactly one must be set. The remaining flag bits are reserved.
	FlagPublisher uint16 = 0x0001
	FlagViewer    uint16 = 0x0002
	flagRoleMask  uint16 = FlagPublisher | FlagViewer
)

var tenantPattern = regexp.MustCompile(`^[a-z0-9]{6,8}$`)

var (
	ErrInvalidKey     = errors.New("invalid ed25519 private key")
	ErrInvalidTime    = errors.New("issued timestamp is outside the SID format range")
	ErrInvalidEntropy = errors.New("invalid SID entropy source")
	ErrInvalidTenant  = errors.New("tenant must be 6-8 characters of [a-z0-9]")
	ErrInvalidFlags   = errors.New("flags must select exactly one of publisher or viewer and use no reserved bits")
	ErrMalformed      = errors.New("malformed SID")
	ErrInvalidPublic  = errors.New("invalid ed25519 public key")
	ErrBadSignature   = errors.New("invalid SID signature")
	ErrTooOld         = errors.New("SID predates the minimum issue time")
	ErrInvalidMinimum = errors.New("minimum issue time is outside the SID timestamp range")
)

// Verified contains the non-secret payload metadata and digest for an
// authenticated SID. It deliberately does not retain or return the SID.
type Verified struct {
	IssuedAt time.Time
	Flags    uint16
	Tenant   string
	Nonce    [NonceSize]byte
	Digest   [sha256.Size]byte
}

// Publisher reports whether the SID licenses a streaming device.
func (v Verified) Publisher() bool { return v.Flags&FlagPublisher != 0 }

// ValidTenant reports whether name is a syntactically valid tenant.
func ValidTenant(name string) bool { return tenantPattern.MatchString(name) }

// ValidFlags reports whether flags select exactly one role and no reserved bits.
func ValidFlags(flags uint16) bool {
	role := flags & flagRoleMask
	return flags&^flagRoleMask == 0 && (role == FlagPublisher || role == FlagViewer)
}

// Issue creates a canonical SID. issuedAt is encoded as unsigned Unix seconds
// in UTC. entropy must provide the nonce and should normally be crypto/rand.Reader.
func Issue(issuedAt time.Time, tenant string, flags uint16, privateKey ed25519.PrivateKey, entropy io.Reader) (string, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return "", ErrInvalidKey
	}
	if entropy == nil {
		return "", ErrInvalidEntropy
	}
	if !ValidTenant(tenant) {
		return "", ErrInvalidTenant
	}
	if !ValidFlags(flags) {
		return "", ErrInvalidFlags
	}
	unix := issuedAt.Unix()
	if unix < 0 || uint64(unix) > math.MaxUint32 {
		return "", ErrInvalidTime
	}

	var payload [PayloadSize]byte
	payload[0] = Version
	binary.BigEndian.PutUint32(payload[1:5], uint32(unix))
	binary.BigEndian.PutUint16(payload[5:7], flags)
	copy(payload[7:7+TenantSize], tenant)
	if _, err := io.ReadFull(entropy, payload[7+TenantSize:]); err != nil {
		return "", fmt.Errorf("read SID nonce: %w", err)
	}

	signature := ed25519.Sign(privateKey, payload[:])
	raw := make([]byte, 0, RawSize)
	raw = append(raw, payload[:]...)
	raw = append(raw, signature...)
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Verify validates canonical unpadded Base64URL, the complete Ed25519
// signature, the payload fields, and a minimum-issued-time policy. minIssued
// is compared at whole-second precision; a fractional second rounds up to the
// next representable SID timestamp. It does not validate expiry or tenant
// registration; those are Center policy.
func Verify(encoded string, publicKey ed25519.PublicKey, minIssued time.Time) (Verified, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return Verified{}, ErrInvalidPublic
	}
	minUnix := minIssued.Unix()
	if minUnix < 0 || uint64(minUnix) > math.MaxUint32 {
		return Verified{}, ErrInvalidMinimum
	}
	if minIssued.Nanosecond() != 0 {
		if uint64(minUnix) == math.MaxUint32 {
			return Verified{}, ErrInvalidMinimum
		}
		minUnix++
	}
	if len(encoded) != EncodedSize {
		return Verified{}, ErrMalformed
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) != RawSize || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return Verified{}, ErrMalformed
	}
	payload := raw[:PayloadSize]
	if !ed25519.Verify(publicKey, payload, raw[PayloadSize:]) {
		return Verified{}, ErrBadSignature
	}
	if payload[0] != Version {
		return Verified{}, ErrMalformed
	}
	issuedUnix := binary.BigEndian.Uint32(payload[1:5])
	if uint64(issuedUnix) < uint64(minUnix) {
		return Verified{}, ErrTooOld
	}
	flags := binary.BigEndian.Uint16(payload[5:7])
	if !ValidFlags(flags) {
		return Verified{}, ErrMalformed
	}
	tenantBytes := payload[7 : 7+TenantSize]
	end := TenantSize
	for end > 0 && tenantBytes[end-1] == 0 {
		end--
	}
	tenant := string(tenantBytes[:end])
	if !ValidTenant(tenant) {
		return Verified{}, ErrMalformed
	}
	var result Verified
	result.IssuedAt = time.Unix(int64(issuedUnix), 0).UTC()
	result.Flags = flags
	result.Tenant = tenant
	copy(result.Nonce[:], payload[7+TenantSize:])
	result.Digest = SIDFingerprint(encoded)
	return result, nil
}

// PublicKeyFingerprint returns SHA-256 of the raw 32-byte Ed25519 public key.
func PublicKeyFingerprint(publicKey ed25519.PublicKey) ([sha256.Size]byte, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return [sha256.Size]byte{}, errors.New("invalid ed25519 public key")
	}
	return sha256.Sum256(publicKey), nil
}

// SIDFingerprint hashes the canonical ASCII SID representation. The result is
// a 32-byte digest used in tickets, blocklists and audit records. It is an
// identifier, not a secret, so it must never be enough to use a camera.
func SIDFingerprint(canonicalSID string) [sha256.Size]byte {
	return sha256.Sum256([]byte(canonicalSID))
}

// CameraRef returns the lowercase-hex view reference of a SID:
// SHA-256("camera-ref-v1" || SID). A player that was given a camera's SID
// during pairing computes it to find the camera. Unlike the digest it can not
// be derived without the SID, so Center may safely look cameras up by it.
func CameraRef(canonicalSID string) string {
	h := sha256.New()
	h.Write([]byte("camera-ref-v1"))
	h.Write([]byte(canonicalSID))
	return hex.EncodeToString(h.Sum(nil))
}
