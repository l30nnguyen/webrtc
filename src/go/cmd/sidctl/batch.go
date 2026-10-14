package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"time"

	"signaling-server/internal/activation"
	"signaling-server/internal/sid"
)

// MaxBatch keeps each sheet well under the 1,048,576-row Excel limit and each
// batch small enough to hand to one factory run.
const MaxBatch = 100000

var batchIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,40}$`)

var batchHeader = []string{"batch_id", "tenant", "role", "sid_digest", "sid", "issued_at", "status", "delivered_to"}
var manifestHeader = []string{"batch_id", "tenant", "role", "sid_digest", "issued_at"}
var statusHeader = []string{"batch_id", "tenant", "role", "sid_digest", "status", "device_id", "activated_at"}

// runBatch releases a batch of SIDs for one tenant and role. It writes:
//
//	PREFIX.csv           the sheet for the factory, with raw SIDs (secret)
//	PREFIX.xlsx          the same sheet for people
//	PREFIX.manifest.csv  digests only, safe to share with operators and import
//
// The audit file gets one digest-only record per SID before any file appears,
// and nothing is written if any step fails.
func runBatch(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sidctl batch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	privatePath := fs.String("private-key", "", "PKCS#8 Ed25519 private-key PEM path")
	auditPath := fs.String("audit", "", "private JSONL issuance audit file (required)")
	tenant := fs.String("tenant", "", "tenant, 6-8 characters of [a-z0-9] (required)")
	roleText := fs.String("role", "", "publisher (streams video) or viewer (required)")
	count := fs.Int("count", 0, fmt.Sprintf("number of SIDs, 1..%d (required)", MaxBatch))
	issuedAtText := fs.String("issued-at", "", "issued timestamp in RFC3339 format (default: now)")
	batchID := fs.String("batch-id", "", "batch identifier (default: tenant-date-random)")
	outPrefix := fs.String("out", "", "output path prefix, e.g. /secure/acme01-001 (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) != 0 || *privatePath == "" || *auditPath == "" || *tenant == "" || *roleText == "" || *outPrefix == "" || *count < 1 || *count > MaxBatch {
		return fmt.Errorf("usage: sidctl batch --private-key PATH --audit PATH --tenant NAME --role publisher|viewer --count 1..%d --out PREFIX [--issued-at RFC3339] [--batch-id ID]", MaxBatch)
	}
	flags, err := parseRole(*roleText)
	if err != nil {
		return err
	}
	if err := checkTenant(*tenant); err != nil {
		return err
	}
	issuedAt := time.Now().UTC().Truncate(time.Second)
	if *issuedAtText != "" {
		if issuedAt, err = time.Parse(time.RFC3339, *issuedAtText); err != nil {
			return fmt.Errorf("parse --issued-at as RFC3339: %w", err)
		}
		issuedAt = issuedAt.UTC()
	}
	id := *batchID
	if id == "" {
		suffix := make([]byte, 2)
		if _, err := rand.Read(suffix); err != nil {
			return err
		}
		id = fmt.Sprintf("%s-%s-%s", *tenant, issuedAt.Format("20060102"), hex.EncodeToString(suffix))
	}
	if !batchIDPattern.MatchString(id) {
		return errors.New("--batch-id must be 1-40 characters of [A-Za-z0-9._-]")
	}
	paths := []string{*outPrefix + ".csv", *outPrefix + ".xlsx", *outPrefix + ".manifest.csv"}
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("refusing to overwrite %s", path)
		}
	}
	privateKey, err := readPrivateKey(*privatePath)
	if err != nil {
		return err
	}
	fingerprint, err := sid.PublicKeyFingerprint(privateKey.Public().(ed25519.PublicKey))
	if err != nil {
		return err
	}

	sheet := [][]string{batchHeader}
	manifest := [][]string{manifestHeader}
	audit := make([]auditRecord, 0, *count)
	issued := issuedAt.Format(time.RFC3339)
	for i := 0; i < *count; i++ {
		sidText, err := sid.Issue(issuedAt, *tenant, flags, privateKey, rand.Reader)
		if err != nil {
			return err
		}
		digest := sid.SIDFingerprint(sidText)
		digestHex := hex.EncodeToString(digest[:])
		sheet = append(sheet, []string{id, *tenant, *roleText, digestHex, sidText, issued, "unassigned", ""})
		manifest = append(manifest, []string{id, *tenant, *roleText, digestHex, issued})
		audit = append(audit, auditRecord{DigestSHA256: digestHex, Tenant: *tenant, Role: *roleText, BatchID: id, IssuedAt: issued, KeyFingerprintSHA: hex.EncodeToString(fingerprint[:])})
	}

	sheetCSV, err := encodeCSV(sheet)
	if err != nil {
		return err
	}
	manifestCSV, err := encodeCSV(manifest)
	if err != nil {
		return err
	}
	workbook, err := writeXLSX("SIDs "+id, sheet)
	if err != nil {
		return err
	}
	// Audit first: a SID that was never recorded must never leave this process.
	if err := appendAudit(*auditPath, audit); err != nil {
		return fmt.Errorf("record SID issuance: %w", err)
	}
	var created []string
	for i, data := range [][]byte{sheetCSV, workbook, manifestCSV} {
		mode := os.FileMode(0o600)
		if i == 2 {
			mode = 0o640 // digests only
		}
		if err := writeExclusive(paths[i], data, mode); err != nil {
			return errors.Join(fmt.Errorf("write %s: %w", paths[i], err), cleanupCreated(created...))
		}
		created = append(created, paths[i])
	}
	_, err = fmt.Fprintf(stdout, "batch_id=%s tenant=%s role=%s count=%d\nsheet=%s\nworkbook=%s\nmanifest=%s\n", id, *tenant, *roleText, *count, paths[0], paths[1], paths[2])
	return err
}

func encodeCSV(rows [][]string) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.WriteAll(rows); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// runStatus joins a batch manifest with Center's activation store to show,
// per SID, whether a device has claimed it. It never needs the raw SIDs.
func runStatus(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sidctl status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	manifestPath := fs.String("manifest", "", "batch manifest CSV (PREFIX.manifest.csv) (required)")
	activationsPath := fs.String("activations", "", "Center activation store file (required)")
	outPath := fs.String("out", "", "write the status CSV to this new file (default: stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) != 0 || *manifestPath == "" || *activationsPath == "" {
		return errors.New("usage: sidctl status --manifest PATH --activations PATH [--out PATH]")
	}
	file, err := os.Open(*manifestPath)
	if err != nil {
		return fmt.Errorf("open manifest: %w", err)
	}
	defer file.Close()
	reader := csv.NewReader(file)
	reader.FieldsPerRecord = len(manifestHeader)
	rows, err := reader.ReadAll()
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	if len(rows) == 0 || !equalRow(rows[0], manifestHeader) {
		return errors.New("manifest header is not a sidctl batch manifest")
	}
	records, err := activation.ReadAll(*activationsPath)
	if err != nil {
		return err
	}
	byDigest := make(map[string]activation.Record, len(records))
	for _, record := range records {
		byDigest[record.Digest] = record
	}

	out := [][]string{statusHeader}
	type counts struct{ total, activated, released int }
	perBatch := map[string]*counts{}
	for _, row := range rows[1:] {
		batch := row[0]
		if perBatch[batch] == nil {
			perBatch[batch] = &counts{}
		}
		perBatch[batch].total++
		status, deviceID, activatedAt := "unactivated", "", ""
		if record, ok := byDigest[row[3]]; ok {
			activatedAt = time.Unix(record.ActivatedAt, 0).UTC().Format(time.RFC3339)
			if record.Bound() {
				status, deviceID = "activated", record.DeviceID
				perBatch[batch].activated++
			} else {
				// Released to the pool: available again, no device.
				status = "released"
				perBatch[batch].released++
			}
		}
		out = append(out, []string{row[0], row[1], row[2], row[3], status, deviceID, activatedAt})
	}
	data, err := encodeCSV(out)
	if err != nil {
		return err
	}
	if *outPath != "" {
		if err := writeExclusive(*outPath, data, 0o640); err != nil {
			return fmt.Errorf("write status: %w", err)
		}
	} else if _, err := stdout.Write(data); err != nil {
		return err
	}
	batches := make([]string, 0, len(perBatch))
	for batch := range perBatch {
		batches = append(batches, batch)
	}
	sort.Strings(batches)
	for _, batch := range batches {
		c := perBatch[batch]
		fmt.Fprintf(stderr, "batch %s: %d SIDs, %d activated, %d released, %d unactivated\n", batch, c.total, c.activated, c.released, c.total-c.activated-c.released)
	}
	return nil
}

func equalRow(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
