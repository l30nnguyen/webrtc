package admission

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	digestA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func writeBlocklist(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blocklist.txt")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBlocklistDigestsAndTenants(t *testing.T) {
	path := writeBlocklist(t, "# comment\n"+digestA+"\ntenant:badco01\n\n")
	b := Blocklist{Path: path}
	if blocked, err := b.Blocked(digestA, "acme01"); err != nil || !blocked {
		t.Fatalf("listed digest: blocked=%v err=%v", blocked, err)
	}
	if blocked, err := b.Blocked(strings.ToUpper(digestA), ""); err != nil || !blocked {
		t.Fatalf("digest lookup must ignore case: blocked=%v err=%v", blocked, err)
	}
	if blocked, err := b.Blocked(digestB, "badco01"); err != nil || !blocked {
		t.Fatalf("blocked tenant: blocked=%v err=%v", blocked, err)
	}
	if blocked, err := b.Blocked(digestB, "acme01"); err != nil || blocked {
		t.Fatalf("clean SID reported blocked: blocked=%v err=%v", blocked, err)
	}
	if blocked, err := b.Contains(digestB); err != nil || blocked {
		t.Fatalf("Contains(digestB) = %v, %v", blocked, err)
	}
}

func TestBlocklistReloadsWhenFileChanges(t *testing.T) {
	path := writeBlocklist(t, "")
	b := Blocklist{Path: path}
	if blocked, err := b.Contains(digestA); err != nil || blocked {
		t.Fatalf("empty list: %v %v", blocked, err)
	}
	if err := os.WriteFile(path, []byte(digestA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if blocked, err := b.Contains(digestA); err != nil || !blocked {
		t.Fatalf("update not applied: %v %v", blocked, err)
	}
}

func TestBlocklistFailsClosed(t *testing.T) {
	missing := Blocklist{Path: filepath.Join(t.TempDir(), "missing")}
	if _, err := missing.Contains(digestA); err == nil {
		t.Fatal("missing blocklist did not fail closed")
	}
	for _, bad := range []string{"not-a-digest\n", "tenant:BAD\n", strings.Repeat("z", 64) + "\n"} {
		b := Blocklist{Path: writeBlocklist(t, bad)}
		if _, err := b.Contains(digestA); err == nil {
			t.Fatalf("malformed blocklist %q accepted", bad)
		}
		if b.Validate() == nil {
			t.Fatalf("Validate accepted %q", bad)
		}
	}
	b := Blocklist{Path: writeBlocklist(t, "")}
	if _, err := b.Contains("short"); err == nil {
		t.Fatal("invalid digest accepted")
	}
}

func TestBlocklistLookupScalesWithEntries(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 200000; i++ {
		sb.WriteString(strings.Repeat("0", 64-8))
		sb.WriteString(strings.Repeat("1", 8))
		sb.WriteString("\n")
		if i == 0 {
			continue
		}
	}
	sb.WriteString(digestA + "\n")
	b := Blocklist{Path: writeBlocklist(t, sb.String())}
	if _, err := b.Contains(digestB); err != nil { // builds the index
		t.Fatal(err)
	}
	start := time.Now()
	for i := 0; i < 1000; i++ {
		if blocked, err := b.Contains(digestA); err != nil || !blocked {
			t.Fatalf("lookup: %v %v", blocked, err)
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("1000 lookups took %v; the index is not being reused", elapsed)
	}
}

func TestDeviceRevocationExpiresAndSurvivesAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "block.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 64)), 0o600); err != nil { // no trailing newline
		t.Fatal(err)
	}
	b := Blocklist{Path: path}
	id := "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
	now := time.Unix(1_800_000_000, 0)
	if revoked, err := b.DeviceRevoked(id, now); err != nil || revoked {
		t.Fatalf("before = %v %v", revoked, err)
	}
	if err := b.RevokeDevice(id, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if revoked, _ := b.DeviceRevoked(id, now); !revoked {
		t.Fatal("not revoked right after RevokeDevice")
	}
	if revoked, _ := b.DeviceRevoked(id, now.Add(2*time.Minute)); revoked {
		t.Fatal("revocation must end at its expiry")
	}
	if revoked, _ := b.DeviceRevoked("another-device-id", now); revoked {
		t.Fatal("revocation leaked to another device")
	}
	if blocked, _ := b.Blocked(strings.Repeat("a", 64), ""); !blocked {
		t.Fatal("the digest line before the new entry was damaged")
	}
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("device:bad id 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b.Validate() == nil {
		t.Fatal("malformed device entry accepted")
	}
}
