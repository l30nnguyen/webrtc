package admission

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"
)

func TestTicketVerifierStrictClaims(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	digestBytes := sha256.Sum256([]byte("sid"))
	valid := Claims{Issuer: "center", Subject: "camera0", Role: "camera", Tenant: "acme01", Audience: "wss://main.example.test/", Digest: hex.EncodeToString(digestBytes[:]), IssuedAt: now.Unix(), Expires: now.Unix() + 300, TokenID: base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef"))}
	encodeClaims := func(claims Claims) []byte { data, _ := json.Marshal(claims); return data }
	encodeHeader := func(algorithm, kid string) []byte {
		data, _ := json.Marshal(map[string]string{"alg": algorithm, "kid": kid, "typ": "JWT"})
		return data
	}
	makeToken := func(header, claims []byte) string {
		enc := base64.RawURLEncoding
		input := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
		return input + "." + enc.EncodeToString(ed25519.Sign(privateKey, []byte(input)))
	}
	verifier := TicketVerifier{PublicKey: publicKey, KeyID: "center-v1", Audience: valid.Audience, Now: func() time.Time { return now }}

	t.Run("valid token", func(t *testing.T) {
		if _, err := verifier.Verify(makeToken(encodeHeader("EdDSA", "center-v1"), encodeClaims(valid)), "camera0"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("small forward clock skew is tolerated", func(t *testing.T) {
		skewed := valid
		skewed.IssuedAt = now.Unix() + 30
		skewed.Expires = skewed.IssuedAt + 60
		if _, err := verifier.Verify(makeToken(encodeHeader("EdDSA", "center-v1"), encodeClaims(skewed)), "camera0"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("player role is accepted and VerifyAny returns the claims", func(t *testing.T) {
		player := valid
		player.Role = "player"
		claims, err := verifier.VerifyAny(makeToken(encodeHeader("EdDSA", "center-v1"), encodeClaims(player)))
		if err != nil || claims.Role != "player" || claims.Tenant != "acme01" || claims.Subject != "camera0" {
			t.Fatalf("VerifyAny = %+v, %v", claims, err)
		}
		if _, err := verifier.Verify(makeToken(encodeHeader("EdDSA", "center-v1"), encodeClaims(player)), "other"); err == nil {
			t.Fatal("subject mismatch accepted")
		}
	})
	cases := []struct {
		name      string
		claims    Claims
		header    []byte
		rawClaims []byte
	}{
		{name: "negative issue time", claims: func() Claims { c := valid; c.IssuedAt = -1; return c }()},
		{name: "issue time beyond clock leeway", claims: func() Claims { c := valid; c.IssuedAt = now.Unix() + 31; c.Expires = c.IssuedAt + 60; return c }()},
		{name: "missing tenant", claims: func() Claims { c := valid; c.Tenant = ""; return c }()},
		{name: "invalid tenant", claims: func() Claims { c := valid; c.Tenant = "ACME01"; return c }()},
		{name: "unknown role", claims: func() Claims { c := valid; c.Role = "admin"; return c }()},
		{name: "expiry over 300 seconds", claims: func() Claims { c := valid; c.Expires = now.Unix() + 301; return c }()},
		{name: "wrong pinned kid", claims: valid, header: encodeHeader("EdDSA", "other")},
		{name: "wrong algorithm", claims: valid, header: encodeHeader("HS256", "center-v1")},
		{name: "duplicate claim", claims: valid, rawClaims: []byte(`{"iss":"center","iss":"center","sub":"camera0","role":"camera","tid":"acme01","aud":"wss://main.example.test/","digest":"` + valid.Digest + `","iat":1791806400,"exp":1791806700,"jti":"MDEyMzQ1Njc4OWFiY2RlZg"}`)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			header := test.header
			if header == nil {
				header = encodeHeader("EdDSA", "center-v1")
			}
			claims := test.rawClaims
			if claims == nil {
				claims = encodeClaims(test.claims)
			}
			if _, err := verifier.Verify(makeToken(header, claims), "camera0"); err == nil {
				t.Fatal("invalid ticket was accepted")
			}
		})
	}
}

func TestSignerDefaultsToSixtySecondTicketsAndRejectsLongOnes(t *testing.T) {
	publicKey, privateKey, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	digest := hex.EncodeToString(make([]byte, 32))
	signer := TicketSigner{PrivateKey: privateKey, KeyID: "center-v1", Audience: "wss://main.example.test/", Now: func() time.Time { return now }}
	token, err := signer.Sign("camera0", digest, "acme01", RoleCamera)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := (TicketVerifier{PublicKey: publicKey, KeyID: "center-v1", Audience: signer.Audience, Now: func() time.Time { return now }}).Verify(token, "camera0")
	if err != nil || claims.Expires-claims.IssuedAt != 60 || claims.Tenant != "acme01" || claims.Role != RoleCamera {
		t.Fatalf("claims = %+v, %v", claims, err)
	}
	signer.Lifetime = 301 * time.Second
	if _, err := signer.Sign("camera0", digest, "acme01", RoleCamera); err == nil {
		t.Fatal("lifetime above the maximum accepted")
	}
	signer.Lifetime = 0
	for _, bad := range []struct{ tenant, role string }{{"", RoleCamera}, {"acme01", "root"}, {"AB", RoleCamera}} {
		if _, err := signer.Sign("camera0", digest, bad.tenant, bad.role); err == nil {
			t.Fatalf("Sign accepted tenant=%q role=%q", bad.tenant, bad.role)
		}
	}
}

func TestCameraViewerTicketCarriesTargetCameraScope(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	signer := TicketSigner{PrivateKey: private, KeyID: "center-v1", Audience: "wss://main.example.test/", Now: func() time.Time { return now }}
	viewerID := "q3N0k3NoKkPyHf4W9Jw9F2iQ7n_oa3q8T1LZ6KfV5zA"
	cameraID := "cameraDevice01"
	digest := hex.EncodeToString(make([]byte, 32))
	token, err := signer.SignCameraViewer(viewerID, digest, "acme01", cameraID)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := (TicketVerifier{PublicKey: public, KeyID: "center-v1", Audience: signer.Audience, Now: func() time.Time { return now }}).Verify(token, viewerID)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Role != RolePlayer || claims.Subject != viewerID || claims.Digest != digest || claims.CameraID != cameraID || claims.Expires-claims.IssuedAt != 60 {
		t.Fatalf("unexpected scoped viewer claims: %+v", claims)
	}
}
