package main

import (
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

// relaySecretsFile holds this node's shared secrets of the relays in the
// fleet configuration: {"turn-eu00": "<secret>"}, owner-only. It is read on
// every viewer request, so a change needs no restart.
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
		user, password := relay.Credentials([]byte(secrets[r.Name]), cameraID, expiry)
		servers = append(servers, map[string]interface{}{"host": r.Host, "port": r.Port, "username": user, "password": password})
	}
	return servers
}

func (s *Server) loadRelaySecrets() (map[string]string, error) {
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
	var secrets map[string]string
	if err := json.Unmarshal(data, &secrets); err != nil {
		return nil, err
	}
	for name, secret := range secrets {
		if len(secret) < 16 {
			return nil, fmt.Errorf("the secret of %s is shorter than 16 characters", name)
		}
	}
	return secrets, nil
}

// relayLoad is the measured load of a relay; none is measured yet.
func (s *Server) relayLoad(name string) (float64, bool, bool) { return 0, false, false }
