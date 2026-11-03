package center

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"signaling-server/internal/sid"
)

// DefaultActivationWindow is how long a SID may stay unactivated (3 years). An
// unactivated SID is a bearer secret, so it stops being accepted afterwards.
// Activated SIDs never expire; they are revoked through the blocklist.
const DefaultActivationWindow = 3 * 365 * 24 * time.Hour

// Tenant is the Center-side policy for one tenant.
type Tenant struct {
	// ActivationWindow overrides DefaultActivationWindow when positive.
	ActivationWindow time.Duration
}

// TenantSource answers whether a tenant is registered. Unknown tenants are
// rejected, so a SID can not create a tenant by existing.
type TenantSource interface {
	Lookup(name string) (Tenant, bool, error)
}

// StaticTenants is an in-memory TenantSource.
type StaticTenants map[string]Tenant

func (s StaticTenants) Lookup(name string) (Tenant, bool, error) {
	t, ok := s[name]
	return t, ok, nil
}

// TenantFile reads a JSON object mapping tenant names to settings and rereads
// it when the file changes:
//
//	{"acme01": {}, "bigco001": {"activation_window_days": 365}}
type TenantFile struct {
	Path string

	mu      sync.Mutex
	loaded  bool
	size    int64
	modTime time.Time
	tenants map[string]Tenant
}

type tenantEntry struct {
	ActivationWindowDays int `json:"activation_window_days"`
}

func (f *TenantFile) Lookup(name string) (Tenant, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, err := os.Stat(f.Path)
	if err != nil {
		f.loaded = false
		return Tenant{}, false, fmt.Errorf("stat tenants file: %w", err)
	}
	if !f.loaded || info.Size() != f.size || !info.ModTime().Equal(f.modTime) {
		tenants, err := readTenants(f.Path)
		if err != nil {
			f.loaded = false
			return Tenant{}, false, err
		}
		f.tenants, f.size, f.modTime, f.loaded = tenants, info.Size(), info.ModTime(), true
	}
	t, ok := f.tenants[name]
	return t, ok, nil
}

// Validate loads the file once, for startup checks.
func (f *TenantFile) Validate() error {
	_, err := readTenants(f.Path)
	return err
}

func readTenants(path string) (map[string]Tenant, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read tenants file: %w", err)
	}
	var raw map[string]tenantEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse tenants file: %w", err)
	}
	tenants := make(map[string]Tenant, len(raw))
	for name, entry := range raw {
		if !sid.ValidTenant(name) || entry.ActivationWindowDays < 0 {
			return nil, fmt.Errorf("invalid tenant entry %q", name)
		}
		tenants[name] = Tenant{ActivationWindow: time.Duration(entry.ActivationWindowDays) * 24 * time.Hour}
	}
	return tenants, nil
}
