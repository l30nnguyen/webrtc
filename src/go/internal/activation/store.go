package activation

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// A SID has exactly one seat at a time. The first key to bind owns it until the
// pairing system releases it; after a release another device may bind it, but a
// device that held it before never can again (so a lost camera cannot race back
// in), and not before ReleaseGrace has passed, which orders the release and its
// revocation before any new bind.
const (
	StatusReleased = "released" // the zero Status means bound
	ReleaseGrace   = 3 * time.Second
	maxFormer      = 1000
)

// Record is the state of one SID (by digest): bound to the single device
// allowed to use it, or released.
type Record struct {
	Digest      string `json:"digest"` // lowercase hex SHA-256 of the SID
	Tenant      string `json:"tenant"`
	Role        string `json:"role"`       // "camera" or "player"
	DeviceID    string `json:"device_id"`  // DeviceID(DevicePub)
	DevicePub   string `json:"device_pub"` // Base64URL raw Ed25519 public key
	CameraRef   string `json:"camera_ref"` // lowercase hex, see sid.CameraRef
	IssuedAt    int64  `json:"issued_at"`
	ActivatedAt int64  `json:"activated_at"`
	// Status is "" while bound and StatusReleased after a release, when
	// DeviceID and DevicePub are cleared. Former lists every device ID that held
	// the SID before the current holder.
	Status     string   `json:"status,omitempty"`
	ReleasedAt int64    `json:"released_at,omitempty"`
	Former     []string `json:"former,omitempty"`
}

// Bound reports whether a device currently holds the SID.
func (r Record) Bound() bool { return r.Status != StatusReleased }

// State is "bound" or "released".
func (r Record) State() string {
	if r.Bound() {
		return "bound"
	}
	return StatusReleased
}

// Held reports whether deviceID held the SID now or in the past.
func (r Record) Held(deviceID string) bool {
	if r.Bound() && r.DeviceID == deviceID {
		return true
	}
	for _, former := range r.Former {
		if former == deviceID {
			return true
		}
	}
	return false
}

// Key returns the raw device public key.
func (r Record) Key() (ed25519.PublicKey, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(r.DevicePub)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("invalid device public key")
	}
	return ed25519.PublicKey(raw), nil
}

// Store is an append-only JSONL file with an in-memory index. It is the
// single-node implementation; the methods are the contract a database-backed
// store would implement. Memory is roughly 300 bytes per activation.
type Store struct {
	mu       sync.Mutex
	file     *os.File
	size     int64
	byDigest map[string]Record
	byRef    map[string]string // tenant + "/" + camera ref -> digest
}

// Open loads (creating if needed) the store at path. The file holds device
// public keys and SID digests, never SIDs, but is still kept private (0600).
func Open(path string) (*Store, error) {
	created := false
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("activation store must be a regular, non-symlink file")
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, errors.New("activation store permissions must not allow group or other access")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		created = true
	} else {
		return nil, fmt.Errorf("inspect activation store: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open activation store: %w", err)
	}
	if created {
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			file.Close()
			return nil, fmt.Errorf("open activation store directory: %w", err)
		}
		err = dir.Sync()
		closeErr := dir.Close()
		if err != nil {
			file.Close()
			return nil, fmt.Errorf("sync activation store directory: %w", err)
		}
		if closeErr != nil {
			file.Close()
			return nil, fmt.Errorf("close activation store directory: %w", closeErr)
		}
	}
	s := &Store{file: file, byDigest: make(map[string]Record), byRef: make(map[string]string)}
	records, validLength, err := readRecords(file)
	if err != nil {
		file.Close()
		return nil, err
	}
	// A crash can leave a partial final line; drop it so appends stay aligned.
	if info, statErr := file.Stat(); statErr == nil && info.Size() != validLength {
		if err := file.Truncate(validLength); err != nil {
			file.Close()
			return nil, fmt.Errorf("repair activation store: %w", err)
		}
	}
	s.size = validLength
	for _, record := range records {
		s.index(record)
	}
	return s, nil
}

// ReadAll reads every record of a store file without opening it for writing.
func ReadAll(path string) ([]Record, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open activation store: %w", err)
	}
	defer file.Close()
	records, _, err := readRecords(file)
	return records, err
}

func readRecords(file *os.File) ([]Record, int64, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}
	reader := bufio.NewReaderSize(file, 1<<20)
	var records []Record
	var valid int64
	for line := 1; ; line++ {
		data, err := reader.ReadBytes('\n')
		if err == io.EOF {
			// no terminating newline: a torn write, ignore it
			return records, valid, nil
		}
		if err != nil {
			return nil, 0, fmt.Errorf("read activation store: %w", err)
		}
		trimmed := bytes.TrimSpace(data)
		if len(trimmed) > 0 {
			var record Record
			if err := json.Unmarshal(trimmed, &record); err != nil || record.Digest == "" || record.Tenant == "" {
				return nil, 0, fmt.Errorf("malformed activation record at line %d", line)
			}
			records = append(records, record)
		}
		valid += int64(len(data))
	}
}

func (s *Store) index(record Record) {
	s.byDigest[record.Digest] = record
	if record.Bound() {
		s.byRef[record.Tenant+"/"+record.CameraRef] = record.Digest
	} else {
		delete(s.byRef, record.Tenant+"/"+record.CameraRef)
	}
}

// Get returns the binding for a SID digest.
func (s *Store) Get(digest string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.byDigest[digest]
	return record, ok
}

// LookupRef returns the binding for a tenant's camera reference. A released
// SID has no camera reference.
func (s *Store) LookupRef(tenant, ref string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	digest, ok := s.byRef[tenant+"/"+ref]
	if !ok {
		return Record{}, false
	}
	return s.byDigest[digest], true
}

// Count returns the number of activations.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byDigest)
}

// BindResult says what Bind did.
type BindResult int

const (
	BindCreated      BindResult = iota // first bind of this SID
	BindRebound                        // bound again after a release
	BindExisting                       // already bound; the caller compares keys
	BindFormerHolder                   // refused: this device held the SID before
	BindTooSoon                        // refused: released less than ReleaseGrace ago
)

// ErrUnknown is returned for a SID digest the store has never bound.
var ErrUnknown = errors.New("unknown SID")

// append makes record durable (fsynced) and visible. Callers hold s.mu.
func (s *Store) append(record Record) error {
	line, err := json.Marshal(record)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if _, err := s.file.Write(line); err != nil {
		_ = s.file.Truncate(s.size) // keep the file aligned on a failed append
		return fmt.Errorf("append activation: %w", err)
	}
	if err := s.file.Sync(); err != nil {
		_ = s.file.Truncate(s.size)
		return fmt.Errorf("sync activation: %w", err)
	}
	s.size += int64(len(line))
	s.index(record)
	return nil
}

// Bind gives candidate's device the seat of its SID when the rules allow it
// and returns the record now in force. The check and the write are one step
// under the store lock, so two devices can never both win.
func (s *Store) Bind(candidate Record, now time.Time) (Record, BindResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.byDigest[candidate.Digest]
	if !ok {
		if err := s.append(candidate); err != nil {
			return Record{}, 0, err
		}
		return candidate, BindCreated, nil
	}
	if existing.Bound() {
		return existing, BindExisting, nil
	}
	if existing.Held(candidate.DeviceID) {
		return existing, BindFormerHolder, nil
	}
	if now.Sub(time.Unix(existing.ReleasedAt, 0)) < ReleaseGrace {
		return existing, BindTooSoon, nil
	}
	candidate.Former = existing.Former
	if err := s.append(candidate); err != nil {
		return Record{}, 0, err
	}
	return candidate, BindRebound, nil
}

// Release ends the current holder's seat. It returns the record as it was
// before (so the caller knows which device to revoke) and whether this call
// changed anything; releasing a released SID is a no-op.
func (s *Store) Release(digest string, now time.Time) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.byDigest[digest]
	if !ok {
		return Record{}, false, ErrUnknown
	}
	if !existing.Bound() {
		return existing, false, nil
	}
	released := existing
	released.Status = StatusReleased
	released.ReleasedAt = now.Unix()
	released.Former = append(append([]string(nil), existing.Former...), existing.DeviceID)
	if len(released.Former) > maxFormer {
		released.Former = released.Former[len(released.Former)-maxFormer:]
	}
	released.DeviceID, released.DevicePub = "", ""
	if err := s.append(released); err != nil {
		return Record{}, false, err
	}
	return existing, true, nil
}

// Close releases the file.
func (s *Store) Close() error { return s.file.Close() }
