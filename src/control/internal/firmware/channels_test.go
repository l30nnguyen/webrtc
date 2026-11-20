package firmware

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// touch moves the file's time on so a rewrite in the same second is seen.
func touch(t *testing.T, path string, n int) {
	t.Helper()
	when := time.Now().Add(time.Duration(n) * time.Second)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

const sample = `{"firmware": {"HCX130": {"stable": "2.2.2", "canary": "2.2.3", "previous": "2.2.1"},
  "anycloud": {"stable": "2.2.2", "canary": "2.2.3"}, "GIRAFFE_HD": {"stable": "2.2.2"}},
 "canary": {"ips": ["115.78.12.186", "10.1.0.0/16"], "tenants": ["test01"], "sids": ["3fa1c2d4e5f6"]}}`

func TestPickByGroup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "firmware-channels.json")
	write(t, path, sample)
	c := New(path, t.Logf)
	cases := []struct {
		name string
		who  Who
		want string
	}{
		{"HCX130", Who{Addr: netip.MustParseAddr("115.78.12.186")}, "2.2.3"},
		{"HCX130", Who{Addr: netip.MustParseAddr("::ffff:115.78.12.186")}, "2.2.3"},
		{"HCX130", Who{Addr: netip.MustParseAddr("10.1.9.9")}, "2.2.3"},
		{"HCX130", Who{Addr: netip.MustParseAddr("8.8.8.8")}, "2.2.2"},
		{"anycloud", Who{Addr: netip.MustParseAddr("8.8.8.8"), Tenant: "test01"}, "2.2.3"},
		{"anycloud", Who{Addr: netip.MustParseAddr("8.8.8.8"), Tenant: "other", SIDDigest: "3fa1c2d4e5f6aa"}, "2.2.3"},
		{"anycloud", Who{Addr: netip.MustParseAddr("8.8.8.8"), Tenant: "other", SIDDigest: "4fa1c2d4e5f6aa"}, "2.2.2"},
		{"GIRAFFE_HD", Who{Addr: netip.MustParseAddr("115.78.12.186")}, "2.2.2"}, // no canary set
		{"HCX330L", Who{Addr: netip.MustParseAddr("115.78.12.186")}, ""},         // not listed
	}
	for _, tc := range cases {
		if got, _ := c.Pick(tc.name, tc.who); got != tc.want {
			t.Errorf("Pick(%s, %+v) = %q, want %q", tc.name, tc.who, got, tc.want)
		}
	}
}

func TestMissingFileAndBadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "firmware-channels.json")
	var logs []string
	c := New(path, func(f string, a ...any) { logs = append(logs, f) })
	if v, _ := c.Pick("HCX130", Who{}); v != "" {
		t.Fatalf("no file: %q", v)
	}
	write(t, path, sample)
	if v, _ := c.Pick("HCX130", Who{}); v != "2.2.2" {
		t.Fatalf("stable: %q", v)
	}
	write(t, path, `{"firmware": {"HCX130": {"stable": "../etc"}}}`)
	touch(t, path, 5)
	if v, _ := c.Pick("HCX130", Who{}); v != "2.2.2" {
		t.Fatalf("a bad file must keep the last good one: %q", v)
	}
	if len(logs) == 0 {
		t.Fatal("a bad file must be logged")
	}
	write(t, path, `{"firmware": {"HCX130": {"stable": "2.2.4"}}}`)
	touch(t, path, 10)
	if v, _ := c.Pick("HCX130", Who{}); v != "2.2.4" {
		t.Fatalf("reload: %q", v)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if v, _ := c.Pick("HCX130", Who{}); v != "" {
		t.Fatalf("removed file must turn channels off: %q", v)
	}
}

func TestParseRefuses(t *testing.T) {
	for _, body := range []string{
		`{"firmware": {"HCX130": {"stable": ""}}}`,
		`{"firmware": {"HC/X": {"stable": "2.2.2"}}}`,
		`{"firmware": {"HCX130": {"stable": "2.2.2", "canary": "x"}}}`,
		`{"firmware": {}, "canary": {"ips": ["not-an-ip"]}}`,
		`{"firmware": {}, "canary": {"sids": ["ABC"]}}`,
		`{"firmware": {}, "unknown": 1}`,
	} {
		if _, _, err := Parse([]byte(body)); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
}

func TestResolveFallbacks(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "downloads")
	write(t, base+"/HCX130/matrix", "flat")
	write(t, base+"/HCX130/2.2.2/matrix", "stable")
	write(t, base+"/HCX130/2.2.3/matrix", "canary")
	path := filepath.Join(dir, "firmware-channels.json")
	write(t, path, sample)
	c := New(path, t.Logf)
	office := Who{Addr: netip.MustParseAddr("115.78.12.186")}
	other := Who{Addr: netip.MustParseAddr("8.8.8.8")}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	if p, v, ch := c.Resolve(base, "HCX130", "matrix", office); read(p) != "canary" || v != "2.2.3" || ch != Canary {
		t.Fatalf("office: %s %s %s", p, v, ch)
	}
	if p, _, ch := c.Resolve(base, "HCX130", "matrix", other); read(p) != "stable" || ch != Stable {
		t.Fatalf("other: %s %s", p, ch)
	}
	os.RemoveAll(base + "/HCX130/2.2.3")
	if p, v, _ := c.Resolve(base, "HCX130", "matrix", office); read(p) != "stable" || v != "2.2.2" {
		t.Fatalf("missing canary copy must fall back to stable: %s %s", p, v)
	}
	os.RemoveAll(base + "/HCX130/2.2.2")
	if p, v, _ := c.Resolve(base, "HCX130", "matrix", office); read(p) != "flat" || v != "" {
		t.Fatalf("missing versions must fall back to the flat file: %s %s", p, v)
	}
	if p, _, _ := c.Resolve(base, "HCX330L", "matrix", office); !strings.HasSuffix(p, "HCX330L/matrix") {
		t.Fatalf("unlisted model: %s", p)
	}
}
