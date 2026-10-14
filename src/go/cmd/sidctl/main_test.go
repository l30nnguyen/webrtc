package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"signaling-server/internal/sid"
)

func TestKeygenCreatesPrivateFilesWithoutOverwriting(t *testing.T) {
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "issuer.key.pem")
	publicPath := filepath.Join(dir, "issuer.pub.pem")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"keygen", "--private", privatePath, "--public", publicPath}, &stdout, &stderr); err != nil {
		t.Fatalf("keygen: %v; stderr=%s", err, stderr.String())
	}
	if strings.Contains(stdout.String(), "PRIVATE KEY") || strings.Contains(stdout.String(), "BEGIN") {
		t.Fatal("keygen printed private key material")
	}
	privateInfo, err := os.Stat(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	if privateInfo.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode = %04o, want 0600", privateInfo.Mode().Perm())
	}
	publicInfo, err := os.Stat(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	if publicInfo.Mode().Perm() != 0o644 {
		t.Fatalf("public key mode = %04o, want 0644", publicInfo.Mode().Perm())
	}
	privateBefore, err := os.ReadFile(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	publicBefore, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := run([]string{"keygen", "--private", privatePath, "--public", publicPath}, &stdout, &stderr); err == nil {
		t.Fatal("keygen unexpectedly overwrote existing paths")
	}
	privateAfter, err := os.ReadFile(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	publicAfter, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(privateBefore, privateAfter) || !bytes.Equal(publicBefore, publicAfter) {
		t.Fatal("keygen modified existing key material")
	}
}

func TestKeygenRemovesOnlyNewPrivateFileIfPublicPathExists(t *testing.T) {
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "new-private.pem")
	publicPath := filepath.Join(dir, "existing-public.pem")
	if err := os.WriteFile(publicPath, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := run([]string{"keygen", "--private", privatePath, "--public", publicPath}, &stdout, &stderr)
	if err == nil {
		t.Fatal("keygen unexpectedly succeeded with existing public path")
	}
	if _, err := os.Lstat(privatePath); !os.IsNotExist(err) {
		t.Fatalf("new private key left after failed pair creation: %v", err)
	}
	got, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep me" {
		t.Fatalf("existing public path changed: %q", got)
	}
}

func TestIssueWritesDurablePrivateAuditRecordBeforeEmittingSID(t *testing.T) {
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "issuer.key.pem")
	publicPath := filepath.Join(dir, "issuer.pub.pem")
	auditPath := filepath.Join(dir, "issued.jsonl")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"keygen", "--private", privatePath, "--public", publicPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := run([]string{
		"issue", "--private-key", privatePath, "--audit", auditPath,
		"--issued-at", "2026-10-09T05:06:07+07:00", "--tenant", "acme01", "--role", "publisher",
	}, &stdout, &stderr); err != nil {
		t.Fatalf("issue: %v; stderr=%s", err, stderr.String())
	}
	sidText := strings.TrimSpace(stdout.String())
	if len(sidText) != 118 || strings.Contains(sidText, "=") {
		t.Fatalf("issued SID format invalid: length=%d value=%q", len(sidText), sidText)
	}
	auditInfo, err := os.Stat(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if auditInfo.Mode().Perm() != 0o600 {
		t.Fatalf("audit mode = %04o, want 0600", auditInfo.Mode().Perm())
	}
	auditBytes, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	var record auditRecord
	if err := json.Unmarshal(bytes.TrimSpace(auditBytes), &record); err != nil {
		t.Fatalf("decode audit record: %v", err)
	}
	digest := sha256.Sum256([]byte(sidText))
	if record.DigestSHA256 != hex.EncodeToString(digest[:]) || record.IssuedAt != "2026-10-08T22:06:07Z" || record.Tenant != "acme01" || record.Role != "publisher" {
		t.Fatalf("unexpected audit record: %+v", record)
	}
	if bytes.Contains(auditBytes, []byte(sidText)) {
		t.Fatal("audit file contains the raw SID; it must hold digests only")
	}
	if len(record.KeyFingerprintSHA) != 64 {
		t.Fatalf("key fingerprint length = %d, want 64 hex chars", len(record.KeyFingerprintSHA))
	}
	if _, err := hex.DecodeString(record.KeyFingerprintSHA); err != nil {
		t.Fatalf("invalid key fingerprint: %v", err)
	}
	publicPEM, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(publicPEM)
	if block == nil {
		t.Fatal("public key PEM missing")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := parsed.(ed25519.PublicKey)
	raw, err := base64.RawURLEncoding.DecodeString(sidText)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(publicKey, raw[:sid.PayloadSize], raw[sid.PayloadSize:]) {
		t.Fatal("issued SID signature did not verify")
	}
}

func TestIssueDoesNotEmitSIDWhenAuditCannotBeWritten(t *testing.T) {
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "issuer.key.pem")
	publicPath := filepath.Join(dir, "issuer.pub.pem")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"keygen", "--private", privatePath, "--public", publicPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	err := run([]string{
		"issue", "--private-key", privatePath,
		"--audit", filepath.Join(dir, "missing", "audit.jsonl"),
		"--issued-at", "2026-10-09T00:00:00Z", "--tenant", "acme01", "--role", "publisher",
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("issue unexpectedly succeeded without writable audit path")
	}
	if stdout.Len() != 0 {
		t.Fatalf("SID emitted despite failed audit: %q", stdout.String())
	}
}

func TestIssueRejectsPermissiveOrSymlinkAudit(t *testing.T) {
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "issuer.key.pem")
	publicPath := filepath.Join(dir, "issuer.pub.pem")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"keygen", "--private", privatePath, "--public", publicPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		prep func(string) error
	}{
		{name: "permissive", prep: func(path string) error { return os.WriteFile(path, nil, 0o640) }},
		{name: "symlink", prep: func(path string) error { return os.Symlink(filepath.Join(dir, "target"), path) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
			if err := tc.prep(auditPath); err != nil {
				t.Fatal(err)
			}
			stdout.Reset()
			err := run([]string{
				"issue", "--private-key", privatePath, "--audit", auditPath,
				"--issued-at", "2026-10-09T00:00:00Z", "--tenant", "acme01", "--role", "publisher",
			}, &stdout, &stderr)
			if err == nil {
				t.Fatal("issue unexpectedly accepted unsafe audit path")
			}
			if stdout.Len() != 0 {
				t.Fatalf("SID emitted for unsafe audit path: %q", stdout.String())
			}
		})
	}
}

func TestIssueRequiresExplicitAudit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"issue", "--private-key", "key.pem", "--issued-at", "2026-10-09T00:00:00Z"}, &stdout, &stderr); err == nil {
		t.Fatal("issue unexpectedly succeeded without required audit path")
	}
	if stdout.Len() != 0 {
		t.Fatalf("unexpected output: %q", stdout.String())
	}
}

func TestVerifyCLIReadsStdinAndFileAndOmitsSID(t *testing.T) {
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "issuer.key.pem")
	publicPath := filepath.Join(dir, "issuer.pub.pem")
	auditPath := filepath.Join(dir, "issued.jsonl")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"keygen", "--private", privatePath, "--public", publicPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := run([]string{
		"issue", "--private-key", privatePath, "--audit", auditPath,
		"--issued-at", "2026-10-09T05:06:07Z", "--tenant", "acme01", "--role", "viewer",
	}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	sidText := strings.TrimSpace(stdout.String())
	verifyArgs := []string{"verify", "--public-key", publicPath, "--min-issued", "2026-10-09T05:06:07Z"}
	for _, input := range []string{sidText, sidText + "\n", sidText + "\r\n"} {
		stdout.Reset()
		if err := runWithInput(verifyArgs, strings.NewReader(input), &stdout, &stderr); err != nil {
			t.Fatalf("verify stdin with input length %d: %v", len(input), err)
		}
		if strings.Contains(stdout.String(), sidText) {
			t.Fatal("verify output exposed the SID")
		}
		var result struct {
			IssuedAt     string `json:"issued_at"`
			Tenant       string `json:"tenant"`
			Role         string `json:"role"`
			Flags        uint16 `json:"flags"`
			Nonce        string `json:"nonce_hex"`
			DigestSHA256 string `json:"digest_sha256_hex"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatalf("decode verify output: %v", err)
		}
		if result.IssuedAt != "2026-10-09T05:06:07Z" || result.Tenant != "acme01" || result.Role != "viewer" || result.Flags != sid.FlagViewer || len(result.Nonce) != 18 || len(result.DigestSHA256) != 64 {
			t.Fatalf("unexpected verification result: %+v", result)
		}
	}
	sidFile := filepath.Join(dir, "input.sid")
	if err := os.WriteFile(sidFile, []byte(sidText+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := runWithInput(append(verifyArgs, "--sid-file", sidFile), strings.NewReader("ignored"), &stdout, &stderr); err != nil {
		t.Fatalf("verify SID file: %v", err)
	}
	if strings.Contains(stdout.String(), sidText) {
		t.Fatal("file verification output exposed the SID")
	}
}

func TestVerifyCLIRejectsExtraLinesOversizeAndMalformedPublicKey(t *testing.T) {
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "issuer.key.pem")
	publicPath := filepath.Join(dir, "issuer.pub.pem")
	auditPath := filepath.Join(dir, "issued.jsonl")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"keygen", "--private", privatePath, "--public", publicPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := run([]string{"issue", "--private-key", privatePath, "--audit", auditPath, "--issued-at", "2026-10-09T05:06:07Z", "--tenant", "acme01", "--role", "publisher"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	sidText := strings.TrimSpace(stdout.String())
	args := []string{"verify", "--public-key", publicPath, "--min-issued", "2026-10-09T00:00:00Z"}
	for _, input := range []string{sidText + "\n\n", sidText + strings.Repeat("x", 3)} {
		stdout.Reset()
		err := runWithInput(args, strings.NewReader(input), &stdout, &stderr)
		if err == nil {
			t.Fatal("verify unexpectedly accepted extra input")
		}
		if stdout.Len() != 0 || strings.Contains(err.Error(), sidText) {
			t.Fatalf("unsafe error/output: err=%q stdout=%q", err, stdout.String())
		}
	}
	badKeyPath := filepath.Join(dir, "two-blocks.pem")
	publicData, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badKeyPath, append(publicData, publicData...), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := runWithInput([]string{"verify", "--public-key", badKeyPath, "--min-issued", "2026-10-09T00:00:00Z"}, strings.NewReader(sidText), &stdout, &stderr); err == nil {
		t.Fatal("verify unexpectedly accepted multiple PEM blocks")
	}
	if stdout.Len() != 0 {
		t.Fatalf("unexpected output for malformed public key: %q", stdout.String())
	}
}

func TestVerifyCLIFailureDoesNotEchoSID(t *testing.T) {
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "issuer.key.pem")
	publicPath := filepath.Join(dir, "issuer.pub.pem")
	auditPath := filepath.Join(dir, "issued.jsonl")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"keygen", "--private", privatePath, "--public", publicPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := run([]string{"issue", "--private-key", privatePath, "--audit", auditPath, "--issued-at", "2026-10-09T05:06:07Z", "--tenant", "acme01", "--role", "publisher"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	sidText := strings.TrimSpace(stdout.String())
	raw, err := base64.RawURLEncoding.DecodeString(sidText)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	tampered := base64.RawURLEncoding.EncodeToString(raw)
	stdout.Reset()
	err = runWithInput([]string{"verify", "--public-key", publicPath, "--min-issued", "2026-10-09T00:00:00Z"}, strings.NewReader(tampered), &stdout, &stderr)
	if err == nil {
		t.Fatal("verify unexpectedly accepted tampered signature")
	}
	if stdout.Len() != 0 || strings.Contains(err.Error(), sidText) || strings.Contains(err.Error(), tampered) {
		t.Fatalf("verification failure exposed SID: err=%q stdout=%q", err, stdout.String())
	}
}

func TestIssueRejectsBadTenantAndRole(t *testing.T) {
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "issuer.key.pem")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"keygen", "--private", privatePath, "--public", filepath.Join(dir, "issuer.pub.pem")}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, tenant, role string }{
		{"short tenant", "abc", "publisher"},
		{"long tenant", "abcdefghi", "publisher"},
		{"uppercase tenant", "ACME01", "publisher"},
		{"symbol tenant", "acme-01", "publisher"},
		{"reserved tenant", "system", "publisher"},
		{"no role", "acme01", ""},
		{"unknown role", "acme01", "camera"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout.Reset()
			args := []string{"issue", "--private-key", privatePath, "--audit", filepath.Join(dir, "audit.jsonl"), "--issued-at", "2026-10-09T00:00:00Z", "--tenant", tc.tenant}
			if tc.role != "" {
				args = append(args, "--role", tc.role)
			}
			if err := run(args, &stdout, &stderr); err == nil || stdout.Len() != 0 {
				t.Fatalf("accepted %+v: err=%v stdout=%q", tc, err, stdout.String())
			}
		})
	}
	if _, err := os.Stat(filepath.Join(dir, "audit.jsonl")); err == nil {
		t.Fatal("audit file written for rejected requests")
	}
}
