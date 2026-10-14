package admission

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	// MaxTicketLifetime is the longest ticket a verifier accepts.
	MaxTicketLifetime = 300 * time.Second
	// DefaultTicketLifetime is what Center issues unless configured otherwise.
	DefaultTicketLifetime = 60 * time.Second
	// ClockLeeway is how far in the future a ticket's iat may be, so a small
	// clock difference between Center and a node does not reject a fresh ticket.
	ClockLeeway = 30 * time.Second

	RoleCamera = "camera"
	RolePlayer = "player"
)

// Claims is the signed admission ticket body. Tenant and Role come from the
// verified SID, never from anything the client sends; Subject is the device
// identifier derived from the device's own public key.
type Claims struct {
	Issuer   string `json:"iss"`
	Subject  string `json:"sub"`
	Role     string `json:"role"`
	Tenant   string `json:"tid"`
	Audience string `json:"aud"`
	Digest   string `json:"digest"`
	// CameraID scopes a viewer ticket to one activated camera. Empty for
	// camera tickets and older unscoped player tickets.
	CameraID string `json:"cid,omitempty"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
	TokenID  string `json:"jti"`
}

type header struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Type      string `json:"typ"`
}

type TicketSigner struct {
	PrivateKey ed25519.PrivateKey
	KeyID      string
	Audience   string
	Lifetime   time.Duration // zero means DefaultTicketLifetime
	Now        func() time.Time
	Random     io.Reader
}

// EffectiveLifetime is the ticket lifetime the signer will use.
func (s TicketSigner) EffectiveLifetime() time.Duration {
	if s.Lifetime <= 0 {
		return DefaultTicketLifetime
	}
	return s.Lifetime
}

func (s TicketSigner) Sign(deviceID, digest, tenant, role string) (string, error) {
	return s.sign(deviceID, digest, tenant, role, "")
}

// SignCameraViewer creates a player ticket scoped to one camera device ID.
func (s TicketSigner) SignCameraViewer(viewerID, cameraSIDDigest, tenant, cameraID string) (string, error) {
	if !ValidDeviceID(cameraID) {
		return "", errors.New("invalid camera scope")
	}
	return s.sign(viewerID, cameraSIDDigest, tenant, RolePlayer, cameraID)
}

func (s TicketSigner) sign(deviceID, digest, tenant, role, cameraID string) (string, error) {
	lifetime := s.EffectiveLifetime()
	if len(s.PrivateKey) != ed25519.PrivateKeySize || !ValidKeyID(s.KeyID) || ValidateAudience(s.Audience) != nil || !ValidDeviceID(deviceID) || !validDigest(digest) || !validTenant(tenant) || !validRole(role) || (cameraID != "" && role != RolePlayer) || lifetime > MaxTicketLifetime {
		return "", errors.New("invalid ticket signer configuration or input")
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	random := s.Random
	if random == nil {
		random = rand.Reader
	}
	nowSeconds := now().Unix()
	if nowSeconds < 0 || nowSeconds > int64(^uint64(0)>>1)-int64(lifetime.Seconds()) {
		return "", errors.New("invalid ticket issue time")
	}
	var jtiBytes [16]byte
	if _, err := io.ReadFull(random, jtiBytes[:]); err != nil {
		return "", fmt.Errorf("create ticket id: %w", err)
	}
	h := header{Algorithm: "EdDSA", KeyID: s.KeyID, Type: "JWT"}
	c := Claims{Issuer: "center", Subject: deviceID, Role: role, Tenant: tenant, Audience: s.Audience, Digest: digest, CameraID: cameraID, IssuedAt: nowSeconds, Expires: nowSeconds + int64(lifetime.Seconds()), TokenID: base64.RawURLEncoding.EncodeToString(jtiBytes[:])}
	hb, err := json.Marshal(h)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	input := enc.EncodeToString(hb) + "." + enc.EncodeToString(cb)
	sig := ed25519.Sign(s.PrivateKey, []byte(input))
	return input + "." + enc.EncodeToString(sig), nil
}

type TicketVerifier struct {
	PublicKey ed25519.PublicKey
	KeyID     string
	Audience  string
	Now       func() time.Time
}

// Verify authenticates a ticket and requires its subject to be deviceID.
func (v TicketVerifier) Verify(token, deviceID string) (Claims, error) {
	if !ValidDeviceID(deviceID) {
		return Claims{}, errors.New("invalid ticket")
	}
	return v.verify(token, deviceID)
}

// VerifyAny authenticates a ticket for whichever subject it names. Center uses
// it to learn who is calling its own HTTP API with a ticket it issued.
func (v TicketVerifier) VerifyAny(token string) (Claims, error) {
	return v.verify(token, "")
}

func (v TicketVerifier) verify(token, deviceID string) (Claims, error) {
	var c Claims
	if len(v.PublicKey) != ed25519.PublicKeySize || !ValidKeyID(v.KeyID) || ValidateAudience(v.Audience) != nil || len(token) > 4096 {
		return c, errors.New("invalid ticket")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return c, errors.New("invalid ticket")
	}
	enc := base64.RawURLEncoding.Strict()
	hb, err := decodeCanonical(enc, parts[0])
	if err != nil {
		return c, errors.New("invalid ticket")
	}
	cb, err := decodeCanonical(enc, parts[1])
	if err != nil {
		return c, errors.New("invalid ticket")
	}
	sig, err := decodeCanonical(enc, parts[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return c, errors.New("invalid ticket")
	}
	var h header
	if err := decodeStrict(hb, &h); err != nil || h.Algorithm != "EdDSA" || h.KeyID != v.KeyID || h.Type != "JWT" {
		return c, errors.New("invalid ticket")
	}
	if !ed25519.Verify(v.PublicKey, []byte(parts[0]+"."+parts[1]), sig) {
		return c, errors.New("invalid ticket")
	}
	if err := decodeStrict(cb, &c); err != nil {
		return Claims{}, errors.New("invalid ticket")
	}
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	nowSec := now().Unix()
	maxExpiry := nowSec + int64(MaxTicketLifetime.Seconds())
	if nowSec > int64(^uint64(0)>>1)-int64(MaxTicketLifetime.Seconds()) {
		maxExpiry = int64(^uint64(0) >> 1)
	}
	if c.Issuer != "center" || !ValidDeviceID(c.Subject) || (deviceID != "" && c.Subject != deviceID) || !validRole(c.Role) || !validTenant(c.Tenant) || c.Audience != v.Audience || !validDigest(c.Digest) || (c.CameraID != "" && (c.Role != RolePlayer || !ValidDeviceID(c.CameraID))) || c.IssuedAt < 0 || c.IssuedAt > nowSec+int64(ClockLeeway.Seconds()) || c.Expires <= nowSec || c.Expires > maxExpiry || c.Expires <= c.IssuedAt || c.Expires-c.IssuedAt > int64(MaxTicketLifetime.Seconds()) || !validTokenID(c.TokenID) {
		return Claims{}, errors.New("invalid ticket")
	}
	return c, nil
}

func decodeCanonical(enc *base64.Encoding, text string) ([]byte, error) {
	data, err := enc.DecodeString(text)
	if err != nil || enc.EncodeToString(data) != text {
		return nil, errors.New("noncanonical base64url")
	}
	return data, nil
}

func decodeStrict(data []byte, dst any) error {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	d := json.NewDecoder(strings.NewReader(string(data)))
	if err := consumeJSONValue(d); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func consumeJSONValue(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := make(map[string]struct{})
		for d.More() {
			keyToken, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("invalid JSON object key")
			}
			if _, exists := keys[key]; exists {
				return errors.New("duplicate JSON object key")
			}
			keys[key] = struct{}{}
			if err := consumeJSONValue(d); err != nil {
				return err
			}
		}
		closeToken, err := d.Token()
		if err != nil || closeToken != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case '[':
		for d.More() {
			if err := consumeJSONValue(d); err != nil {
				return err
			}
		}
		closeToken, err := d.Token()
		if err != nil || closeToken != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validRole(role string) bool { return role == RoleCamera || role == RolePlayer }

func validTenant(tenant string) bool { return tenantPattern.MatchString(tenant) }

func validTokenID(value string) bool {
	b, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(b) == 16 && base64.RawURLEncoding.EncodeToString(b) == value
}
