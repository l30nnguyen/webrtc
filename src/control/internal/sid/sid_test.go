package sid

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"math"
	"strings"
	"testing"
	"time"
)

var zeroNonce = make([]byte, NonceSize)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return publicKey, privateKey
}

func TestIssueEncodesExpectedPayloadAndSignature(t *testing.T) {
	publicKey, privateKey := newKey(t)
	issuedAt := time.Unix(1_700_000_123, 0).In(time.FixedZone("test", 7*60*60))
	nonce := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8}

	got, err := Issue(issuedAt, "acme01", FlagPublisher, privateKey, bytes.NewReader(nonce))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != EncodedSize {
		t.Fatalf("encoded length = %d, want %d", len(got), EncodedSize)
	}
	if strings.Contains(got, "=") {
		t.Fatalf("SID has Base64 padding: %q", got)
	}
	raw, err := base64.RawURLEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("decode SID: %v", err)
	}
	if len(raw) != RawSize {
		t.Fatalf("decoded length = %d, want %d", len(raw), RawSize)
	}
	if raw[0] != Version {
		t.Fatalf("version = %d, want %d", raw[0], Version)
	}
	if gotUnix := binary.BigEndian.Uint32(raw[1:5]); gotUnix != uint32(issuedAt.Unix()) {
		t.Fatalf("issued timestamp = %d, want %d", gotUnix, issuedAt.Unix())
	}
	if gotFlags := binary.BigEndian.Uint16(raw[5:7]); gotFlags != FlagPublisher {
		t.Fatalf("flags = %#x, want %#x", gotFlags, FlagPublisher)
	}
	if want := []byte{'a', 'c', 'm', 'e', '0', '1', 0, 0}; !bytes.Equal(raw[7:15], want) {
		t.Fatalf("tenant bytes = %v, want %v", raw[7:15], want)
	}
	if !bytes.Equal(raw[15:PayloadSize], nonce) {
		t.Fatalf("nonce = %v, want %v", raw[15:PayloadSize], nonce)
	}
	if !ed25519.Verify(publicKey, raw[:PayloadSize], raw[PayloadSize:]) {
		t.Fatal("generated signature did not verify")
	}
}

func TestIssueRejectsUnrepresentableTimestamps(t *testing.T) {
	_, privateKey := newKey(t)
	for _, tt := range []struct {
		name string
		when time.Time
	}{
		{name: "before epoch", when: time.Unix(-1, 0)},
		{name: "after uint32", when: time.Unix(int64(math.MaxUint32)+1, 0)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Issue(tt.when, "acme01", FlagPublisher, privateKey, bytes.NewReader(zeroNonce)); err != ErrInvalidTime {
				t.Fatalf("Issue() error = %v, want %v", err, ErrInvalidTime)
			}
		})
	}
}

func TestIssueRejectsInvalidKeysEntropyTenantAndFlags(t *testing.T) {
	when := time.Unix(1_700_000_000, 0)
	validKey := make(ed25519.PrivateKey, ed25519.PrivateKeySize)
	if _, err := Issue(when, "acme01", FlagPublisher, validKey[:len(validKey)-1], bytes.NewReader(zeroNonce)); err != ErrInvalidKey {
		t.Fatalf("short key error = %v, want %v", err, ErrInvalidKey)
	}
	if _, err := Issue(when, "acme01", FlagPublisher, validKey, nil); err != ErrInvalidEntropy {
		t.Fatalf("nil entropy error = %v, want %v", err, ErrInvalidEntropy)
	}
	if _, err := Issue(when, "acme01", FlagPublisher, validKey, bytes.NewReader([]byte{1})); err == nil {
		t.Fatal("short entropy source unexpectedly succeeded")
	}
	for _, tenant := range []string{"", "short", "toolong99", "UPPER1", "ac me01", "acme-01", "acmé01"} {
		if _, err := Issue(when, tenant, FlagPublisher, validKey, bytes.NewReader(zeroNonce)); err != ErrInvalidTenant {
			t.Fatalf("tenant %q error = %v, want %v", tenant, err, ErrInvalidTenant)
		}
	}
	for _, flags := range []uint16{0, FlagPublisher | FlagViewer, 0x0004, FlagViewer | 0x8000} {
		if _, err := Issue(when, "acme01", flags, validKey, bytes.NewReader(zeroNonce)); err != ErrInvalidFlags {
			t.Fatalf("flags %#x error = %v, want %v", flags, err, ErrInvalidFlags)
		}
	}
}

func TestIssueAllowsTimestampBoundary(t *testing.T) {
	_, privateKey := newKey(t)
	got, err := Issue(time.Unix(math.MaxUint32, 0), "12345678", FlagViewer, privateKey, bytes.NewReader(zeroNonce))
	if err != nil {
		t.Fatalf("Issue() at maximum timestamp: %v", err)
	}
	if len(got) != EncodedSize {
		t.Fatalf("encoded length = %d, want %d", len(got), EncodedSize)
	}
}

func TestFingerprints(t *testing.T) {
	publicKey := ed25519.PublicKey(make([]byte, ed25519.PublicKeySize))
	got, err := PublicKeyFingerprint(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	if want := sha256.Sum256(publicKey); got != want {
		t.Fatalf("public key fingerprint = %x, want %x", got, want)
	}
	if want := sha256.Sum256([]byte("sample SID")); SIDFingerprint("sample SID") != want {
		t.Fatalf("SID fingerprint = %x, want %x", SIDFingerprint("sample SID"), want)
	}
	if _, err := PublicKeyFingerprint(publicKey[:len(publicKey)-1]); err == nil {
		t.Fatal("invalid public-key size unexpectedly succeeded")
	}
}

func TestCameraRefIsDomainSeparatedFromDigest(t *testing.T) {
	ref := CameraRef("sample SID")
	if len(ref) != 64 {
		t.Fatalf("ref length = %d, want 64", len(ref))
	}
	digest := SIDFingerprint("sample SID")
	if ref == hex.EncodeToString(digest[:]) {
		t.Fatal("camera ref equals the SID digest; it must not be derivable from the digest")
	}
	want := sha256.Sum256([]byte("camera-ref-v1sample SID"))
	if ref != hex.EncodeToString(want[:]) {
		t.Fatalf("ref = %s, want %x", ref, want)
	}
}

func TestVerifyReturnsAuthenticatedMetadataAndCanonicalDigest(t *testing.T) {
	publicKey, privateKey := newKey(t)
	issuedAt := time.Unix(1_700_000_123, 0).UTC()
	nonce := []byte{10, 11, 12, 13, 14, 15, 16, 17, 18}
	encoded, err := Issue(issuedAt, "tenant8x", FlagViewer, privateKey, bytes.NewReader(nonce))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Verify(encoded, publicKey, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IssuedAt.Equal(issuedAt) || got.Flags != FlagViewer || got.Tenant != "tenant8x" || !bytes.Equal(got.Nonce[:], nonce) || got.Publisher() {
		t.Fatalf("unexpected verified metadata: %+v", got)
	}
	if want := sha256.Sum256([]byte(encoded)); got.Digest != want {
		t.Fatalf("digest = %x, want %x", got.Digest, want)
	}
}

func TestVerifyTenantLengths(t *testing.T) {
	publicKey, privateKey := newKey(t)
	issuedAt := time.Unix(1_700_000_000, 0).UTC()
	for _, tenant := range []string{"abcdef", "abcdefg", "abcdefgh", "a1b2c3"} {
		encoded, err := Issue(issuedAt, tenant, FlagPublisher, privateKey, bytes.NewReader(zeroNonce))
		if err != nil {
			t.Fatal(err)
		}
		got, err := Verify(encoded, publicKey, issuedAt)
		if err != nil || got.Tenant != tenant {
			t.Fatalf("tenant %q round trip = %q, %v", tenant, got.Tenant, err)
		}
	}
}

// signRaw builds a correctly signed SID around an arbitrary payload so the
// payload validation in Verify can be exercised independently of Issue.
func signRaw(privateKey ed25519.PrivateKey, payload []byte) string {
	raw := append(append([]byte(nil), payload...), ed25519.Sign(privateKey, payload)...)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func TestVerifyRejectsSignedButInvalidPayloads(t *testing.T) {
	publicKey, privateKey := newKey(t)
	issuedAt := time.Unix(1_700_000_000, 0).UTC()
	good, err := Issue(issuedAt, "acme01", FlagPublisher, privateKey, bytes.NewReader(zeroNonce))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(good)
	mutate := func(f func(p []byte)) string {
		p := append([]byte(nil), raw[:PayloadSize]...)
		f(p)
		return signRaw(privateKey, p)
	}
	for name, text := range map[string]string{
		"wrong version":      mutate(func(p []byte) { p[0] = 1 }),
		"no role":            mutate(func(p []byte) { binary.BigEndian.PutUint16(p[5:7], 0) }),
		"both roles":         mutate(func(p []byte) { binary.BigEndian.PutUint16(p[5:7], 3) }),
		"reserved flag":      mutate(func(p []byte) { binary.BigEndian.PutUint16(p[5:7], 0x8001) }),
		"empty tenant":       mutate(func(p []byte) { copy(p[7:15], make([]byte, 8)) }),
		"short tenant":       mutate(func(p []byte) { copy(p[7:15], []byte{'a', 'b', 0, 0, 0, 0, 0, 0}) }),
		"uppercase tenant":   mutate(func(p []byte) { p[7] = 'A' }),
		"embedded nul":       mutate(func(p []byte) { p[9] = 0 }),
		"non-ascii tenant":   mutate(func(p []byte) { p[7] = 0xc3 }),
		"tenant with symbol": mutate(func(p []byte) { p[8] = '-' }),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Verify(text, publicKey, issuedAt); err != ErrMalformed {
				t.Fatalf("Verify() error = %v, want %v", err, ErrMalformed)
			}
		})
	}
}

func TestVerifyRejectsMalformedAndNoncanonicalEncodings(t *testing.T) {
	publicKey, privateKey := newKey(t)
	issuedAt := time.Unix(1_700_000_000, 0).UTC()
	encoded, err := Issue(issuedAt, "acme01", FlagPublisher, privateKey, bytes.NewReader(zeroNonce))
	if err != nil {
		t.Fatal(err)
	}
	alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	lastIndex := strings.IndexByte(alphabet, encoded[len(encoded)-1])
	if lastIndex < 0 || lastIndex%16 != 0 {
		t.Fatalf("unexpected final Base64URL sextet: %q index=%d", encoded[len(encoded)-1], lastIndex)
	}
	noncanonical := encoded[:len(encoded)-1] + string(alphabet[lastIndex+1])
	for _, tt := range []struct {
		name string
		text string
	}{
		{name: "short", text: encoded[:len(encoded)-1]},
		{name: "padded", text: encoded + "="},
		{name: "newline", text: encoded + "\n"},
		{name: "whitespace", text: encoded[:20] + " " + encoded[20:]},
		{name: "noncanonical trailing bits", text: noncanonical},
		{name: "old v1 length", text: encoded[:107]},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Verify(tt.text, publicKey, issuedAt); err != ErrMalformed {
				t.Fatalf("Verify() error = %v, want %v", err, ErrMalformed)
			}
		})
	}
}

func TestVerifyRejectsInvalidSignaturesAndWrongKeys(t *testing.T) {
	publicKey, privateKey := newKey(t)
	issuedAt := time.Unix(1_700_000_000, 0).UTC()
	encoded, err := Issue(issuedAt, "acme01", FlagPublisher, privateKey, bytes.NewReader(zeroNonce))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	tamperedPayload := append([]byte(nil), raw...)
	tamperedPayload[8] ^= 1 // flip a tenant bit
	tamperedSignature := append([]byte(nil), raw...)
	tamperedSignature[RawSize-1] ^= 1
	otherPublic, _ := newKey(t)
	for _, tt := range []struct {
		name string
		sid  string
		key  ed25519.PublicKey
	}{
		{name: "tenant tampered", sid: base64.RawURLEncoding.EncodeToString(tamperedPayload), key: publicKey},
		{name: "signature tampered", sid: base64.RawURLEncoding.EncodeToString(tamperedSignature), key: publicKey},
		{name: "wrong key", sid: encoded, key: otherPublic},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Verify(tt.sid, tt.key, issuedAt); err != ErrBadSignature {
				t.Fatalf("Verify() error = %v, want %v", err, ErrBadSignature)
			}
		})
	}
	if _, err := Verify(encoded, publicKey[:len(publicKey)-1], issuedAt); err != ErrInvalidPublic {
		t.Fatalf("short public key error = %v, want %v", err, ErrInvalidPublic)
	}
}

func TestVerifyMinimumIssueTimeBoundary(t *testing.T) {
	publicKey, privateKey := newKey(t)
	issuedAt := time.Unix(1_700_000_000, 0).UTC()
	encoded, err := Issue(issuedAt, "acme01", FlagPublisher, privateKey, bytes.NewReader(zeroNonce))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(encoded, publicKey, issuedAt); err != nil {
		t.Fatalf("SID at minimum timestamp rejected: %v", err)
	}
	for _, min := range []time.Time{issuedAt.Add(time.Second), issuedAt.Add(100 * time.Millisecond)} {
		if _, err := Verify(encoded, publicKey, min); err != ErrTooOld {
			t.Fatalf("minimum %s: Verify() error = %v, want %v", min, err, ErrTooOld)
		}
	}
	if _, err := Verify(encoded, publicKey, time.Unix(-1, 0)); err != ErrInvalidMinimum {
		t.Fatalf("negative minimum error = %v, want %v", err, ErrInvalidMinimum)
	}
}

// TestGoldenVector pins the wire format shared with the camera verifier
// (libdatachannel_c14/examples/streamer/sid_verifier_test.cpp). Ed25519 is
// deterministic, so the same inputs must always yield the same SID.
func TestGoldenVector(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	nonce := []byte{0xa0, 0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8}
	got, err := Issue(time.Unix(1_790_000_000, 0), "acme01", FlagPublisher, privateKey, bytes.NewReader(nonce))
	if err != nil {
		t.Fatal(err)
	}
	publicHex := hex.EncodeToString(privateKey.Public().(ed25519.PublicKey))
	const wantPublicHex = "79b5562e8fe654f94078b112e8a98ba7901f853ae695bed7e0e3910bad049664"
	const wantSID = "AmqxO4AAAWFjbWUwMQAAoKGio6Slpqeo69QW527HNirlkBhlJxrrja7rgEEWZC_odr4OpHHVA2NBERot7UmgkO8DBP8eF6NBbeiI3Tkoudp6-6NpzxXiDw"
	if publicHex != wantPublicHex || got != wantSID {
		t.Fatalf("golden vector changed:\npublic=%s\nsid=%s", publicHex, got)
	}
}
