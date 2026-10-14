package main

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"signaling-server/internal/activation"
	"signaling-server/internal/sid"
)

type batchFixture struct {
	dir, privatePath, publicPath, auditPath string
	public                                  ed25519.PublicKey
}

func newBatchFixture(t *testing.T) batchFixture {
	t.Helper()
	dir := t.TempDir()
	f := batchFixture{dir: dir, privatePath: filepath.Join(dir, "issuer.key.pem"), publicPath: filepath.Join(dir, "issuer.pub.pem"), auditPath: filepath.Join(dir, "audit.jsonl")}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"keygen", "--private", f.privatePath, "--public", f.publicPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var err error
	if f.public, err = readPublicKey(f.publicPath); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f batchFixture) batch(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args := append([]string{"batch", "--private-key", f.privatePath, "--audit", f.auditPath, "--issued-at", "2026-10-09T00:00:00Z"}, extra...)
	err := run(args, &stdout, &stderr)
	return stdout.String(), err
}

func readCSV(t *testing.T, path string) [][]string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	rows, err := csv.NewReader(file).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestBatchWritesSheetWorkbookManifestAndDigestOnlyAudit(t *testing.T) {
	f := newBatchFixture(t)
	prefix := filepath.Join(f.dir, "acme01-001")
	out, err := f.batch(t, "--tenant", "acme01", "--role", "publisher", "--count", "25", "--batch-id", "acme01-001", "--out", prefix)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "sid=") && len(out) > 400 {
		t.Fatalf("batch printed too much: %q", out)
	}

	sheet := readCSV(t, prefix+".csv")
	if len(sheet) != 26 || strings.Join(sheet[0], ",") != strings.Join(batchHeader, ",") {
		t.Fatalf("sheet shape: %d rows, header %v", len(sheet), sheet[0])
	}
	seen := map[string]bool{}
	for _, row := range sheet[1:] {
		verified, err := sid.Verify(row[4], f.public, time.Unix(0, 0))
		if err != nil {
			t.Fatalf("row SID does not verify: %v", err)
		}
		if verified.Tenant != "acme01" || !verified.Publisher() || row[0] != "acme01-001" || row[1] != "acme01" || row[2] != "publisher" || row[6] != "unassigned" || row[7] != "" {
			t.Fatalf("unexpected row %v / %+v", row, verified)
		}
		sum := sha256.Sum256([]byte(row[4]))
		if row[3] != hex.EncodeToString(sum[:]) {
			t.Fatalf("digest column does not match the SID: %v", row)
		}
		if seen[row[4]] {
			t.Fatal("duplicate SID in a batch")
		}
		seen[row[4]] = true
	}

	manifestBytes, _ := os.ReadFile(prefix + ".manifest.csv")
	for sidText := range seen {
		if strings.Contains(string(manifestBytes), sidText) {
			t.Fatal("manifest contains a raw SID")
		}
	}
	if rows := readCSV(t, prefix+".manifest.csv"); len(rows) != 26 || len(rows[0]) != len(manifestHeader) {
		t.Fatalf("manifest shape: %d rows", len(rows))
	}

	auditBytes, _ := os.ReadFile(f.auditPath)
	for sidText := range seen {
		if strings.Contains(string(auditBytes), sidText) {
			t.Fatal("audit contains a raw SID")
		}
	}
	lines := strings.Split(strings.TrimSpace(string(auditBytes)), "\n")
	if len(lines) != 25 {
		t.Fatalf("audit lines = %d, want 25", len(lines))
	}
	var rec auditRecord
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil || rec.BatchID != "acme01-001" || rec.Tenant != "acme01" {
		t.Fatalf("audit record = %+v %v", rec, err)
	}

	for _, path := range []string{prefix + ".csv", prefix + ".xlsx"} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v (%v), want 0600", path, info.Mode().Perm(), err)
		}
	}

	// the workbook carries the same data
	cells := readXLSXCells(t, prefix+".xlsx")
	if len(cells) != 26 || cells[1][4] != sheet[1][4] || cells[0][0] != "batch_id" {
		t.Fatalf("workbook content differs from the sheet: rows=%d", len(cells))
	}
}

// readXLSXCells reads the inline-string cells of sheet1 back out of a workbook.
func readXLSXCells(t *testing.T, path string) [][]string {
	t.Helper()
	archive, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("workbook is not a zip: %v", err)
	}
	defer archive.Close()
	names := map[string]bool{}
	var sheet io.ReadCloser
	for _, f := range archive.File {
		names[f.Name] = true
		if f.Name == "xl/worksheets/sheet1.xml" {
			if sheet, err = f.Open(); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, want := range []string{"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml", "xl/_rels/workbook.xml.rels"} {
		if !names[want] {
			t.Fatalf("workbook is missing %s", want)
		}
	}
	defer sheet.Close()
	var doc struct {
		Rows []struct {
			Cells []struct {
				Ref  string `xml:"r,attr"`
				Text string `xml:"is>t"`
			} `xml:"c"`
		} `xml:"sheetData>row"`
	}
	if err := xml.NewDecoder(sheet).Decode(&doc); err != nil {
		t.Fatalf("sheet1.xml is not valid XML: %v", err)
	}
	var rows [][]string
	for _, r := range doc.Rows {
		row := make([]string, 8)
		for _, c := range r.Cells {
			col := int(c.Ref[0] - 'A')
			row[col] = c.Text
		}
		rows = append(rows, row)
	}
	return rows
}

func TestBatchRefusesBadInputAndNeverOverwrites(t *testing.T) {
	f := newBatchFixture(t)
	prefix := filepath.Join(f.dir, "b")
	for name, extra := range map[string][]string{
		"zero count":      {"--tenant", "acme01", "--role", "publisher", "--count", "0", "--out", prefix},
		"too many":        {"--tenant", "acme01", "--role", "publisher", "--count", "100001", "--out", prefix},
		"bad tenant":      {"--tenant", "ACME", "--role", "publisher", "--count", "1", "--out", prefix},
		"reserved tenant": {"--tenant", "public", "--role", "publisher", "--count", "1", "--out", prefix},
		"bad role":        {"--tenant", "acme01", "--role", "x", "--count", "1", "--out", prefix},
		"bad batch id":    {"--tenant", "acme01", "--role", "viewer", "--count", "1", "--batch-id", "a b", "--out", prefix},
		"no out":          {"--tenant", "acme01", "--role", "viewer", "--count", "1"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.batch(t, extra...); err == nil {
				t.Fatal("bad batch accepted")
			}
		})
	}
	if _, err := os.Stat(f.auditPath); err == nil {
		t.Fatal("audit written for rejected batches")
	}
	good := []string{"--tenant", "acme01", "--role", "viewer", "--count", "2", "--out", prefix}
	if _, err := f.batch(t, good...); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(prefix + ".csv")
	if _, err := f.batch(t, good...); err == nil {
		t.Fatal("second batch overwrote the first")
	}
	after, _ := os.ReadFile(prefix + ".csv")
	if !bytes.Equal(before, after) {
		t.Fatal("existing sheet was modified")
	}
}

func TestBatchWritesNothingIfAuditFails(t *testing.T) {
	f := newBatchFixture(t)
	f.auditPath = filepath.Join(f.dir, "missing-dir", "audit.jsonl")
	prefix := filepath.Join(f.dir, "b")
	if _, err := f.batch(t, "--tenant", "acme01", "--role", "publisher", "--count", "3", "--out", prefix); err == nil {
		t.Fatal("batch succeeded without a writable audit")
	}
	for _, ext := range []string{".csv", ".xlsx", ".manifest.csv"} {
		if _, err := os.Stat(prefix + ext); err == nil {
			t.Fatalf("%s written although the audit failed", ext)
		}
	}
}

func TestStatusJoinsManifestWithActivations(t *testing.T) {
	f := newBatchFixture(t)
	prefix := filepath.Join(f.dir, "acme01-001")
	if _, err := f.batch(t, "--tenant", "acme01", "--role", "publisher", "--count", "3", "--batch-id", "acme01-001", "--out", prefix); err != nil {
		t.Fatal(err)
	}
	manifest := readCSV(t, prefix+".manifest.csv")
	store, err := activation.Open(filepath.Join(f.dir, "activations.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	activatedDigest := manifest[2][3]
	if _, _, err := store.Bind(activation.Record{Digest: activatedDigest, Tenant: "acme01", Role: "camera", DeviceID: "device-xyz", DevicePub: "p", CameraRef: "r", ActivatedAt: 1_790_000_000}, time.Now()); err != nil {
		t.Fatal(err)
	}
	store.Close()

	var stdout, stderr bytes.Buffer
	if err := run([]string{"status", "--manifest", prefix + ".manifest.csv", "--activations", filepath.Join(f.dir, "activations.jsonl")}, &stdout, &stderr); err != nil {
		t.Fatalf("status: %v", err)
	}
	rows, err := csv.NewReader(&stdout).ReadAll()
	if err != nil || len(rows) != 4 {
		t.Fatalf("status rows = %d (%v)", len(rows), err)
	}
	counts := map[string]int{}
	for _, row := range rows[1:] {
		counts[row[4]]++
		if row[3] == activatedDigest && (row[4] != "activated" || row[5] != "device-xyz" || row[6] == "") {
			t.Fatalf("activated row wrong: %v", row)
		}
	}
	if counts["activated"] != 1 || counts["unactivated"] != 2 {
		t.Fatalf("counts = %v", counts)
	}
	if !strings.Contains(stderr.String(), "3 SIDs, 1 activated, 0 released, 2 unactivated") {
		t.Fatalf("summary = %q", stderr.String())
	}

	// into a new file, never over an existing one
	out := filepath.Join(f.dir, "status.csv")
	args := []string{"status", "--manifest", prefix + ".manifest.csv", "--activations", filepath.Join(f.dir, "activations.jsonl"), "--out", out}
	if err := run(args, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if err := run(args, &stdout, &stderr); err == nil {
		t.Fatal("status overwrote an existing output file")
	}
	// a non-manifest file is refused
	if err := run([]string{"status", "--manifest", prefix + ".csv", "--activations", filepath.Join(f.dir, "activations.jsonl")}, &stdout, &stderr); err == nil {
		t.Fatal("the raw-SID sheet was accepted as a manifest")
	}
}

func TestWriteXLSXEscapesAndKeepsFormulaLookalikesAsText(t *testing.T) {
	data, err := writeXLSX(`Sheet <"1">&`, [][]string{{"=1+1", "a<b&c", `"q"`, "", "-x"}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "x.xlsx")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	row := readXLSXCells(t, path)[0]
	if row[0] != "=1+1" || row[1] != "a<b&c" || row[2] != `"q"` || row[3] != "" || row[4] != "-x" {
		t.Fatalf("cells = %q", row)
	}
	if strings.Contains(string(data), "<f>") {
		t.Fatal("workbook contains formula elements")
	}
	for i, want := range map[int]string{0: "A", 25: "Z", 26: "AA", 27: "AB", 701: "ZZ", 702: "AAA"} {
		if got := columnName(i); got != want {
			t.Fatalf("columnName(%d) = %s, want %s", i, got, want)
		}
	}
}
