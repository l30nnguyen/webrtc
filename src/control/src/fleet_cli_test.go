package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"signaling-server/internal/fleet"
)

const cliSelf = "wss://center.example.test:443/"

func writeCandidate(t *testing.T, dir, name, routes string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := `{"nodes":[{"name":"us00","address":"` + cliSelf + `","region":"US"},` +
		`{"name":"de00","address":"wss://de.example.test:443/","region":"DE","token_sha256":"` + strings.Repeat("ab", 32) + `"}],` +
		`"routes":` + routes + `}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runFleet(t *testing.T, active string, args ...string) (string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := fleetCommand(append([]string{"-config", active, "-self", cliSelf}, args...), &out, &errOut)
	return out.String() + errOut.String(), code
}

func activeVersion(t *testing.T, path string) fleet.Config {
	t.Helper()
	config, err := fleet.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func TestFleetApplyAndRollback(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "config.json")
	first := writeCandidate(t, dir, "first.json", `{"IT":"DE","*":"US"}`)
	second := writeCandidate(t, dir, "second.json", `{"IT":"US","*":"US"}`)

	if out, code := runFleet(t, active, "check", first); code != 0 || !strings.Contains(out, "+ route IT -> DE") {
		t.Fatalf("check: %d %s", code, out)
	}
	if _, err := os.Stat(active); err == nil {
		t.Fatal("check wrote the active file")
	}
	if out, code := runFleet(t, active, "apply", first); code != 0 {
		t.Fatalf("apply first: %d %s", code, out)
	}
	if out, code := runFleet(t, active, "apply", second); code != 0 || !strings.Contains(out, "~ route IT -> DE  =>  US") {
		t.Fatalf("apply second: %d %s", code, out)
	}
	if c := activeVersion(t, active); c.Version != 2 || c.Routes["IT"] != "US" {
		t.Fatalf("after apply: %+v", c)
	}
	// Applying the same content again changes nothing.
	if out, code := runFleet(t, active, "apply", second); code != 0 || !strings.Contains(out, "No changes") {
		t.Fatalf("re-apply: %d %s", code, out)
	}
	if out, code := runFleet(t, active, "rollback"); code != 0 {
		t.Fatalf("rollback: %d %s", code, out)
	}
	if c := activeVersion(t, active); c.Version != 3 || c.Routes["IT"] != "DE" {
		t.Fatalf("after rollback: %+v", c)
	}
	// The history has every version.
	for _, v := range []string{"v1.json", "v2.json", "v3.json"} {
		if _, err := os.Stat(filepath.Join(dir, "config.d", v)); err != nil {
			t.Fatal(err)
		}
	}
	if out, code := runFleet(t, active, "rollback", "2"); code != 0 || activeVersion(t, active).Routes["IT"] != "US" {
		t.Fatalf("rollback 2: %d %s", code, out)
	}
}

func TestFleetApplyRejectsAnInvalidFile(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "config.json")
	good := writeCandidate(t, dir, "good.json", `{"IT":"DE"}`)
	bad := writeCandidate(t, dir, "bad.json", `{"ITA":"GER"}`)
	if _, code := runFleet(t, active, "apply", good); code != 0 {
		t.Fatal("apply good failed")
	}
	before, _ := os.ReadFile(active)
	out, code := runFleet(t, active, "apply", bad)
	if code != 1 || !strings.Contains(out, "two-letter") {
		t.Fatalf("apply bad: %d %s", code, out)
	}
	after, _ := os.ReadFile(active)
	if !bytes.Equal(before, after) {
		t.Fatal("rejected file changed the active configuration")
	}
}

func TestFleetKeepsALegacyFileInTheHistory(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "config.json")
	legacy := `{"nodes":[{"address":"` + cliSelf + `","region":"US"}]}`
	if err := os.WriteFile(active, []byte(legacy), 0o640); err != nil {
		t.Fatal(err)
	}
	next := writeCandidate(t, dir, "next.json", `{"*":"US"}`)
	if out, code := runFleet(t, active, "apply", next); code != 0 {
		t.Fatalf("apply: %d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.d", "v0.json")); err != nil {
		t.Fatal("the legacy version was not kept:", err)
	}
	info, err := os.Stat(active)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("mode not kept: %v %v", info.Mode(), err)
	}
	if out, code := runFleet(t, active, "rollback"); code != 0 || len(activeVersion(t, active).Nodes) != 1 {
		t.Fatalf("rollback to legacy: %d %s", code, out)
	}
}

func TestFleetUsage(t *testing.T) {
	if _, code := runFleet(t, filepath.Join(t.TempDir(), "c.json"), "frobnicate"); code != 2 {
		t.Fatalf("unknown command exit code %d", code)
	}
}

func TestServerReloadKeepsTheRunningConfigOnError(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "config.json")
	first := writeCandidate(t, dir, "first.json", `{"IT":"DE"}`)
	if _, code := runFleet(t, active, "apply", first); code != 0 {
		t.Fatal("apply failed")
	}
	registry, err := fleet.Load(active, cliSelf, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{fleet: registry}
	second := writeCandidate(t, dir, "second.json", `{"IT":"US"}`)
	if _, code := runFleet(t, active, "apply", second); code != 0 {
		t.Fatal("apply failed")
	}
	if err := srv.reloadFleet(active); err != nil || registry.Version() != 2 {
		t.Fatalf("reload: %v, version %d", err, registry.Version())
	}
	if err := os.WriteFile(active, []byte(`{"nodes":[],"routes":{"IT":"DE"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := srv.reloadFleet(active); err == nil || registry.Version() != 2 {
		t.Fatalf("broken file applied: %v, version %d", err, registry.Version())
	}
}

func TestFleetThroughTheAdminSocket(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "config.json")
	first := writeCandidate(t, dir, "first.json", `{"IT":"DE"}`)
	if _, code := runFleet(t, active, "apply", first); code != 0 {
		t.Fatal("apply failed")
	}
	registry, err := fleet.Load(active, cliSelf, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer("", dir+"/")
	srv.fleet = registry
	socket := filepath.Join(dir, "admin.sock")
	listener, err := listenAdminSocket(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go srv.serveAdmin(listener, active)
	if info, err := os.Stat(socket); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v %v", info.Mode(), err)
	}
	withSocket := func(args ...string) (string, int) {
		return runFleet(t, active, append([]string{"-socket", socket}, args...)...)
	}

	out, code := withSocket("status")
	if code != 0 || !strings.Contains(out, "Configuration version 1") || !strings.Contains(out, "de00") {
		t.Fatalf("status: %d %s", code, out)
	}
	second := writeCandidate(t, dir, "second.json", `{"IT":"US"}`)
	if out, code := withSocket("check", second); code != 0 || !strings.Contains(out, "Of 0 camera(s) online now, 0 would go") {
		t.Fatalf("check: %d %s", code, out)
	}
	if out, code := withSocket("apply", second); code != 0 || !strings.Contains(out, "The server now runs version 2") {
		t.Fatalf("apply: %d %s", code, out)
	}
	if registry.Version() != 2 {
		t.Fatalf("server version %d", registry.Version())
	}
	// Not a Center: no statistics here.
	if out, code := withSocket("stats"); code != 1 || !strings.Contains(out, "not one") {
		t.Fatalf("stats: %d %s", code, out)
	}
	if out, code := withSocket("-json", "status"); code != 0 || !strings.Contains(out, `"config_version":2`) {
		t.Fatalf("status -json: %d %s", code, out)
	}
}

func TestFleetBlocklistCheck(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.txt")
	bad := filepath.Join(dir, "bad.txt")
	os.WriteFile(good, []byte(strings.Repeat("a", 64)+"\ntenant:acme01\n"), 0o600)
	os.WriteFile(bad, []byte("not a digest\n"), 0o600)
	if out, code := runFleet(t, filepath.Join(dir, "c.json"), "blocklist-check", good); code != 0 {
		t.Fatalf("good blocklist: %d %s", code, out)
	}
	if out, code := runFleet(t, filepath.Join(dir, "c.json"), "blocklist-check", bad); code != 1 {
		t.Fatalf("bad blocklist: %d %s", code, out)
	}
}
