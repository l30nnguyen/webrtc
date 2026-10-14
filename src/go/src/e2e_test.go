package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"signaling-server/internal/activation"
	"signaling-server/internal/admission"
	"signaling-server/internal/center"
	"signaling-server/internal/sid"
)

// TestEndToEndCameraSIDViewScope walks camera activation and the real viewer
// flow: camera SID -> challenge activation -> scoped view ticket -> WebSocket.
func TestEndToEndCameraSIDViewScope(t *testing.T) {
	enc := base64.RawURLEncoding
	issuerPublic, issuerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	centerPublic, centerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	dir := t.TempDir()
	blocklist := filepath.Join(dir, "blocklist.txt")
	if err := os.WriteFile(blocklist, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := activation.Open(filepath.Join(dir, "activations.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// One TLS listener serves both Center HTTP endpoints and ticket-admitted WSS.
	var nodeAddress string
	srv := NewServer("", "")
	httpNode := httptest.NewUnstartedServer(srv)
	configureHTTPServer(httpNode.Config)
	httpNode.StartTLS()
	defer httpNode.Close()
	nodeAddress = "wss" + strings.TrimPrefix(httpNode.URL, "https") + "/"

	handler, err := center.NewHandler(center.Config{
		SIDPublicKey: issuerPublic, MinIssued: time.Now().Add(-24 * time.Hour),
		Signer:      admission.TicketSigner{PrivateKey: centerPrivate, KeyID: "center-v1", Audience: nodeAddress},
		MainAddress: nodeAddress, Blocklist: admission.Blocklist{Path: blocklist},
		Tenants:     center.StaticTenants{"acme01": {}, "other01": {}},
		Activations: store, ChallengeKey: bytes.Repeat([]byte{9}, 32),
		Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.SetCenterHandler(handler)
	srv.SetAdmission(admission.Validator{
		Tickets:   admission.TicketVerifier{PublicKey: centerPublic, KeyID: "center-v1", Audience: nodeAddress},
		Blocklist: admission.Blocklist{Path: blocklist},
		Replay:    &admission.ReplayCache{},
	})
	client := httpNode.Client()
	reserved, err := http.NewRequest(http.MethodGet, httpNode.URL+"/v1/challenge", nil)
	if err != nil {
		t.Fatal(err)
	}
	reserved.Header.Set("Connection", "Upgrade")
	reserved.Header.Set("Upgrade", "websocket")
	reserved.Header.Set("Sec-WebSocket-Version", "13")
	reserved.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	reservedResponse, err := client.Do(reserved)
	if err != nil {
		t.Fatal(err)
	}
	reservedResponse.Body.Close()
	if reservedResponse.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("Center route with WebSocket headers returned %d, want 405", reservedResponse.StatusCode)
	}

	post := func(path string, body any, headers map[string]string) (int, []byte) {
		data, _ := json.Marshal(body)
		request, _ := http.NewRequest(http.MethodPost, httpNode.URL+path, bytes.NewReader(data))
		request.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			request.Header.Set(k, v)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(response.Body)
		return response.StatusCode, buf.Bytes()
	}

	type session struct {
		deviceID string
		conn     *websocket.Conn
		ticket   string
	}
	activate := func(sidText string) session {
		devicePublic, devicePrivate, _ := ed25519.GenerateKey(rand.Reader)
		_, body := post("/v1/challenge", map[string]any{}, nil)
		var challenge struct {
			Nonce string `json:"nonce"`
		}
		_ = json.Unmarshal(body, &challenge)
		nonce, _ := enc.DecodeString(challenge.Nonce)
		sig, _ := activation.Sign(devicePrivate, nonce, sidText, nodeAddress)
		status, body := post("/verify", map[string]any{"proto": 2, "sid": sidText, "device_pub": enc.EncodeToString(devicePublic), "nonce": challenge.Nonce, "sig": enc.EncodeToString(sig)}, nil)
		if status != http.StatusOK {
			t.Fatalf("verify = %d %s", status, body)
		}
		var result struct {
			Ticket   string `json:"ticket"`
			DeviceID string `json:"device_id"`
		}
		_ = json.Unmarshal(body, &result)
		dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
		conn, response, err := dialer.Dial(nodeAddress+result.DeviceID+"?ticket="+url.QueryEscape(result.Ticket), nil)
		if err != nil {
			t.Fatalf("node refused the Center ticket: %v", err)
		}
		response.Body.Close()
		t.Cleanup(func() { conn.Close() })
		return session{result.DeviceID, conn, result.Ticket}
	}
	view := func(sidText string) session {
		status, body := post("/v1/view", map[string]string{"sid": sidText}, nil)
		if status != http.StatusOK {
			t.Fatalf("view = %d %s", status, body)
		}
		var result struct {
			Ticket   string `json:"ticket"`
			DeviceID string `json:"device_id"`
			CameraID string `json:"camera_id"`
			Role     string `json:"role"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
		if result.Role != admission.RolePlayer {
			t.Fatalf("view ticket role=%q", result.Role)
		}
		dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
		conn, response, err := dialer.Dial(nodeAddress+result.DeviceID+"?ticket="+url.QueryEscape(result.Ticket), nil)
		if err != nil {
			t.Fatalf("node refused the scoped view ticket: %v", err)
		}
		response.Body.Close()
		t.Cleanup(func() { conn.Close() })
		return session{result.DeviceID, conn, result.Ticket}
	}
	issue := func(tenant string, flags uint16) string {
		text, err := sid.Issue(time.Now(), tenant, flags, issuerPrivate, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return text
	}

	cameraSID := issue("acme01", sid.FlagPublisher)
	camera := activate(cameraSID)
	// Ticket jti replay is rejected after the initial WSS admission.
	dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	_, replayResponse, replayErr := dialer.Dial(nodeAddress+camera.deviceID+"?ticket="+url.QueryEscape(camera.ticket), nil)
	if replayErr == nil {
		replayResponse.Body.Close()
		t.Fatal("replayed camera ticket was accepted")
	}
	if replayResponse == nil || replayResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replayed ticket response = %v, err=%v", replayResponse, replayErr)
	}
	otherCamera := activate(issue("other01", sid.FlagPublisher))
	player := view(cameraSID)
	time.Sleep(100 * time.Millisecond) // let the node register the sockets

	// A viewer can only target the activated camera named by its ticket. Caller
	// supplied identity metadata is overwritten with the camera SID digest.
	send(t, player.conn, map[string]any{"type": "request", "id": camera.deviceID, "sid_digest": "forged", "password": "camera-password"})
	if got := receive(camera.conn, time.Second); got == nil || got["id"] != player.deviceID || got["sid_digest"] != center.Digest(cameraSID) || got["password"] != "camera-password" {
		t.Fatalf("camera did not receive scoped request: %v", got)
	}
	send(t, player.conn, map[string]any{"type": "request", "id": otherCamera.deviceID, "password": "camera-password"})
	if reply := receive(player.conn, time.Second); reply == nil || reply["msg"] != "Device offline" {
		t.Fatalf("cross-tenant camera was not hidden as offline: %v", reply)
	}
	if got := receive(otherCamera.conn, 200*time.Millisecond); got != nil {
		t.Fatalf("camera in another tenant received the request: %v", got)
	}
}
