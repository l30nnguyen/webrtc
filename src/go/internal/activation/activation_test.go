package activation

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func record(digest, tenant, ref string) Record {
	return Record{Digest: digest, Tenant: tenant, Role: "camera", DeviceID: "dev-" + digest, DevicePub: "pub", CameraRef: ref, IssuedAt: 1, ActivatedAt: 2}
}

func TestStoreOneSeatPerSIDAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activations.jsonl")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first := record("d1", "acme01", "ref1")
	got, result, err := s.Bind(first, time.Now())
	if err != nil || result != BindCreated || got.DevicePub != first.DevicePub {
		t.Fatalf("Bind = %+v %v %v", got, result, err)
	}
	other := first
	other.DevicePub = "someone-else"
	got, result, err = s.Bind(other, time.Now())
	if err != nil || result != BindExisting || got.DevicePub != "pub" {
		t.Fatalf("second bind must return the original binding: %+v result=%v err=%v", got, result, err)
	}
	if _, ok := s.LookupRef("acme01", "ref1"); !ok {
		t.Fatal("ref lookup failed")
	}
	if _, ok := s.LookupRef("other01", "ref1"); ok {
		t.Fatal("ref lookup crossed tenants")
	}
	s.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Count() != 1 {
		t.Fatalf("count after reopen = %d", reopened.Count())
	}
	if got, ok := reopened.Get("d1"); !ok || got.DeviceID != first.DeviceID || got.DevicePub != first.DevicePub {
		t.Fatalf("Get after reopen = %+v %v", got, ok)
	}
	all, err := ReadAll(path)
	if err != nil || len(all) != 1 {
		t.Fatalf("ReadAll = %v %v", all, err)
	}
}

func TestStoreSameRefInTwoTenantsIsIndependent(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "a.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Bind(record("d1", "acme01", "same"), time.Now())
	s.Bind(record("d2", "other01", "same"), time.Now())
	a, _ := s.LookupRef("acme01", "same")
	b, _ := s.LookupRef("other01", "same")
	if a.Digest != "d1" || b.Digest != "d2" {
		t.Fatalf("tenant isolation broken: %+v %+v", a, b)
	}
}

func TestStoreRepairsTornFinalLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	s, _ := Open(path)
	s.Bind(record("d1", "acme01", "ref1"), time.Now())
	s.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"digest":"d2","tenant":"acme`)
	f.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Count() != 1 {
		t.Fatalf("count = %d, want 1", s.Count())
	}
	if _, result, err := s.Bind(record("d3", "acme01", "ref3"), time.Now()); err != nil || result != BindCreated {
		t.Fatalf("append after repair: %v %v", result, err)
	}
	s.Close()
	if all, err := ReadAll(path); err != nil || len(all) != 2 {
		t.Fatalf("after repair ReadAll = %v %v", all, err)
	}
}

func TestStoreRejectsUnsafeOrCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	loose := filepath.Join(dir, "loose.jsonl")
	os.WriteFile(loose, nil, 0o644)
	os.Chmod(loose, 0o644)
	if _, err := Open(loose); err == nil {
		t.Fatal("group/world readable store accepted")
	}
	corrupt := filepath.Join(dir, "corrupt.jsonl")
	os.WriteFile(corrupt, []byte("not json\n"), 0o600)
	if _, err := Open(corrupt); err == nil {
		t.Fatal("corrupt store accepted")
	}
}

func TestProofMessageAndDeviceID(t *testing.T) {
	publicKey, privateKey, _ := ed25519.GenerateKey(rand.Reader)
	nonce := make([]byte, NonceSize)
	for i := range nonce {
		nonce[i] = byte(i)
	}
	msg, err := Message(nonce, "the-sid", "wss://main.example.test/")
	if err != nil {
		t.Fatal(err)
	}
	if string(msg[:len(ProofContext)]) != ProofContext || len(msg) != len(ProofContext)+NonceSize+32+len("wss://main.example.test/") {
		t.Fatalf("unexpected message layout, len=%d", len(msg))
	}
	sig, _ := Sign(privateKey, nonce, "the-sid", "wss://main.example.test/")
	if !ed25519.Verify(publicKey, msg, sig) {
		t.Fatal("signature over Message does not verify")
	}
	if other, _ := Message(nonce, "the-sid", "wss://other.example.test/"); ed25519.Verify(publicKey, other, sig) {
		t.Fatal("proof is not bound to the main origin")
	}
	if _, err := Message(nonce[:5], "x", "y"); err == nil {
		t.Fatal("short nonce accepted")
	}
	sum := sha256.Sum256(publicKey)
	if DeviceID(publicKey) != base64.RawURLEncoding.EncodeToString(sum[:]) || len(DeviceID(publicKey)) != 43 {
		t.Fatalf("DeviceID = %q", DeviceID(publicKey))
	}
}

// TestGoldenProofVector pins the activation proof shared with the camera
// (libdatachannel_c14/examples/streamer/device_key_test.cpp). Ed25519 is
// deterministic, so a native implementation must reproduce these bytes.
func TestGoldenProofVector(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(0xd0 + i)
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	nonce := make([]byte, NonceSize)
	for i := range nonce {
		nonce[i] = byte(i)
	}
	const goldenSID = "AmqxO4AAAWFjbWUwMQAAoKGio6Slpqeo69QW527HNirlkBhlJxrrja7rgEEWZC_odr4OpHHVA2NBERot7UmgkO8DBP8eF6NBbeiI3Tkoudp6-6NpzxXiDw"
	sig, err := Sign(privateKey, nonce, goldenSID, "wss://main.example.test:8000/")
	if err != nil {
		t.Fatal(err)
	}
	got := hex.EncodeToString(publicKey) + "\n" + DeviceID(publicKey) + "\n" + hex.EncodeToString(sig)
	const want = "6f0f0eeb3fbff925c66d03e19dce48d5edc9ffee51cd26ae71dd39e56f7fdda2\ngF0vfOTYfcKmnOVpHQRkUe_y1VPmSLhXDydcnkVEvjc\nac83e3ddbbe6d88900b8f090b3ccd041e214fc0fc5621d283dcc1305ca7648d61953a94288ac8bb1d30b50ac11fd970d0e72e802a84b3c72124b071ff8156004"
	if got != want {
		t.Fatalf("golden proof changed:\n%s", got)
	}
}

func deviceRecord(digest, deviceID string) Record {
	r := record(digest, "acme01", "ref-"+digest)
	r.DeviceID, r.DevicePub = deviceID, "pub-"+deviceID
	return r
}

func TestReleaseThenRebindByAnotherDevice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Unix(1_800_000_000, 0)
	if _, result, _ := s.Bind(deviceRecord("d1", "old"), t0); result != BindCreated {
		t.Fatalf("first bind = %v", result)
	}
	before, changed, err := s.Release("d1", t0)
	if err != nil || !changed || before.DeviceID != "old" {
		t.Fatalf("Release = %+v %v %v", before, changed, err)
	}
	if _, ok := s.LookupRef("acme01", "ref-d1"); ok {
		t.Fatal("a released SID must not resolve to a camera")
	}
	if got, _ := s.Get("d1"); got.Bound() || got.DeviceID != "" || got.DevicePub != "" || got.State() != "released" {
		t.Fatalf("released record = %+v", got)
	}
	if _, _, err := s.Release("d1", t0); err != nil {
		t.Fatalf("second release must be a no-op: %v", err)
	}
	if _, changed, _ := s.Release("d1", t0); changed {
		t.Fatal("second release reported a change")
	}
	if _, _, err := s.Release("missing", t0); err != ErrUnknown {
		t.Fatalf("unknown digest = %v", err)
	}

	// Within the grace the new device is told to retry; the old device is refused for good.
	if _, result, _ := s.Bind(deviceRecord("d1", "new"), t0.Add(ReleaseGrace-time.Second)); result != BindTooSoon {
		t.Fatalf("bind inside grace = %v", result)
	}
	if _, result, _ := s.Bind(deviceRecord("d1", "old"), t0.Add(time.Hour)); result != BindFormerHolder {
		t.Fatalf("former holder = %v", result)
	}
	got, result, err := s.Bind(deviceRecord("d1", "new"), t0.Add(ReleaseGrace))
	if err != nil || result != BindRebound || got.DeviceID != "new" || len(got.Former) != 1 || got.Former[0] != "old" {
		t.Fatalf("rebind = %+v %v %v", got, result, err)
	}
	if _, ok := s.LookupRef("acme01", "ref-d1"); !ok {
		t.Fatal("rebound SID must resolve again")
	}
	if _, result, _ := s.Bind(deviceRecord("d1", "third"), t0.Add(time.Hour)); result != BindExisting {
		t.Fatalf("a bound SID must not be taken over: %v", result)
	}
	s.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, _ = reopened.Get("d1")
	if got.DeviceID != "new" || !got.Held("old") || !got.Held("new") || got.Held("third") {
		t.Fatalf("history lost across reopen: %+v", got)
	}
}

func TestOnlyOneOfManyConcurrentBindsWins(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "a.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t0 := time.Unix(1_800_000_000, 0)
	s.Bind(deviceRecord("d1", "old"), t0)
	s.Release("d1", t0)
	var wins int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, result, _ := s.Bind(deviceRecord("d1", fmt.Sprintf("n%d", i)), t0.Add(time.Minute)); result == BindRebound {
				atomic.AddInt32(&wins, 1)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d binds won, want exactly 1", wins)
	}
}
