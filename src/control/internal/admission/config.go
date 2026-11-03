package admission

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

var deviceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var tenantPattern = regexp.MustCompile(`^[a-z0-9]{6,8}$`)
var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func ValidDeviceID(value string) bool {
	return deviceIDPattern.MatchString(value)
}

func ValidKeyID(value string) bool { return keyIDPattern.MatchString(value) }

func ValidateAudience(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "wss" || u.Host == "" || u.User != nil || u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("audience must be an exact wss://host[:port]/ origin")
	}
	return nil
}

func ReadPrivateKey(path string) (ed25519.PrivateKey, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("read signing key metadata: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("signing key must be a regular file with no group or other permissions")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("invalid PKCS#8 Ed25519 private key PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("invalid PKCS#8 Ed25519 private key")
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid PKCS#8 Ed25519 private key")
	}
	return key, nil
}

func ReadPublicKey(path string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read verification key: %w", err)
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("invalid PKIX Ed25519 public key PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errors.New("invalid PKIX Ed25519 public key")
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("invalid PKIX Ed25519 public key")
	}
	return key, nil
}
