// Command sidctl generates Ed25519 SIDs and key files.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"signaling-server/internal/sid"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	return runWithInput(args, os.Stdin, stdout, stderr)
}

func runWithInput(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: sidctl <keygen|issue|batch|status|verify> [flags]")
	}
	switch args[0] {
	case "keygen":
		return runKeygen(args[1:], stdout, stderr)
	case "issue":
		return runIssue(args[1:], stdout, stderr)
	case "batch":
		return runBatch(args[1:], stdout, stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
	case "verify":
		return runVerify(args[1:], stdin, stdout, stderr)
	default:
		return fmt.Errorf("unknown sidctl command %q", args[0])
	}
}

func runKeygen(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sidctl keygen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	privatePath := fs.String("private", "", "new PKCS#8 Ed25519 private-key PEM path")
	publicPath := fs.String("public", "", "new PKIX Ed25519 public-key PEM path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) != 0 || *privatePath == "" || *publicPath == "" {
		return errors.New("usage: sidctl keygen --private PATH --public PATH")
	}
	privateAbs, err := filepath.Abs(*privatePath)
	if err != nil {
		return fmt.Errorf("resolve private-key path: %w", err)
	}
	publicAbs, err := filepath.Abs(*publicPath)
	if err != nil {
		return fmt.Errorf("resolve public-key path: %w", err)
	}
	if filepath.Clean(privateAbs) == filepath.Clean(publicAbs) {
		return errors.New("private and public key paths must differ")
	}

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate Ed25519 key: %w", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return fmt.Errorf("encode PKCS#8 private key: %w", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return fmt.Errorf("encode PKIX public key: %w", err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})

	if err := writeExclusive(privateAbs, privatePEM, 0o600); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}
	if err := writeExclusive(publicAbs, publicPEM, 0o644); err != nil {
		cleanupErr := os.Remove(privateAbs)
		if cleanupErr == nil {
			cleanupErr = syncDirectory(filepath.Dir(privateAbs))
		}
		return errors.Join(fmt.Errorf("write public key: %w", err), cleanupErr)
	}
	if err := syncDirectory(filepath.Dir(privateAbs)); err != nil {
		return errors.Join(fmt.Errorf("sync private-key directory: %w", err), cleanupCreated(privateAbs, publicAbs))
	}
	if filepath.Dir(publicAbs) != filepath.Dir(privateAbs) {
		if err := syncDirectory(filepath.Dir(publicAbs)); err != nil {
			return errors.Join(fmt.Errorf("sync public-key directory: %w", err), cleanupCreated(privateAbs, publicAbs))
		}
	}

	fingerprint, err := sid.PublicKeyFingerprint(publicKey)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "public_key_fingerprint_sha256=%s\n", hex.EncodeToString(fingerprint[:]))
	return err
}

func runIssue(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sidctl issue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	privatePath := fs.String("private-key", "", "PKCS#8 Ed25519 private-key PEM path")
	auditPath := fs.String("audit", "", "private JSONL issuance audit file (required)")
	issuedAtText := fs.String("issued-at", "", "issued timestamp in RFC3339 format (required)")
	tenant := fs.String("tenant", "", "tenant, 6-8 characters of [a-z0-9] (required)")
	roleText := fs.String("role", "", "publisher (streams video) or viewer (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) != 0 || *privatePath == "" || *auditPath == "" || *issuedAtText == "" || *tenant == "" || *roleText == "" {
		return errors.New("usage: sidctl issue --private-key PATH --audit PATH --issued-at RFC3339 --tenant NAME --role publisher|viewer")
	}
	flags, err := parseRole(*roleText)
	if err != nil {
		return err
	}
	if err := checkTenant(*tenant); err != nil {
		return err
	}
	issuedAt, err := time.Parse(time.RFC3339, *issuedAtText)
	if err != nil {
		return fmt.Errorf("parse --issued-at as RFC3339: %w", err)
	}
	privateKey, err := readPrivateKey(*privatePath)
	if err != nil {
		return err
	}
	fingerprint, err := sid.PublicKeyFingerprint(privateKey.Public().(ed25519.PublicKey))
	if err != nil {
		return err
	}
	sidText, err := sid.Issue(issuedAt.UTC(), *tenant, flags, privateKey, rand.Reader)
	if err != nil {
		return err
	}
	digest := sid.SIDFingerprint(sidText)
	record := auditRecord{
		DigestSHA256:      hex.EncodeToString(digest[:]),
		Tenant:            *tenant,
		Role:              *roleText,
		IssuedAt:          issuedAt.UTC().Format(time.RFC3339),
		KeyFingerprintSHA: hex.EncodeToString(fingerprint[:]),
	}
	if err := appendAudit(*auditPath, []auditRecord{record}); err != nil {
		return fmt.Errorf("record SID issuance: %w", err)
	}
	if _, err := fmt.Fprintln(stdout, sidText); err != nil {
		return fmt.Errorf("write SID to stdout: %w", err)
	}
	return nil
}

// reservedTenants can never be issued: they are too easy to confuse with
// platform or placeholder names.
var reservedTenants = map[string]bool{
	"system": true, "public": true, "tenant": true, "default": true, "nobody": true,
	"unknown": true, "internal": true, "support": true, "testing": true, "example": true,
	"nulltenant": true,
}

func checkTenant(name string) error {
	if !sid.ValidTenant(name) {
		return sid.ErrInvalidTenant
	}
	if reservedTenants[name] {
		return fmt.Errorf("tenant %q is reserved", name)
	}
	return nil
}

func parseRole(text string) (uint16, error) {
	switch text {
	case "publisher":
		return sid.FlagPublisher, nil
	case "viewer":
		return sid.FlagViewer, nil
	}
	return 0, errors.New("--role must be publisher or viewer")
}

func runVerify(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sidctl verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	publicPath := fs.String("public-key", "", "PKIX Ed25519 public-key PEM path")
	sidPath := fs.String("sid-file", "", "file containing one SID; default reads stdin")
	minIssuedText := fs.String("min-issued", "", "minimum accepted issue time in RFC3339 format (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) != 0 || *publicPath == "" || *minIssuedText == "" {
		return errors.New("usage: sidctl verify --public-key PATH --min-issued RFC3339 [--sid-file PATH]")
	}
	minIssued, err := time.Parse(time.RFC3339, *minIssuedText)
	if err != nil {
		return fmt.Errorf("parse --min-issued as RFC3339: %w", err)
	}
	publicKey, err := readPublicKey(*publicPath)
	if err != nil {
		return err
	}
	input := stdin
	if *sidPath != "" {
		file, err := os.Open(*sidPath)
		if err != nil {
			return fmt.Errorf("open SID input file: %w", err)
		}
		defer file.Close()
		input = file
	}
	sidText, err := readSIDInput(input)
	if err != nil {
		return err
	}
	verified, err := sid.Verify(sidText, publicKey, minIssued)
	if err != nil {
		return err
	}
	role := "viewer"
	if verified.Publisher() {
		role = "publisher"
	}
	result := struct {
		IssuedAt     string `json:"issued_at"`
		Tenant       string `json:"tenant"`
		Role         string `json:"role"`
		Flags        uint16 `json:"flags"`
		Nonce        string `json:"nonce_hex"`
		DigestSHA256 string `json:"digest_sha256_hex"`
	}{
		IssuedAt:     verified.IssuedAt.Format(time.RFC3339),
		Tenant:       verified.Tenant,
		Role:         role,
		Flags:        verified.Flags,
		Nonce:        hex.EncodeToString(verified.Nonce[:]),
		DigestSHA256: hex.EncodeToString(verified.Digest[:]),
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("write verification result: %w", err)
	}
	return nil
}

func readSIDInput(input io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(input, sid.EncodedSize+3))
	if err != nil {
		return "", fmt.Errorf("read SID input: %w", err)
	}
	if len(data) > sid.EncodedSize+2 {
		return "", sid.ErrMalformed
	}
	if bytes.HasSuffix(data, []byte("\r\n")) {
		data = data[:len(data)-2]
	} else if bytes.HasSuffix(data, []byte("\n")) {
		data = data[:len(data)-1]
	}
	if len(data) != sid.EncodedSize {
		return "", sid.ErrMalformed
	}
	return string(data), nil
}

func readPublicKey(path string) (ed25519.PublicKey, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat public key: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("public key must be a regular, non-symlink file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read public key: %w", err)
	}
	trimmed := bytes.TrimSpace(data)
	if !bytes.HasPrefix(trimmed, []byte("-----BEGIN PUBLIC KEY-----")) {
		return nil, errors.New("public key must contain one PKIX PUBLIC KEY PEM block")
	}
	block, rest := pem.Decode(trimmed)
	if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("public key must contain one PKIX PUBLIC KEY PEM block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKIX public key: %w", err)
	}
	publicKey, ok := parsed.(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return nil, sid.ErrInvalidPublic
	}
	return publicKey, nil
}

// auditRecord deliberately omits the SID itself: the audit trail proves what
// was issued without becoming a second place where usable credentials live.
type auditRecord struct {
	DigestSHA256      string `json:"digest_sha256"`
	Tenant            string `json:"tenant"`
	Role              string `json:"role"`
	BatchID           string `json:"batch_id,omitempty"`
	IssuedAt          string `json:"issued_at"`
	KeyFingerprintSHA string `json:"key_fingerprint_sha256"`
}

func readPrivateKey(path string) (ed25519.PrivateKey, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat private key: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("private key must be a regular, non-symlink file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("private key permissions must not allow group or other access")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("private key must contain one PKCS#8 PRIVATE KEY PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKCS#8 private key: %w", err)
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("private key is not a valid Ed25519 key")
	}
	return privateKey, nil
}

func writeExclusive(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	created := true
	defer func() {
		if created {
			_ = os.Remove(path)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return fmt.Errorf("set permissions: %w", err)
	}
	if err := writeAll(f, data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close file: %w", err)
	}
	created = false
	return nil
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func cleanupCreated(paths ...string) error {
	var errs []error
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func appendAudit(path string, records []auditRecord) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("audit path must be a regular, non-symlink file")
		}
		if info.Mode().Perm()&0o077 != 0 {
			return errors.New("audit file permissions must not allow group or other access")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect audit file: %w", err)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open audit file: %w", err)
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("set audit permissions: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat audit file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("audit file must be regular and private")
	}
	startSize := info.Size()
	var line []byte
	for _, record := range records {
		encoded, err := jsonLine(record)
		if err != nil {
			return err
		}
		line = append(line, encoded...)
	}
	if err := writeAll(f, line); err != nil {
		rollbackErr := f.Truncate(startSize)
		if rollbackErr == nil {
			rollbackErr = f.Sync()
		}
		return errors.Join(fmt.Errorf("append audit record: %w", err), rollbackErr)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync audit record: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync audit directory: %w", err)
	}
	return nil
}

func jsonLine(record auditRecord) ([]byte, error) {
	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("encode audit record: %w", err)
	}
	return append(data, '\n'), nil
}
