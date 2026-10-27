package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"signaling-server/internal/fleet"
)

func relayServer(t *testing.T, secrets string) *Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "cfg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cfg", "turn.conf"), []byte(`[{"host":"old.example.test","port":3478,"username":"u","password":"p"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if secrets != "" {
		if err := os.WriteFile(filepath.Join(dir, relaySecretsFile), []byte(secrets), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := fleet.NewFromConfig(fleet.Config{Version: 1,
		Nodes: []fleet.Node{{Address: cliSelf, Region: "DE"}},
		Relays: []fleet.Relay{
			{Name: "turn-de", Region: "DE", Host: "turn-de.example.test"},
			{Name: "turn-us", Region: "US", Host: "turn-us.example.test", Port: 3479},
		}}, cliSelf, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer("", dir+"/")
	srv.fleet, srv.selfAddress = registry, cliSelf
	return srv
}

func TestRelaysComeFromTheConfigWithExpiringCredentials(t *testing.T) {
	srv := relayServer(t, `{"turn-de":"de-secret-0123456789","turn-us":"us-secret-0123456789"}`)
	servers := srv.relaysFor("camera1")
	if len(servers) != 2 || servers[0]["host"] != "turn-de.example.test" || servers[1]["host"] != "turn-us.example.test" ||
		servers[0]["port"] != fleet.DefaultRelayPort || servers[1]["port"] != 3479 {
		t.Fatalf("servers %v", servers)
	}
	user := servers[0]["username"].(string)
	expiry, id, _ := strings.Cut(user, ":")
	at, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil || id != "camera1" || time.Until(time.Unix(at, 0)) < 23*time.Hour {
		t.Fatalf("user %q", user)
	}
	mac := hmac.New(sha1.New, []byte("de-secret-0123456789"))
	mac.Write([]byte(user))
	if servers[0]["password"] != base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
		t.Fatal("password not minted with the relay's secret")
	}
}

func TestRelaysWithoutSecretsFallBackToTurnConf(t *testing.T) {
	for name, secrets := range map[string]string{
		"no secrets file": "",
		"no matching key": `{"other":"0123456789abcdef"}`,
		"short secret":    `{"turn-de":"short"}`,
	} {
		servers := relayServer(t, secrets).relaysFor("camera1")
		if len(servers) != 1 || servers[0]["host"] != "old.example.test" {
			t.Errorf("%s: %v", name, servers)
		}
	}
	// Only the relays with a secret are handed out.
	servers := relayServer(t, `{"turn-us":"us-secret-0123456789"}`).relaysFor("camera1")
	if len(servers) != 1 || servers[0]["host"] != "turn-us.example.test" {
		t.Fatalf("partial secrets: %v", servers)
	}
}

func TestRelaysWithFixedCredentials(t *testing.T) {
	srv := relayServer(t, `{"turn-de":{"username":"matrix","password":"violet-pass"}}`)
	servers := srv.relaysFor("camera1")
	if len(servers) != 1 || servers[0]["username"] != "matrix" || servers[0]["password"] != "violet-pass" {
		t.Fatalf("servers %v", servers)
	}
	if got := relayServer(t, `{"turn-de":{"username":"matrix"}}`).relaysFor("camera1"); got[0]["host"] != "old.example.test" {
		t.Fatalf("incomplete fixed credentials accepted: %v", got)
	}
}
