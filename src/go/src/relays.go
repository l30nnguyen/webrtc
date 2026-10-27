package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math/rand"
	"os"
	"sync"
	"time"

	"signaling-server/internal/fleet"
	"signaling-server/internal/relay"
)

// relaySecretsFile holds this node's credentials for the relays in the fleet
// configuration, owner-only, read on every viewer request (no restart):
//
//	{"turn-eu00": "<shared secret>",                       // coturn use-auth-secret: minted per session
//	 "turn-vn00": {"username": "matrix", "password": "…"}} // fixed credentials (violet)
const relaySecretsFile = "cfg/relay-secrets.json"

var relayWarnings sync.Map // message -> struct{}: each logged once

func warnOnce(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	if _, seen := relayWarnings.LoadOrStore(message, struct{}{}); !seen {
		log.Print(message)
	}
}

// fleetConfig is the configuration this server runs: Center's own, or the
// one Center pushed to this node.
func (s *Server) fleetConfig() (fleet.Config, bool) {
	if s.fleet != nil {
		return s.fleet.Config(), true
	}
	if s.nodeLink != nil {
		config, version := s.nodeLink.Config()
		return config, version > 0
	}
	return fleet.Config{}, false
}

// relaysFor returns the relay stream servers handed to a camera for one
// viewing session, with credentials minted for it: the relays of the fleet
// configuration when it lists any this node has secrets for, else
// cfg/turn.conf as before.
func (s *Server) relaysFor(cameraID string) []map[string]interface{} {
	config, ok := s.fleetConfig()
	if !ok || len(config.Relays) == 0 {
		return s.loadTurnConfig()
	}
	secrets, err := s.loadRelaySecrets()
	if err != nil {
		warnOnce("relays: %s: %v; using cfg/turn.conf", relaySecretsFile, err)
		return s.loadTurnConfig()
	}
	var usable []fleet.Relay
	for _, r := range config.Relays {
		if _, ok := secrets[r.Name]; ok {
			usable = append(usable, r)
		} else {
			warnOnce("relays: no secret for relay %s in %s; not handing it out", r.Name, relaySecretsFile)
		}
	}
	if len(usable) == 0 {
		return s.loadTurnConfig()
	}
	region := ""
	for _, n := range config.Nodes {
		if n.Address == s.selfAddress {
			region = n.Region
		}
	}
	expiry := time.Now().Add(relay.CredentialLifetime)
	var servers []map[string]interface{}
	for _, r := range relay.Choose(usable, region, s.relayLoad, rand.Intn) {
		secret := secrets[r.Name]
		user, password := secret.Username, secret.Password
		if secret.Shared != "" {
			user, password = relay.Credentials([]byte(secret.Shared), cameraID, expiry)
		}
		servers = append(servers, map[string]interface{}{"host": r.Host, "port": r.Port, "username": user, "password": password})
	}
	return servers
}

// relaySecret is a relay's credentials on this node: a shared secret to mint
// expiring credentials from (coturn), or a fixed user name and password
// (violet, which has no shared-secret mode).
type relaySecret struct {
	Shared             string
	Username, Password string
}

func (s *Server) loadRelaySecrets() (map[string]relaySecret, error) {
	path := s.rootDir + relaySecretsFile
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("missing")
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("must be a regular file readable only by its owner")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	secrets := make(map[string]relaySecret, len(raw))
	for name, value := range raw {
		var shared string
		var fixed struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		switch {
		case json.Unmarshal(value, &shared) == nil:
			if len(shared) < 16 {
				return nil, fmt.Errorf("the secret of %s is shorter than 16 characters", name)
			}
			secrets[name] = relaySecret{Shared: shared}
		case json.Unmarshal(value, &fixed) == nil && fixed.Username != "" && fixed.Password != "":
			secrets[name] = relaySecret{Username: fixed.Username, Password: fixed.Password}
		default:
			return nil, fmt.Errorf("%s must be a shared secret or {\"username\", \"password\"}", name)
		}
	}
	return secrets, nil
}

// relayLoad is a relay's measured load (relays of this node's region).
func (s *Server) relayLoad(name string) (float64, bool, bool) {
	if s.relayMonitor == nil {
		return 0, false, false
	}
	return s.relayMonitor.Load(name)
}

// relayStatuses are this node's recent relay measurements.
func (s *Server) relayStatuses() []fleet.RelayStatus {
	if s.relayMonitor == nil {
		return nil
	}
	return s.relayMonitor.Statuses()
}

// measureRelays measures the relays of this node's region every
// relay.MeasureInterval, following configuration changes.
func (s *Server) measureRelays() {
	s.relayMonitor = &relay.Monitor{}
	for {
		if config, ok := s.fleetConfig(); ok {
			region := ""
			for _, n := range config.Nodes {
				if n.Address == s.selfAddress {
					region = n.Region
				}
			}
			var mine []fleet.Relay
			for _, r := range config.Relays {
				if region != "" && r.Region == region {
					mine = append(mine, r)
				}
			}
			if len(mine) > 0 {
				ctx, cancel := context.WithTimeout(context.Background(), relay.MeasureInterval)
				s.relayMonitor.Measure(ctx, mine)
				cancel()
			}
		}
		time.Sleep(relay.MeasureInterval)
	}
}
