package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
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
		return s.turnConfForCamera()
	}
	secrets, err := s.loadRelaySecrets()
	if err != nil {
		warnOnce("relays: %s: %v; using cfg/turn.conf", relaySecretsFile, err)
		return s.turnConfForCamera()
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
		return s.turnConfForCamera()
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

// turnConfForCamera is cfg/turn.conf as a camera takes it: the viewer-only
// keys (tcp_port, tls_port, tls_host) stay on the server.
func (s *Server) turnConfForCamera() []map[string]interface{} {
	servers := s.loadTurnConfig()
	for i, server := range servers {
		camera := map[string]interface{}{}
		for _, key := range []string{"host", "port", "username", "password"} {
			if value, ok := server[key]; ok {
				camera[key] = value
			}
		}
		servers[i] = camera
	}
	return servers
}

// relayAddress is where viewers reach one relay: its UDP address, which
// the camera names in its offer, and its TCP and TLS ports (0: none).
type relayAddress struct {
	name     string // in the fleet configuration; "" for cfg/turn.conf
	host     string
	port     int
	tcp, tls int
	tlsHost  string
}

func (s *Server) relayAddresses() []relayAddress {
	var out []relayAddress
	if config, ok := s.fleetConfig(); ok {
		for _, r := range config.Relays {
			out = append(out, relayAddress{name: r.Name, host: r.Host, port: r.Port, tcp: r.TCPPort, tls: r.TLSPort, tlsHost: r.CertificateHost()})
		}
	}
	for _, server := range s.loadTurnConfig() {
		number := func(key string) int {
			n, _ := server[key].(float64)
			return int(n)
		}
		host, _ := server["host"].(string)
		tlsHost, _ := server["tls_host"].(string)
		if tlsHost == "" {
			tlsHost = host
		}
		out = append(out, relayAddress{host: host, port: number("port"), tcp: number("tcp_port"), tls: number("tls_port"), tlsHost: tlsHost})
	}
	return out
}

// addRelayURLs gives each relay a camera's offer names the URLs a viewer
// uses for it, in the order to prefer them: UDP, TCP, TLS. A viewer passes
// them all as one ICE server; ICE then prefers UDP over TCP over TLS by
// itself, so a viewer that can't use UDP falls back without waiting. The
// camera knows only the UDP address: TCP and TLS come from the relay
// configuration, leaving out a port the monitor last found down.
func (s *Server) addRelayURLs(msg map[string]interface{}) {
	var candidates []interface{}
	switch value := msg["candidates"].(type) {
	case []interface{}:
		candidates = value
	case map[string]interface{}:
		candidates = []interface{}{value}
	default:
		return
	}
	var known []relayAddress
	for _, item := range candidates {
		candidate, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		delete(candidate, "urls") // only the server names them
		raw, _ := candidate["url"].(string)
		address, query, _ := strings.Cut(raw, "?")
		if query != "" && query != "transport=udp" {
			continue
		}
		host, portText, err := net.SplitHostPort(address)
		if err != nil {
			continue
		}
		port, err := strconv.Atoi(portText)
		if err != nil {
			continue
		}
		if known == nil {
			known = s.relayAddresses()
		}
		for _, r := range known {
			if r.host != host || r.port != port {
				continue
			}
			urls := []interface{}{"turn:" + net.JoinHostPort(host, portText) + "?transport=udp"}
			if r.tcp != 0 && s.relayTransportUsable(r.name, "tcp") {
				urls = append(urls, "turn:"+net.JoinHostPort(host, strconv.Itoa(r.tcp))+"?transport=tcp")
			}
			if r.tls != 0 && s.relayTransportUsable(r.name, "tls") {
				urls = append(urls, "turns:"+net.JoinHostPort(r.tlsHost, strconv.Itoa(r.tls))+"?transport=tcp")
			}
			candidate["urls"] = urls
			break
		}
	}
}

// relayTransportUsable is false only when this node measured the relay's
// TCP or TLS port down recently; relays it doesn't measure are handed out.
func (s *Server) relayTransportUsable(name, transport string) bool {
	if s.relayMonitor == nil || name == "" {
		return true
	}
	up, known := s.relayMonitor.Transport(name, transport)
	return up || !known
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
