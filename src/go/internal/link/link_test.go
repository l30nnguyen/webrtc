package link

import (
	"bytes"
	"crypto/ed25519"
	"testing"
)

func TestConfigSignature(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, ed25519.SeedSize))
	config := []byte(`{"version":3,"nodes":[]}`)
	m := SignConfig(key, 3, config)
	got, err := VerifyConfig(key.Public().(ed25519.PublicKey), m)
	if err != nil || !bytes.Equal(got, config) {
		t.Fatalf("VerifyConfig = %q, %v", got, err)
	}
	if _, err := VerifyConfig(other.Public().(ed25519.PublicKey), m); err == nil {
		t.Fatal("another key verified")
	}
	replayed := m
	replayed.Version = 4 // the signature covers the version
	if _, err := VerifyConfig(key.Public().(ed25519.PublicKey), replayed); err == nil {
		t.Fatal("changed version verified")
	}
	tampered := SignConfig(key, 3, config)
	tampered.Config = SignConfig(key, 3, []byte(`{"version":3,"nodes":[{}]}`)).Config
	if _, err := VerifyConfig(key.Public().(ed25519.PublicKey), tampered); err == nil {
		t.Fatal("changed content verified")
	}
}

func TestLoginMessageBindsEveryField(t *testing.T) {
	nonce := bytes.Repeat([]byte{1}, NonceSize)
	base := LoginMessage(nonce, "matrix.5gen.care", "wss://eu.5gen.care:443/")
	for _, other := range [][]byte{
		LoginMessage(bytes.Repeat([]byte{2}, NonceSize), "matrix.5gen.care", "wss://eu.5gen.care:443/"),
		LoginMessage(nonce, "evil.example", "wss://eu.5gen.care:443/"),
		LoginMessage(nonce, "matrix.5gen.care", "wss://us.5gen.care:443/"),
	} {
		if bytes.Equal(base, other) {
			t.Fatal("login message does not depend on every field")
		}
	}
}
