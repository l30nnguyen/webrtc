package center

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"signaling-server/internal/firmware"
	"signaling-server/internal/sid"
)

func firmwareEnv(t *testing.T) (*env, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "anycloud"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "anycloud", "matrix.pkg"), []byte("MXPKG1-package-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	return newEnv(t, func(c *Config) { c.FirmwareDir = dir }), dir
}

func firmwareURL(model, sidText string) string {
	return "/v1/firmware/" + model + "/matrix?sid=" + url.QueryEscape(sidText)
}

func TestFirmwareServesPackageToValidCamera(t *testing.T) {
	e, _ := firmwareEnv(t)
	camera := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))
	response := e.do(http.MethodGet, firmwareURL("anycloud", camera), nil, nil)
	if response.Code != http.StatusOK || response.Body.String() != "MXPKG1-package-bytes" {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Length"); got != strconv.Itoa(len("MXPKG1-package-bytes")) {
		t.Fatalf("Content-Length = %q", got)
	}
	logs := e.logs.joined()
	if !strings.Contains(logs, "firmware anycloud") || strings.Contains(logs, camera) {
		t.Fatalf("log must name the model and never the SID itself: %s", logs)
	}
}

func TestFirmwareRefusals(t *testing.T) {
	e, _ := firmwareEnv(t)
	camera := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))
	viewer := e.issue("acme01", sid.FlagViewer, e.now.Add(-time.Hour))
	unknown := e.issue("nobody01", sid.FlagPublisher, e.now.Add(-time.Hour))
	blocked := e.issue("other01", sid.FlagPublisher, e.now.Add(-time.Hour))
	e.block(e.digest(blocked))
	cases := []struct {
		name, method, path string
		want               int
	}{
		{"no sid", http.MethodGet, "/v1/firmware/anycloud/matrix", http.StatusUnauthorized},
		{"bad sid", http.MethodGet, firmwareURL("anycloud", "not-a-sid"), http.StatusUnauthorized},
		{"tampered sid", http.MethodGet, firmwareURL("anycloud", tamper(camera)), http.StatusUnauthorized},
		{"viewer sid", http.MethodGet, firmwareURL("anycloud", viewer), http.StatusUnauthorized},
		{"unknown tenant", http.MethodGet, firmwareURL("anycloud", unknown), http.StatusUnauthorized},
		{"blocked sid", http.MethodGet, firmwareURL("anycloud", blocked), http.StatusUnauthorized},
		{"other platform", http.MethodGet, firmwareURL("anykav500", camera), http.StatusNotFound},
		{"bad platform", http.MethodGet, firmwareURL("any-cloud", camera), http.StatusNotFound},
		{"long platform", http.MethodGet, firmwareURL("anycloud_anycloud_x", camera), http.StatusNotFound},
		{"traversal", http.MethodGet, "/v1/firmware/../matrix?sid=" + url.QueryEscape(camera), http.StatusNotFound},
		{"other file", http.MethodGet, "/v1/firmware/anycloud/matrix.pkg?sid=" + url.QueryEscape(camera), http.StatusNotFound},
		{"post", http.MethodPost, firmwareURL("anycloud", camera), http.StatusMethodNotAllowed},
	}
	for _, c := range cases {
		response := e.do(c.method, c.path, nil, nil)
		if response.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, response.Code, c.want)
		}
		if response.Code == http.StatusOK || strings.Contains(response.Body.String(), "MXPKG1") {
			t.Errorf("%s: package served", c.name)
		}
	}
}

func TestFirmwareOffWithoutDir(t *testing.T) {
	e := newEnv(t)
	camera := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))
	if response := e.do(http.MethodGet, firmwareURL("anycloud", camera), nil, nil); response.Code != http.StatusNotFound {
		t.Fatalf("status = %d", response.Code)
	}
}

// tamper changes one character in the middle of a SID (always a different one).
func tamper(s string) string {
	i := len(s) / 2
	c := byte('A')
	if s[i] == 'A' {
		c = 'B'
	}
	return s[:i] + string(c) + s[i+1:]
}

// The signed package follows cfg/firmware-channels.json too; here the
// canary group is by tenant (the route knows the SID).
func TestFirmwareFollowsChannels(t *testing.T) {
	dir := t.TempDir()
	for path, body := range map[string]string{
		"anycloud/matrix.pkg":       "flat",
		"anycloud/2.2.2/matrix.pkg": "stable-pkg",
		"anycloud/2.2.3/matrix.pkg": "canary-pkg",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	channels := filepath.Join(t.TempDir(), "firmware-channels.json")
	if err := os.WriteFile(channels, []byte(`{"firmware": {"anycloud": {"stable": "2.2.2", "canary": "2.2.3"}}, "canary": {"tenants": ["acme01"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(c *Config) { c.FirmwareDir = dir; c.Channels = firmware.New(channels, nil) })
	canary := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))
	stable := e.issue("other01", sid.FlagPublisher, e.now.Add(-time.Hour))
	if response := e.do(http.MethodGet, firmwareURL("anycloud", canary), nil, nil); response.Body.String() != "canary-pkg" {
		t.Fatalf("canary tenant got %q", response.Body.String())
	}
	if response := e.do(http.MethodGet, firmwareURL("anycloud", stable), nil, nil); response.Body.String() != "stable-pkg" {
		t.Fatalf("other tenant got %q (status %d)", response.Body.String(), response.Code)
	}
	if !strings.Contains(e.logs.joined(), "firmware anycloud 2.2.3 (canary") {
		t.Fatalf("log must name version and channel: %s", e.logs.joined())
	}
}
