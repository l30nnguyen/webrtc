package admission

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TenantPrefix starts a blocklist line that suspends a whole tenant:
//
//	tenant:acme01
//
// A line may also revoke one device until a given time:
//
//	device:<device id> <expires unix seconds>
//
// Center writes it when a SID is released, so the old device is cut off while
// its tickets and connections run out; the entry is ignored once it expires
// and the permanent refusal lives in the activation store.
// Every other nonempty, non-comment line is a 64-character hex SHA-256 SID
// digest. A tenant entry cuts off every SID of that tenant at once.
const (
	TenantPrefix = "tenant:"
	DevicePrefix = "device:"
)

// Blocklist is a local file of blocked SID digests and tenants. Lookups are
// served from an in-memory index that is rebuilt only when the file's size or
// modification time changes, so updates apply without a restart and a lookup
// costs a stat plus a map access instead of a scan of the whole file. A
// present empty file blocks nothing; a missing or malformed file fails closed.
type Blocklist struct{ Path string }

type blocklistIndex struct {
	mu      sync.Mutex
	loaded  bool
	size    int64
	modTime time.Time
	digests map[string]struct{}
	tenants map[string]struct{}
	devices map[string]int64 // device id -> revoked until (unix seconds)
}

var blocklistIndexes sync.Map // path -> *blocklistIndex

func (b Blocklist) index() (*blocklistIndex, error) {
	value, _ := blocklistIndexes.LoadOrStore(b.Path, &blocklistIndex{})
	idx := value.(*blocklistIndex)
	idx.mu.Lock()
	defer idx.mu.Unlock()
	info, err := os.Stat(b.Path)
	if err != nil {
		idx.loaded = false
		return nil, fmt.Errorf("open blocklist: %w", err)
	}
	if idx.loaded && info.Size() == idx.size && info.ModTime().Equal(idx.modTime) {
		return idx, nil
	}
	digests, tenants, devices, err := parseBlocklist(b.Path)
	if err != nil {
		idx.loaded = false
		return nil, err
	}
	idx.digests, idx.tenants, idx.devices = digests, tenants, devices
	idx.size, idx.modTime, idx.loaded = info.Size(), info.ModTime(), true
	return idx, nil
}

func parseBlocklist(path string) (map[string]struct{}, map[string]struct{}, map[string]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open blocklist: %w", err)
	}
	defer f.Close()
	digests := make(map[string]struct{})
	tenants := make(map[string]struct{})
	devices := make(map[string]int64)
	s := bufio.NewScanner(f)
	for line := 1; s.Scan(); line++ {
		entry := strings.TrimSpace(s.Text())
		if entry == "" || strings.HasPrefix(entry, "#") {
			continue
		}
		if strings.HasPrefix(entry, TenantPrefix) {
			tenant := strings.TrimPrefix(entry, TenantPrefix)
			if !validTenant(tenant) {
				return nil, nil, nil, fmt.Errorf("invalid blocklist entry at line %d", line)
			}
			tenants[tenant] = struct{}{}
			continue
		}
		if strings.HasPrefix(entry, DevicePrefix) {
			fields := strings.Fields(strings.TrimPrefix(entry, DevicePrefix))
			if len(fields) != 2 || !ValidDeviceID(fields[0]) {
				return nil, nil, nil, fmt.Errorf("invalid blocklist entry at line %d", line)
			}
			until, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil || until < 0 {
				return nil, nil, nil, fmt.Errorf("invalid blocklist entry at line %d", line)
			}
			if until > devices[fields[0]] {
				devices[fields[0]] = until
			}
			continue
		}
		if len(entry) != 64 {
			return nil, nil, nil, fmt.Errorf("invalid blocklist entry at line %d", line)
		}
		if _, err := hex.DecodeString(entry); err != nil {
			return nil, nil, nil, fmt.Errorf("invalid blocklist entry at line %d", line)
		}
		digests[strings.ToLower(entry)] = struct{}{}
	}
	if err := s.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("read blocklist: %w", err)
	}
	return digests, tenants, devices, nil
}

// Blocked reports whether the SID digest or its tenant is blocked. tenant may
// be empty to check the digest only.
func (b Blocklist) Blocked(digest, tenant string) (bool, error) {
	if len(digest) != 64 {
		return false, errors.New("invalid digest")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return false, errors.New("invalid digest")
	}
	idx, err := b.index()
	if err != nil {
		return false, err
	}
	// The maps are replaced, never mutated, after parsing, so reading them
	// after the lock is released only races with a replacement of the field.
	idx.mu.Lock()
	digests, tenants := idx.digests, idx.tenants
	idx.mu.Unlock()
	if _, ok := digests[strings.ToLower(digest)]; ok {
		return true, nil
	}
	if tenant != "" {
		if _, ok := tenants[tenant]; ok {
			return true, nil
		}
	}
	return false, nil
}

// Contains reports whether the SID digest alone is blocked.
func (b Blocklist) Contains(digest string) (bool, error) { return b.Blocked(digest, "") }

// DeviceRevoked reports whether deviceID is revoked at now.
func (b Blocklist) DeviceRevoked(deviceID string, now time.Time) (bool, error) {
	idx, err := b.index()
	if err != nil {
		return false, err
	}
	idx.mu.Lock()
	devices := idx.devices
	idx.mu.Unlock()
	until, ok := devices[deviceID]
	return ok && now.Unix() < until, nil
}

// RevokeDevice appends a device revocation that lasts until expires. The line
// is synced before it returns, so a caller can order it before later steps.
func (b Blocklist) RevokeDevice(deviceID string, expires time.Time) error {
	if !ValidDeviceID(deviceID) {
		return errors.New("invalid device id")
	}
	f, err := os.OpenFile(b.Path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return fmt.Errorf("open blocklist: %w", err)
	}
	line := fmt.Sprintf("%s%s %d\n", DevicePrefix, deviceID, expires.Unix())
	// A missing final newline would glue this entry to the previous line.
	if info, statErr := f.Stat(); statErr == nil && info.Size() > 0 {
		last := make([]byte, 1)
		if rf, rerr := os.Open(b.Path); rerr == nil {
			if _, e := rf.ReadAt(last, info.Size()-1); e == nil && last[0] != '\n' {
				line = "\n" + line
			}
			rf.Close()
		}
	}
	if _, err := f.WriteString(line); err != nil {
		f.Close()
		return fmt.Errorf("write blocklist: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync blocklist: %w", err)
	}
	return f.Close()
}

func (b Blocklist) Validate() error {
	_, _, _, err := parseBlocklist(b.Path)
	return err
}
