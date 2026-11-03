package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/turn/v5"
)

// maxCredentialLife bounds how far in the future a minted credential may
// expire. The control server mints them for 24 hours (relay.CredentialLifetime).
const maxCredentialLife = 48 * time.Hour

// secrets is the owner-only file the relay authenticates with:
//
//	{"shared": "<secret, the same as the nodes' relay-secrets.json>",
//	 "users": {"matrix": "<fixed password, as violet's --credentials>"}}
//
// Either part may be left out. SIGHUP rereads it.
type secrets struct {
	path string

	mu     sync.RWMutex
	shared []byte
	users  map[string]string
}

func loadSecrets(path string) (*secrets, error) {
	s := &secrets{path: path}
	return s, s.reload()
}

func (s *secrets) reload() error {
	info, err := os.Lstat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s is missing", s.path)
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s must be a regular file readable only by its owner", s.path)
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var file struct {
		Shared string            `json:"shared"`
		Users  map[string]string `json:"users"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return fmt.Errorf("%s: %w", s.path, err)
	}
	if file.Shared != "" && len(file.Shared) < 16 {
		return fmt.Errorf("%s: the shared secret is shorter than 16 characters", s.path)
	}
	for user, password := range file.Users {
		if user == "" || password == "" || strings.Contains(user, ":") {
			return fmt.Errorf("%s: fixed users need a name without ':' and a password", s.path)
		}
	}
	if file.Shared == "" && len(file.Users) == 0 {
		return fmt.Errorf("%s: neither a shared secret nor users", s.path)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shared, s.users = []byte(file.Shared), file.Users
	return nil
}

// fixedPrefix marks a fixed user's userID; camera ids never contain '/'.
const fixedPrefix = "fixed/"

// password returns the password for a user name: a fixed user's own, else
// the one minted from the shared secret while the name's expiry is ahead.
// userID is what quotas count: the minted name's camera id, or the fixed
// name behind fixedPrefix.
func (s *secrets) password(username string, now time.Time) (userID, password string, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if fixed, ok := s.users[username]; ok {
		return fixedPrefix + username, fixed, true
	}
	if len(s.shared) == 0 {
		return "", "", false
	}
	expiry, id, found := strings.Cut(username, ":")
	if !found || id == "" {
		return "", "", false
	}
	at, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil || at < now.Unix() || at > now.Add(maxCredentialLife).Unix() {
		return "", "", false
	}
	mac := hmac.New(sha1.New, s.shared)
	mac.Write([]byte(username))
	return id, base64.StdEncoding.EncodeToString(mac.Sum(nil)), true
}

// sharedSecret is the secret minted credentials and reports are keyed with.
func (s *secrets) sharedSecret() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.shared
}

func authHandler(s *secrets, st *stats, now func() time.Time) turn.AuthHandler {
	return func(ra *turn.RequestAttributes) (string, []byte, bool) {
		userID, password, ok := s.password(ra.Username, now())
		if !ok {
			st.authFailures.Add(1)
			return "", nil, false
		}
		return userID, turn.GenerateAuthKey(ra.Username, ra.Realm, password), true
	}
}
