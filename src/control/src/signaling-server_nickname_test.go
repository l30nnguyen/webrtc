package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"signaling-server/internal/admission"
	"signaling-server/internal/center"
	"signaling-server/internal/sid"
)

func TestValidNickname(t *testing.T) {
	tests := []struct {
		name     string
		nickname string
		want     bool
	}{
		{name: "empty", nickname: "", want: false},
		{name: "ascii", nickname: "user -u value", want: true},
		{name: "multibyte utf8", nickname: "相机用户", want: true},
		{name: "128 bytes", nickname: strings.Repeat("界", 42) + "ab", want: true},
		{name: "over 128 bytes", nickname: strings.Repeat("界", 43), want: false},
		{name: "control character", nickname: "camera\nname", want: false},
		{name: "invalid utf8", nickname: string([]byte{0xff}), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validNickname(tt.nickname); got != tt.want {
				t.Fatalf("validNickname(%q) = %v, want %v", tt.nickname, got, tt.want)
			}
		})
	}
}

func TestRegisterNicknameIsCameraOnlyAndDoesNotChangeIdentityOrScope(t *testing.T) {
	n := newNode(t, func(s *Server) { s.SetDemoTenant("acme01") })
	camera := n.connect("camera0", "acme01", admission.RoleCamera)
	viewerID := "PlayerNicknameTest"
	viewer, response := n.dial("/" + viewerID + "?ticket=" + n.ticket(viewerID, "acme01", admission.RolePlayer, func(c *admission.Claims) {
		c.CameraID = "camera0"
	}))
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("viewer connect status = %d", response.StatusCode)
	}
	defer viewer.Close()
	n.waitRegistered("acme01", viewerID)

	invalid := []struct {
		name  string
		value any
	}{
		{name: "empty", value: ""},
		{name: "too long", value: strings.Repeat("x", 129)},
		{name: "control", value: "bad\u0000name"},
		{name: "wrong type", value: 7},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			send(t, camera, map[string]any{"type": "register", "nickname": tt.value})
			if reply := receive(camera, time.Second); reply == nil || reply["msg"] != "Invalid request" {
				t.Fatalf("invalid nickname reply = %v", reply)
			}
		})
	}

	nickname := "<b>相机用户</b>"
	send(t, camera, map[string]any{
		"type": "register", "nickname": nickname,
		"id": "forged-camera", "tenant": "other-tenant", "role": admission.RolePlayer,
	})
	send(t, viewer, map[string]any{
		"type": "register", "nickname": "player nickname", "sid": strings.Repeat("A", sid.EncodedSize),
		"id": "forged-player", "tenant": "other-tenant", "role": admission.RoleCamera,
	})
	if reply := receive(viewer, time.Second); reply == nil || reply["msg"] != "Invalid request" {
		t.Fatalf("player register reply = %v", reply)
	}
	for _, candidate := range []string{
		"short",
		strings.Repeat("!", sid.EncodedSize),
		strings.Repeat("A", sid.EncodedSize),
	} {
		send(t, camera, map[string]any{"type": "register", "nickname": "must not replace", "sid": candidate})
		if reply := receive(camera, time.Second); reply == nil || reply["msg"] != "Invalid request" {
			t.Fatalf("invalid or mismatched camera SID reply = %v", reply)
		}
	}
	send(t, camera, map[string]any{"type": "register", "nickname": "wrong type SID", "sid": 118})
	if reply := receive(camera, time.Second); reply == nil || reply["msg"] != "Invalid request" {
		t.Fatalf("wrong-type camera SID reply = %v", reply)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n.srv.mu.RLock()
		registered := n.srv.clients[clientKey("acme01", "camera0")]
		updated := registered != nil && registered.Info.Nickname == nickname
		n.srv.mu.RUnlock()
		if updated {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	n.srv.mu.RLock()
	gotCamera := n.srv.clients[clientKey("acme01", "camera0")]
	gotViewer := n.srv.clients[clientKey("acme01", viewerID)]
	cameraInfo := *gotCamera.Info
	viewerRole, viewerScope := gotViewer.Role, gotViewer.CameraIDScope
	viewerNickname := gotViewer.Info.Nickname
	n.srv.mu.RUnlock()
	if cameraInfo.ID != "camera0" || cameraInfo.Tenant != "acme01" || gotCamera.Role != admission.RoleCamera || cameraInfo.Nickname != nickname {
		t.Fatalf("camera identity/metadata after register = %+v, role=%q", cameraInfo, gotCamera.Role)
	}
	if gotCamera.registeredSID != "" {
		t.Fatal("rejected SID registration changed the stored SID")
	}
	if gotViewer.ID != viewerID || gotViewer.Tenant != "acme01" || viewerRole != admission.RolePlayer || viewerScope != "camera0" || viewerNickname != "" {
		t.Fatalf("viewer identity/scope after rejected register = %+v role=%q scope=%q", gotViewer, viewerRole, viewerScope)
	}

	request, err := n.http.Client().Get(n.http.URL + "/api/devices?type=producer")
	if err != nil {
		t.Fatal(err)
	}
	defer request.Body.Close()
	var responseBody struct {
		Devices []DeviceInfo `json:"devices"`
	}
	if err := json.NewDecoder(request.Body).Decode(&responseBody); err != nil {
		t.Fatal(err)
	}
	if len(responseBody.Devices) != 1 || responseBody.Devices[0].ID != "camera0" || responseBody.Devices[0].Nickname != nickname {
		t.Fatalf("device API did not expose display nickname and original ID: %+v", responseBody.Devices)
	}

	// A rejected player registration must leave the authenticated camera scope usable.
	send(t, viewer, map[string]any{"type": "request", "id": "camera0", "password": "test-password"})
	if forwarded := receive(camera, time.Second); forwarded == nil || forwarded["id"] != viewerID {
		t.Fatalf("scoped player request after rejected register = %v", forwarded)
	}
}

func TestDiscoveryPublishesOnlyAuthenticatedMatchingCameraSID(t *testing.T) {
	n := newNode(t, func(s *Server) { s.SetDiscoveryTenant("test01") })
	sidText := base64.RawURLEncoding.EncodeToString(make([]byte, sid.RawSize))
	digest := center.Digest(sidText)
	ticket := n.ticket("sid-camera", "test01", admission.RoleCamera, func(c *admission.Claims) { c.Digest = digest })
	camera, response := n.dial("/sid-camera?ticket=" + ticket)
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("camera connect status = %d", response.StatusCode)
	}
	defer camera.Close()
	n.t.Cleanup(func() { camera.Close() })
	n.waitRegistered("test01", "sid-camera")

	// The matching SID is accepted; a later mismatched replacement is rejected
	// atomically and cannot replace either the nickname or the stored SID.
	send(t, camera, map[string]any{"type": "register", "nickname": "verified name", "sid": sidText})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n.srv.mu.RLock()
		client := n.srv.clients[clientKey("test01", "sid-camera")]
		registered := client != nil && client.registeredSID == sidText
		n.srv.mu.RUnlock()
		if registered {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	n.srv.mu.RLock()
	registeredClient := n.srv.clients[clientKey("test01", "sid-camera")]
	if registeredClient == nil || registeredClient.registeredSID != sidText || registeredClient.Info.Nickname != "verified name" {
		n.srv.mu.RUnlock()
		t.Fatal("camera SID registration did not atomically store matching SID and nickname")
	}
	n.srv.mu.RUnlock()

	badSID := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("z", sid.RawSize)))
	send(t, camera, map[string]any{"type": "register", "nickname": "must not replace", "sid": badSID})
	if reply := receive(camera, time.Second); reply == nil || reply["msg"] != "Invalid request" {
		t.Fatalf("mismatched replacement SID reply = %v", reply)
	}
	n.srv.mu.RLock()
	registeredClient = n.srv.clients[clientKey("test01", "sid-camera")]
	if registeredClient == nil || registeredClient.registeredSID != sidText || registeredClient.Info.Nickname != "verified name" {
		n.srv.mu.RUnlock()
		t.Fatal("rejected SID changed the existing SID or nickname")
	}
	n.srv.mu.RUnlock()

	second := n.connect("second-camera", "test01", admission.RoleCamera)
	send(t, second, map[string]any{"type": "register", "nickname": "legacy registration"})
	otherSID := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", sid.RawSize)))
	otherDigest := center.Digest(otherSID)
	otherTicket := n.ticket("other-camera", "other01", admission.RoleCamera, func(c *admission.Claims) { c.Digest = otherDigest })
	other, response := n.dial("/other-camera?ticket=" + otherTicket)
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("cross-tenant camera connect status = %d", response.StatusCode)
	}
	defer other.Close()
	n.waitRegistered("other01", "other-camera")
	send(t, other, map[string]any{"type": "register", "nickname": "hidden", "sid": otherSID})

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n.srv.mu.RLock()
		client := n.srv.clients[clientKey("other01", "other-camera")]
		registered := client != nil && client.registeredSID == otherSID
		n.srv.mu.RUnlock()
		if registered {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	n.srv.mu.RLock()
	otherRegistered := n.srv.clients[clientKey("other01", "other-camera")]
	if otherRegistered == nil || otherRegistered.registeredSID != otherSID {
		n.srv.mu.RUnlock()
		t.Fatal("cross-tenant camera did not register its ticket-matching SID")
	}
	n.srv.mu.RUnlock()

	response, err := n.http.Client().Get(n.http.URL + "/api/devices")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body struct {
		Devices []map[string]any `json:"devices"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || len(body.Devices) != 2 {
		t.Fatalf("discovery response status/devices = %d/%v", response.StatusCode, body.Devices)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("SID discovery Cache-Control = %q, want no-store", response.Header.Get("Cache-Control"))
	}
	for _, device := range body.Devices {
		switch device["id"] {
		case "sid-camera":
			if device["sid"] != sidText {
				t.Fatalf("authenticated matching SID missing from discovery: %v", device)
			}
		case "second-camera":
			if _, present := device["sid"]; present {
				t.Fatalf("camera without registered SID exposed one: %v", device)
			}
		default:
			t.Fatalf("discovery exposed another tenant: %v", device)
		}
	}
}

func TestLegacyDemoDiscoveryNeverExposesRegisteredSID(t *testing.T) {
	n := newNode(t, func(s *Server) { s.SetDemoTenant("demo0001") })
	sidText := base64.RawURLEncoding.EncodeToString(make([]byte, sid.RawSize))
	ticket := n.ticket("demo-camera", "demo0001", admission.RoleCamera, func(c *admission.Claims) { c.Digest = center.Digest(sidText) })
	camera, response := n.dial("/demo-camera?ticket=" + ticket)
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("camera connect status = %d", response.StatusCode)
	}
	defer camera.Close()
	n.waitRegistered("demo0001", "demo-camera")
	send(t, camera, map[string]any{"type": "register", "nickname": "demo", "sid": sidText})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n.srv.mu.RLock()
		client := n.srv.clients[clientKey("demo0001", "demo-camera")]
		registered := client != nil && client.registeredSID == sidText
		n.srv.mu.RUnlock()
		if registered {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	n.srv.mu.RLock()
	registered := n.srv.clients[clientKey("demo0001", "demo-camera")]
	if registered == nil || registered.registeredSID != sidText {
		n.srv.mu.RUnlock()
		t.Fatal("legacy camera did not store its ticket-matching SID")
	}
	n.srv.mu.RUnlock()
	response, err := n.http.Client().Get(n.http.URL + "/api/devices")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body struct {
		Devices []map[string]any `json:"devices"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Devices) != 1 {
		t.Fatalf("legacy demo discovery returned %d devices", len(body.Devices))
	}
	if _, present := body.Devices[0]["sid"]; present {
		t.Fatal("legacy demo discovery exposed camera SID")
	}
}
