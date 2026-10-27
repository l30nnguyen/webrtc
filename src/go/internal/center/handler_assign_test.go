package center

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"signaling-server/internal/activation"
	"signaling-server/internal/admission"
	"signaling-server/internal/fleet"
	"signaling-server/internal/sid"
	"signaling-server/internal/stats"
)

const assignedNode = "wss://node-b.example.test:443/"

func withAssignedNode(c *Config) { c.AssignAddress = assignedNode }

// challenge3 asks for a proto 3 challenge and returns its nonce and node.
func (e *env) challenge3() (string, string) {
	e.t.Helper()
	response := e.do(http.MethodPost, "/v1/challenge", `{"proto":3}`, nil)
	var result struct {
		Nonce             string `json:"nonce"`
		ExpiresIn         int    `json:"expires_in"`
		MainServerAddress string `json:"main_server_address"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.ExpiresIn != 30 {
		e.t.Fatalf("proto 3 challenge = %d %s", response.Code, response.Body.String())
	}
	return result.Nonce, result.MainServerAddress
}

func (e *env) proof3(d device, nonceText, sidText, node string) map[string]any {
	nonce, _ := enc.DecodeString(nonceText)
	sig, err := activation.SignV2(d.private, nonce, sidText, node)
	if err != nil {
		e.t.Fatal(err)
	}
	return map[string]any{"proto": 3, "sid": sidText, "device_pub": enc.EncodeToString(d.public), "nonce": nonceText, "sig": enc.EncodeToString(sig), "main_server_address": node}
}

func (e *env) ticketAudience(ticket, audience string) error {
	_, err := admission.TicketVerifier{PublicKey: e.centerPublic, KeyID: "center-v1", Audience: audience, Now: func() time.Time { return *e.clock }}.VerifyAny(ticket)
	return err
}

func TestProto3CameraAndViewerGoToTheAssignedNode(t *testing.T) {
	e := newEnv(t, withAssignedNode)
	d := newDevice()
	sidText := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))

	nonce, node := e.challenge3()
	if node != assignedNode {
		t.Fatalf("challenge node = %q, want %q", node, assignedNode)
	}
	response := e.do(http.MethodPost, "/verify", e.proof3(d, nonce, sidText, node), nil)
	var camera verifyResult
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &camera) != nil {
		t.Fatalf("verify = %d %s", response.Code, response.Body.String())
	}
	if camera.MainServerAddress != assignedNode {
		t.Fatalf("camera sent to %q", camera.MainServerAddress)
	}
	if err := e.ticketAudience(camera.Ticket, assignedNode); err != nil {
		t.Fatalf("camera ticket is not for the assigned node: %v", err)
	}
	if e.ticketAudience(camera.Ticket, mainAddress) == nil {
		t.Fatal("camera ticket also valid on this Center's own node")
	}

	view := e.do(http.MethodPost, "/v1/view", map[string]string{"sid": sidText}, nil)
	var viewer viewResult
	if view.Code != http.StatusOK || json.Unmarshal(view.Body.Bytes(), &viewer) != nil {
		t.Fatalf("view = %d %s", view.Code, view.Body.String())
	}
	if viewer.MainServerAddress != assignedNode || e.ticketAudience(viewer.Ticket, assignedNode) != nil {
		t.Fatalf("viewer sent to %q, ticket not for the camera's node", viewer.MainServerAddress)
	}
}

func TestProto2CameraKeepsItsOwnNodeAndViewersFollowIt(t *testing.T) {
	e := newEnv(t, withAssignedNode)
	d := newDevice()
	sidText := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))

	camera := e.mustVerify(d, sidText) // old camera: {} challenge, proto 2 proof
	if camera.MainServerAddress != mainAddress || e.ticketAudience(camera.Ticket, mainAddress) != nil {
		t.Fatalf("old camera sent to %q", camera.MainServerAddress)
	}
	view := e.do(http.MethodPost, "/v1/view", map[string]string{"sid": sidText}, nil)
	var viewer viewResult
	if view.Code != http.StatusOK || json.Unmarshal(view.Body.Bytes(), &viewer) != nil || viewer.MainServerAddress != mainAddress {
		t.Fatalf("viewer of an old camera = %d %s", view.Code, view.Body.String())
	}
}

func TestOldChallengeAnswerIsUnchanged(t *testing.T) {
	e := newEnv(t, withAssignedNode)
	for _, body := range []string{"{}", "", `{"proto":2}`} {
		response := e.do(http.MethodPost, "/v1/challenge", body, nil)
		var fields map[string]any
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &fields) != nil || len(fields) != 2 {
			t.Fatalf("challenge %q answered %s", body, response.Body.String())
		}
	}
}

func TestProto3ProofOnlyForTheNodeCenterChose(t *testing.T) {
	e := newEnv(t, withAssignedNode)
	sidText := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))
	cases := map[string]func(d device) map[string]any{
		"another node": func(d device) map[string]any {
			nonce, _ := e.challenge3()
			return e.proof3(d, nonce, sidText, "wss://evil.example.test:443/")
		},
		"proto 3 nonce as proto 2": func(d device) map[string]any {
			nonce, _ := e.challenge3()
			return e.proof(d, nonce, sidText, mainAddress)
		},
		"proto 2 nonce as proto 3": func(d device) map[string]any {
			return e.proof3(d, e.challenge(), sidText, assignedNode)
		},
		"v1 signature on proto 3": func(d device) map[string]any {
			nonce, node := e.challenge3()
			request := e.proof(d, nonce, sidText, node)
			request["proto"], request["main_server_address"] = 3, node
			return request
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			response := e.do(http.MethodPost, "/verify", build(newDevice()), nil)
			if response.Code == http.StatusOK {
				t.Fatalf("accepted: %s", response.Body.String())
			}
		})
	}
	bad := e.proof(newDevice(), e.challenge(), sidText, mainAddress)
	bad["main_server_address"] = assignedNode
	if response := e.do(http.MethodPost, "/verify", bad, nil); response.Code != http.StatusBadRequest {
		t.Fatalf("proto 2 with an address = %d", response.Code)
	}
}

const remoteNode = "wss://sg1.example.test:443/"

func fleetEnv(t *testing.T, local *fleet.Report, adminToken string) *env {
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	registry, err := fleet.New([]fleet.Node{
		{Address: mainAddress, Capacity: 1},
		{Address: remoteNode, Countries: []string{"VN"}, Capacity: 100, PublicKey: base64.RawURLEncoding.EncodeToString(key)},
	}, mainAddress, func() fleet.Report { return *local }, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(c *Config) {
		c.Fleet = registry
		c.AdminToken = []byte(adminToken)
	})
	e.fleet = registry
	return e
}

// linkNode stands in for a node's link: it logs the node in with its state.
func (e *env) linkNode(report fleet.Report) {
	e.t.Helper()
	if _, err := e.fleet.Linked(report, 0); err != nil {
		e.t.Fatal(err)
	}
}

func TestFleetAssignsByLoadAndViewersFollowPresence(t *testing.T) {
	local := &fleet.Report{Address: mainAddress}
	e := fleetEnv(t, local, strings.Repeat("a", 32))
	if code := e.do(http.MethodPost, "/v1/node/report", fleet.Report{Address: remoteNode}, nil).Code; code != http.StatusNotFound {
		t.Fatalf("the HTTP node report is gone, got %d", code)
	}
	e.linkNode(fleet.Report{Address: remoteNode})

	// The Center node has capacity 1: the first camera goes there, the next to sg1.
	_, first := e.challenge3()
	_, second := e.challenge3()
	if first != mainAddress || second != remoteNode {
		t.Fatalf("assigned %s then %s", first, second)
	}

	d := newDevice()
	sidText := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))
	nonce, node := e.challenge3()
	response := e.do(http.MethodPost, "/verify", e.proof3(d, nonce, sidText, node), nil)
	var camera verifyResult
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &camera) != nil || camera.MainServerAddress != node {
		t.Fatalf("verify = %d %s", response.Code, response.Body.String())
	}
	// The camera actually shows up on the Center node: viewers go there.
	local.Cameras = []string{camera.DeviceID}
	var viewer viewResult
	view := e.do(http.MethodPost, "/v1/view", map[string]string{"sid": sidText}, nil)
	if json.Unmarshal(view.Body.Bytes(), &viewer) != nil || viewer.MainServerAddress != mainAddress {
		t.Fatalf("viewer sent to %s, camera is on the Center node", viewer.MainServerAddress)
	}
	if e.ticketAudience(viewer.Ticket, mainAddress) != nil {
		t.Fatal("viewer ticket is not for the camera's node")
	}
}

func TestStatsAddsUpTheFleet(t *testing.T) {
	today := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC).Format(time.DateOnly) // env clock date
	local := &fleet.Report{Address: mainAddress, Stats: stats.Days{today: {Requests: 2, Camera: stats.Outcomes{Connected: 1}}}}
	token := strings.Repeat("a", 32)
	e := fleetEnv(t, local, token)
	e.linkNode(fleet.Report{Address: remoteNode,
		Stats: stats.Days{today: {Requests: 3, Player: stats.Outcomes{Failed: 1, Errors: map[string]int{"no_path": 1}}}}})

	// Statistics are not served on the public port, even with the admin token.
	if code := e.do(http.MethodGet, "/v1/admin/stats?days=1", nil, map[string]string{"Authorization": "Bearer " + token}).Code; code != http.StatusNotFound {
		t.Fatalf("public stats = %d", code)
	}
	result := e.handler.Stats(1)
	if result.Total.Requests != 5 || result.Total.Camera.Connected != 1 || result.Total.Player.Errors["no_path"] != 1 || len(result.Nodes) != 2 {
		t.Fatalf("fleet stats = %+v", result)
	}
}

// A Center whose own node has the camera sends viewers there, even when it
// would assign new cameras elsewhere and never assigned this one itself.
func TestViewersGoToTheCentersOwnNodeWhenTheCameraIsHere(t *testing.T) {
	var here string
	e := newEnv(t, withAssignedNode, func(c *Config) {
		c.LocalCamera = func(id string) bool { return id == here }
	})
	sidText := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))
	camera := e.mustVerify(newDevice(), sidText) // bound here; then imagine it connected after a restart
	e.handler.nodesMu.Lock()
	delete(e.handler.nodes, camera.DeviceID)
	e.handler.nodesMu.Unlock()
	viewNode := func() string {
		var viewer viewResult
		view := e.do(http.MethodPost, "/v1/view", map[string]string{"sid": sidText}, nil)
		_ = json.Unmarshal(view.Body.Bytes(), &viewer)
		return viewer.MainServerAddress
	}
	if got := viewNode(); got != assignedNode {
		t.Fatalf("unknown camera sent to %s, want the assigned node", got)
	}
	here = camera.DeviceID
	if got := viewNode(); got != mainAddress {
		t.Fatalf("camera connected here, viewer sent to %s", got)
	}
}
