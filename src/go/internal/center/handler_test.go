package center

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"signaling-server/internal/activation"
	"signaling-server/internal/admission"
	"signaling-server/internal/sid"
)

const mainAddress = "wss://main.example.test:8000/"

var enc = base64.RawURLEncoding

type env struct {
	t              *testing.T
	handler        *Handler
	now            time.Time
	clock          *time.Time
	issuerPrivate  ed25519.PrivateKey
	issuerPublic   ed25519.PublicKey
	centerPublic   ed25519.PublicKey
	blocklistPath  string
	store          *activation.Store
	logs           *logCapture
	configure      func(*Config)
	tenants        StaticTenants
	storeInterface Activations
}

type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCapture) Logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logCapture) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func newEnv(t *testing.T, mutate ...func(*Config)) *env {
	t.Helper()
	e := &env{t: t, now: time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC), logs: &logCapture{}}
	e.clock = &e.now
	e.issuerPublic, e.issuerPrivate, _ = ed25519.GenerateKey(rand.Reader)
	var centerPrivate ed25519.PrivateKey
	e.centerPublic, centerPrivate, _ = ed25519.GenerateKey(rand.Reader)
	e.blocklistPath = filepath.Join(t.TempDir(), "blocklist.txt")
	if err := os.WriteFile(e.blocklistPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := activation.Open(filepath.Join(t.TempDir(), "activations.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	e.store = store
	e.tenants = StaticTenants{"acme01": {}, "other01": {}, "shortwin": {ActivationWindow: 24 * time.Hour}}
	config := Config{
		SIDPublicKey: e.issuerPublic, MinIssued: e.now.Add(-5 * 366 * 24 * time.Hour),
		Signer:      admission.TicketSigner{PrivateKey: centerPrivate, KeyID: "center-v1", Audience: mainAddress},
		MainAddress: mainAddress, Blocklist: admission.Blocklist{Path: e.blocklistPath},
		Tenants: e.tenants, Activations: store, ChallengeKey: bytes.Repeat([]byte{7}, 32),
		Now: func() time.Time { return *e.clock }, Logf: e.logs.Logf,
	}
	for _, m := range mutate {
		m(&config)
	}
	e.handler, err = NewHandler(config)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) issue(tenant string, flags uint16, issuedAt time.Time) string {
	e.t.Helper()
	text, err := sid.Issue(issuedAt, tenant, flags, e.issuerPrivate, rand.Reader)
	if err != nil {
		e.t.Fatal(err)
	}
	return text
}

func (e *env) digest(sidText string) string { return Digest(sidText) }

func (e *env) do(method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	switch b := body.(type) {
	case nil:
		reader = bytes.NewReader(nil)
	case string:
		reader = bytes.NewReader([]byte(b))
	default:
		data, _ := json.Marshal(b)
		reader = bytes.NewReader(data)
	}
	request := httptest.NewRequest(method, path, reader)
	request.TLS = &tls.ConnectionState{}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	recorder := httptest.NewRecorder()
	e.handler.ServeHTTP(recorder, request)
	return recorder
}

type device struct {
	public  ed25519.PublicKey
	private ed25519.PrivateKey
}

func newDevice() device {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	return device{public, private}
}

func (e *env) challenge() string {
	e.t.Helper()
	response := e.do(http.MethodPost, "/v1/challenge", "{}", nil)
	if response.Code != http.StatusOK {
		e.t.Fatalf("challenge status = %d", response.Code)
	}
	var result struct {
		Nonce     string `json:"nonce"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.ExpiresIn != 30 {
		e.t.Fatalf("challenge body = %s (%v)", response.Body.String(), err)
	}
	return result.Nonce
}

func (e *env) proof(d device, nonceText, sidText, origin string) map[string]any {
	nonce, _ := enc.DecodeString(nonceText)
	sig, err := activation.Sign(d.private, nonce, sidText, origin)
	if err != nil {
		e.t.Fatal(err)
	}
	return map[string]any{"proto": 2, "sid": sidText, "device_pub": enc.EncodeToString(d.public), "nonce": nonceText, "sig": enc.EncodeToString(sig)}
}

type verifyResult struct {
	MainServerAddress string `json:"main_server_address"`
	Ticket            string `json:"ticket"`
	ExpiresIn         int    `json:"expires_in"`
	DeviceID          string `json:"device_id"`
	Tenant            string `json:"tenant"`
	Role              string `json:"role"`
}

type viewResult struct {
	MainServerAddress string `json:"main_server_address"`
	Ticket            string `json:"ticket"`
	ExpiresIn         int    `json:"expires_in"`
	DeviceID          string `json:"device_id"`
	CameraID          string `json:"camera_id"`
	Tenant            string `json:"tenant"`
	Role              string `json:"role"`
}

func (e *env) verify(d device, sidText string) (*httptest.ResponseRecorder, verifyResult) {
	e.t.Helper()
	response := e.do(http.MethodPost, "/verify", e.proof(d, e.challenge(), sidText, mainAddress), nil)
	var result verifyResult
	if response.Code == http.StatusOK {
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			e.t.Fatal(err)
		}
	}
	return response, result
}

func (e *env) mustVerify(d device, sidText string) verifyResult {
	e.t.Helper()
	response, result := e.verify(d, sidText)
	if response.Code != http.StatusOK {
		e.t.Fatalf("verify status = %d body=%s", response.Code, response.Body.String())
	}
	return result
}

func (e *env) block(lines ...string) {
	e.t.Helper()
	if err := os.WriteFile(e.blocklistPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		e.t.Fatal(err)
	}
	future := e.now.Add(time.Duration(len(lines)+1) * time.Hour)
	_ = os.Chtimes(e.blocklistPath, future, future)
}

func (e *env) claims(token string) admission.Claims {
	e.t.Helper()
	claims, err := (admission.TicketVerifier{PublicKey: e.centerPublic, KeyID: "center-v1", Audience: mainAddress, Now: func() time.Time { return *e.clock }}).VerifyAny(token)
	if err != nil {
		e.t.Fatalf("ticket does not verify: %v", err)
	}
	return claims
}

func TestVerifyActivatesAndIssuesBoundedTicket(t *testing.T) {
	e := newEnv(t)
	d := newDevice()
	sidText := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))
	result := e.mustVerify(d, sidText)

	wantID := activation.DeviceID(d.public)
	if result.DeviceID != wantID || result.Tenant != "acme01" || result.Role != "camera" || result.MainServerAddress != mainAddress || result.ExpiresIn != 60 || result.Ticket == "" {
		t.Fatalf("unexpected result: %+v", result)
	}
	claims := e.claims(result.Ticket)
	if claims.Subject != wantID || claims.Tenant != "acme01" || claims.Role != "camera" || claims.Digest != e.digest(sidText) || claims.Expires-claims.IssuedAt != 60 {
		t.Fatalf("unexpected claims: %+v", claims)
	}
	record, ok := e.store.Get(e.digest(sidText))
	if !ok || record.DeviceID != wantID || record.CameraRef != sid.CameraRef(sidText) || record.Role != "camera" {
		t.Fatalf("activation record = %+v ok=%v", record, ok)
	}
	if strings.Contains(e.logs.joined(), sidText) {
		t.Fatal("the SID was written to the log")
	}
}

func TestViewExchangesActivatedCameraSIDForScopedShortTicket(t *testing.T) {
	e := newEnv(t)
	camera := newDevice()
	sidText := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))
	activated := e.mustVerify(camera, sidText)
	if activated.Role != admission.RoleCamera {
		t.Fatalf("camera role = %q", activated.Role)
	}

	response := e.do(http.MethodPost, "/v1/view", map[string]string{"sid": sidText}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("view status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("view token response is cacheable: %v", response.Header())
	}
	var result viewResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	claims := e.claims(result.Ticket)
	if result.MainServerAddress != mainAddress || result.ExpiresIn != 60 || result.Role != admission.RolePlayer || result.Tenant != "acme01" || result.CameraID != activated.DeviceID || result.DeviceID != claims.Subject || result.DeviceID == result.CameraID || claims.Role != admission.RolePlayer || claims.Digest != e.digest(sidText) || claims.CameraID != result.CameraID || claims.Expires-claims.IssuedAt != 60 || len(result.DeviceID) != 43 {
		t.Fatalf("view response=%+v claims=%+v", result, claims)
	}
	if strings.Contains(response.Body.String(), sidText) || strings.Contains(e.logs.joined(), sidText) {
		t.Fatal("raw camera SID was returned or logged")
	}
}

func TestViewRejectsInvalidOrUnavailableCameraSID(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*env) string
		want  int
	}{
		{name: "unactivated", setup: func(e *env) string { return e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour)) }, want: http.StatusUnauthorized},
		{name: "viewer license", setup: func(e *env) string { return e.issue("acme01", sid.FlagViewer, e.now.Add(-time.Hour)) }, want: http.StatusUnauthorized},
		{name: "unknown tenant", setup: func(e *env) string { return e.issue("other99", sid.FlagPublisher, e.now.Add(-time.Hour)) }, want: http.StatusUnauthorized},
		{name: "blocked camera SID", setup: func(e *env) string {
			d := newDevice()
			sidText := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))
			e.mustVerify(d, sidText)
			e.block(e.digest(sidText))
			return sidText
		}, want: http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := newEnv(t)
			sidText := test.setup(e)
			response := e.do(http.MethodPost, "/v1/view", map[string]string{"sid": sidText}, nil)
			if response.Code != test.want {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), test.want)
			}
		})
	}
}

func TestViewAllowsOnlyConfiguredBrowserOrigin(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.ViewOrigin = "https://viewer.example.test" })
	good := e.do(http.MethodOptions, "/v1/view", nil, map[string]string{
		"Origin": "https://viewer.example.test", "Access-Control-Request-Method": "POST",
	})
	if good.Code != http.StatusNoContent || good.Header().Get("Access-Control-Allow-Origin") != "https://viewer.example.test" {
		t.Fatalf("allowed preflight: status=%d headers=%v", good.Code, good.Header())
	}
	bad := e.do(http.MethodOptions, "/v1/view", nil, map[string]string{
		"Origin": "https://evil.example.test", "Access-Control-Request-Method": "POST",
	})
	if bad.Code != http.StatusForbidden || bad.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("unlisted origin: status=%d headers=%v", bad.Code, bad.Header())
	}
}

func TestSecondSeatIsRejectedAndSameDeviceKeepsWorking(t *testing.T) {
	e := newEnv(t)
	owner, intruder := newDevice(), newDevice()
	sidText := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))
	e.mustVerify(owner, sidText)

	if response, _ := e.verify(intruder, sidText); response.Code != http.StatusUnauthorized {
		t.Fatalf("second key status = %d, want 401", response.Code)
	}
	if !strings.Contains(e.logs.joined(), "ALERT") {
		t.Fatalf("no alert logged for a second key; logs=%q", e.logs.joined())
	}
	e.mustVerify(owner, sidText) // the owner is unaffected
	if e.store.Count() != 1 {
		t.Fatalf("activation count = %d, want 1", e.store.Count())
	}
}

func TestClientClaimedDeviceIDIsNotAccepted(t *testing.T) {
	e := newEnv(t)
	d := newDevice()
	sidText := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))
	body := e.proof(d, e.challenge(), sidText, mainAddress)
	body["device_id"] = "victim-camera"
	if response := e.do(http.MethodPost, "/verify", body, nil); response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown field)", response.Code)
	}
	// And the ticket subject always derives from the proven key.
	result := e.mustVerify(d, sidText)
	if result.DeviceID != activation.DeviceID(d.public) {
		t.Fatalf("device id = %q", result.DeviceID)
	}
}

func TestChallengeIsSingleUseFreshAndBoundToSIDAndOrigin(t *testing.T) {
	e := newEnv(t)
	d := newDevice()
	sidText := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))
	otherSID := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))

	nonce := e.challenge()
	body := e.proof(d, nonce, sidText, mainAddress)
	if response := e.do(http.MethodPost, "/verify", body, nil); response.Code != http.StatusOK {
		t.Fatalf("first use status = %d", response.Code)
	}
	if response := e.do(http.MethodPost, "/verify", body, nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("replayed challenge status = %d, want 401", response.Code)
	}

	for name, mutate := range map[string]func() map[string]any{
		"proof for another SID": func() map[string]any {
			b := e.proof(d, e.challenge(), otherSID, mainAddress)
			b["sid"] = sidText
			return b
		},
		"proof for another origin": func() map[string]any {
			return e.proof(d, e.challenge(), otherSID, "wss://evil.example.test/")
		},
		"proof by another key": func() map[string]any {
			b := e.proof(d, e.challenge(), otherSID, mainAddress)
			b["device_pub"] = enc.EncodeToString(newDevice().public)
			return b
		},
		"forged nonce": func() map[string]any {
			return e.proof(d, enc.EncodeToString(make([]byte, activation.NonceSize)), otherSID, mainAddress)
		},
		"stale nonce": func() map[string]any {
			b := e.proof(d, e.challenge(), otherSID, mainAddress)
			*e.clock = e.clock.Add(31 * time.Second)
			return b
		},
	} {
		t.Run(name, func(t *testing.T) {
			saved := *e.clock
			defer func() { *e.clock = saved }()
			if response := e.do(http.MethodPost, "/verify", mutate(), nil); response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", response.Code)
			}
		})
	}
}

func TestTenantPolicy(t *testing.T) {
	e := newEnv(t)
	d := newDevice()
	t.Run("unknown tenant", func(t *testing.T) {
		if response, _ := e.verify(d, e.issue("nobody01", sid.FlagPublisher, e.now.Add(-time.Hour))); response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d", response.Code)
		}
	})
	t.Run("suspended tenant", func(t *testing.T) {
		e.block("tenant:acme01")
		defer e.block()
		if response, _ := e.verify(d, e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))); response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d", response.Code)
		}
		if response, _ := e.verify(d, e.issue("other01", sid.FlagPublisher, e.now.Add(-time.Hour))); response.Code != http.StatusOK {
			t.Fatalf("another tenant affected by the suspension: status = %d", response.Code)
		}
	})
	t.Run("blocked SID", func(t *testing.T) {
		sidText := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))
		e.block(e.digest(sidText))
		defer e.block()
		if response, _ := e.verify(newDevice(), sidText); response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d", response.Code)
		}
	})
}

func TestActivationWindow(t *testing.T) {
	e := newEnv(t)
	d := newDevice()
	old := e.issue("acme01", sid.FlagPublisher, e.now.Add(-3*365*24*time.Hour-time.Hour))
	if response, _ := e.verify(d, old); response.Code != http.StatusUnauthorized {
		t.Fatalf("SID unactivated for over 3 years status = %d, want 401", response.Code)
	}
	if !strings.Contains(e.logs.joined(), "not bound within") {
		t.Fatalf("expiry not logged: %q", e.logs.joined())
	}
	almost := e.issue("acme01", sid.FlagPublisher, e.now.Add(-3*365*24*time.Hour+time.Hour))
	e.mustVerify(d, almost)

	// per-tenant window, and an activated SID never expires
	short := e.issue("shortwin", sid.FlagPublisher, e.now.Add(-48*time.Hour))
	if response, _ := e.verify(d, short); response.Code != http.StatusUnauthorized {
		t.Fatalf("tenant window not applied: %d", response.Code)
	}
	fresh := e.issue("shortwin", sid.FlagPublisher, e.now.Add(-time.Hour))
	e.mustVerify(d, fresh)
	*e.clock = e.now.Add(10 * 365 * 24 * time.Hour)
	e.handler.config.MinIssued = e.now.Add(-time.Hour * 24 * 365 * 20)
	e.mustVerify(d, fresh)
}

func TestBadRequestsAreRejectedWithoutEchoingCredentials(t *testing.T) {
	e := newEnv(t)
	d := newDevice()
	good := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))
	base := func() map[string]any { return e.proof(d, e.challenge(), good, mainAddress) }
	for name, mutate := range map[string]func(map[string]any){
		"wrong proto":       func(b map[string]any) { b["proto"] = 1 },
		"bad pub":           func(b map[string]any) { b["device_pub"] = "AAAA" },
		"bad nonce":         func(b map[string]any) { b["nonce"] = "AAAA" },
		"bad sig":           func(b map[string]any) { b["sig"] = "AAAA" },
		"padded sid":        func(b map[string]any) { b["sid"] = good + " " },
		"missing fields":    func(b map[string]any) { delete(b, "sig") },
		"noncanonical sig":  func(b map[string]any) { b["sig"] = b["sig"].(string)[:85] + "B" },
		"proto as string":   func(b map[string]any) { b["proto"] = "2" },
		"unexpected nested": func(b map[string]any) { b["x"] = map[string]any{} },
	} {
		t.Run(name, func(t *testing.T) {
			body := base()
			mutate(body)
			response := e.do(http.MethodPost, "/verify", body, nil)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s, want 400", response.Code, response.Body.String())
			}
		})
	}
	t.Run("tampered SID is 401 and not echoed", func(t *testing.T) {
		tampered := "tampered-secret"
		body := base()
		body["sid"] = tampered
		response := e.do(http.MethodPost, "/verify", body, nil)
		if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), tampered) {
			t.Fatalf("unsafe response: %d %q", response.Code, response.Body.String())
		}
	})
	t.Run("wrong SID issuer key", func(t *testing.T) {
		other := newEnv(t)
		response := other.do(http.MethodPost, "/verify", other.proof(d, other.challenge(), good, mainAddress), nil)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d", response.Code)
		}
	})
	t.Run("wrong method and content type and path", func(t *testing.T) {
		if r := e.do(http.MethodGet, "/verify", nil, nil); r.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET /verify = %d", r.Code)
		}
		if r := e.do(http.MethodPost, "/verify", "{}", map[string]string{"Content-Type": "text/plain"}); r.Code != http.StatusBadRequest {
			t.Fatalf("text/plain = %d", r.Code)
		}
		if r := e.do(http.MethodPost, "/nope", "{}", nil); r.Code != http.StatusNotFound {
			t.Fatalf("/nope = %d", r.Code)
		}
	})
}

func TestBlocklistReadFailureReturnsServiceUnavailable(t *testing.T) {
	e := newEnv(t)
	d := newDevice()
	sidText := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))
	body := e.proof(d, e.challenge(), sidText, mainAddress)
	if err := os.Remove(e.blocklistPath); err != nil {
		t.Fatal(err)
	}
	if response := e.do(http.MethodPost, "/verify", body, nil); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
}

func TestHTTPSRequirementAndTrustedProxy(t *testing.T) {
	e := newEnv(t)
	plain := func(h *Handler, headers map[string]string) int {
		request := httptest.NewRequest(http.MethodPost, "/v1/challenge", strings.NewReader("{}"))
		request.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			request.Header.Set(k, v)
		}
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, request)
		return recorder.Code
	}
	if code := plain(e.handler, nil); code != http.StatusBadRequest {
		t.Fatalf("plain HTTP = %d, want 400", code)
	}
	if code := plain(e.handler, map[string]string{"X-Forwarded-Proto": "https"}); code != http.StatusBadRequest {
		t.Fatalf("forwarded header honoured without TrustProxyTLS: %d", code)
	}
	proxied := newEnv(t, func(c *Config) { c.TrustProxyTLS = true })
	if code := plain(proxied.handler, map[string]string{"X-Forwarded-Proto": "https"}); code != http.StatusOK {
		t.Fatalf("trusted proxy = %d, want 200", code)
	}
	if code := plain(proxied.handler, map[string]string{"X-Forwarded-Proto": "http"}); code != http.StatusBadRequest {
		t.Fatalf("http forwarded proto = %d, want 400", code)
	}
}

func TestResolveKeepsTenantsApart(t *testing.T) {
	e := newEnv(t)
	cameraSID := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))
	camera := newDevice()
	cameraResult := e.mustVerify(camera, cameraSID)
	ref := sid.CameraRef(cameraSID)

	resolve := func(ticket, ref string) (int, resolveResponse) {
		headers := map[string]string{}
		if ticket != "" {
			headers["Authorization"] = "Bearer " + ticket
		}
		response := e.do(http.MethodPost, "/v1/resolve", map[string]string{"ref": ref}, headers)
		var result resolveResponse
		_ = json.Unmarshal(response.Body.Bytes(), &result)
		return response.Code, result
	}

	player := e.mustVerify(newDevice(), e.issue("acme01", sid.FlagViewer, e.now.Add(-time.Hour)))
	if code, result := resolve(player.Ticket, ref); code != 200 || !result.Found || result.DeviceID != cameraResult.DeviceID || result.MainServerAddress != mainAddress {
		t.Fatalf("same-tenant resolve = %d %+v", code, result)
	}
	if code, result := resolve(player.Ticket, strings.ToUpper(ref)); code != 200 || !result.Found {
		t.Fatalf("uppercase ref not accepted: %d %+v", code, result)
	}

	stranger := e.mustVerify(newDevice(), e.issue("other01", sid.FlagViewer, e.now.Add(-time.Hour)))
	if code, result := resolve(stranger.Ticket, ref); code != 200 || result.Found || result.DeviceID != "" {
		t.Fatalf("cross-tenant resolve leaked: %d %+v", code, result)
	}
	if code, result := resolve(player.Ticket, strings.Repeat("0", 64)); code != 200 || result.Found {
		t.Fatalf("unknown ref = %d %+v", code, result)
	}
	if code, result := resolve(player.Ticket, "not-hex"); code != 200 || result.Found {
		t.Fatalf("malformed ref = %d %+v", code, result)
	}
	if code, _ := resolve("", ref); code != http.StatusUnauthorized {
		t.Fatalf("no ticket = %d", code)
	}
	if code, _ := resolve("garbage", ref); code != http.StatusUnauthorized {
		t.Fatalf("garbage ticket = %d", code)
	}
	if code, _ := resolve(cameraResult.Ticket, ref); code != http.StatusUnauthorized {
		t.Fatalf("camera ticket accepted for resolve = %d", code)
	}
	// A viewer SID is never a resolvable camera.
	viewerSID := e.issue("acme01", sid.FlagViewer, e.now.Add(-time.Hour))
	e.mustVerify(newDevice(), viewerSID)
	if _, result := resolve(player.Ticket, sid.CameraRef(viewerSID)); result.Found {
		t.Fatal("a viewer SID resolved as a camera")
	}
	// Blocking the camera or its tenant hides it.
	e.block(e.digest(cameraSID))
	if _, result := resolve(player.Ticket, ref); result.Found {
		t.Fatal("blocked camera still resolves")
	}
	e.block("tenant:acme01")
	if code, _ := resolve(player.Ticket, ref); code != http.StatusUnauthorized {
		t.Fatalf("suspended tenant's player still resolves: %d", code)
	}
}

func TestDemoResolveIsOffUnlessConfiguredAndTenantScoped(t *testing.T) {
	off := newEnv(t)
	if r := off.do(http.MethodGet, "/v1/demo/resolve?ref="+strings.Repeat("0", 64), nil, nil); r.Code != http.StatusNotFound {
		t.Fatalf("demo resolve enabled by default: %d", r.Code)
	}
	e := newEnv(t, func(c *Config) { c.DemoTenant = "acme01" })
	demoSID := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))
	otherSID := e.issue("other01", sid.FlagPublisher, e.now.Add(-time.Hour))
	e.mustVerify(newDevice(), demoSID)
	e.mustVerify(newDevice(), otherSID)
	get := func(ref string) (*httptest.ResponseRecorder, resolveResponse) {
		r := e.do(http.MethodGet, "/v1/demo/resolve?ref="+ref, nil, nil)
		var result resolveResponse
		_ = json.Unmarshal(r.Body.Bytes(), &result)
		return r, result
	}
	if r, result := get(sid.CameraRef(demoSID)); r.Code != 200 || !result.Found || r.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("demo tenant camera = %d %+v", r.Code, result)
	}
	if _, result := get(sid.CameraRef(otherSID)); result.Found {
		t.Fatal("demo resolve reached another tenant")
	}
	if r := e.do(http.MethodOptions, "/v1/demo/resolve", nil, nil); r.Code != http.StatusNoContent {
		t.Fatalf("preflight = %d", r.Code)
	}
	if r := e.do(http.MethodPost, "/v1/demo/resolve", "{}", nil); r.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST demo = %d", r.Code)
	}
}

func TestNewHandlerRejectsUnsafeConfiguration(t *testing.T) {
	good := newEnv(t)
	cfg := good.handler.config
	for name, mutate := range map[string]func(*Config){
		"short challenge key": func(c *Config) { c.ChallengeKey = c.ChallengeKey[:8] },
		"no tenants":          func(c *Config) { c.Tenants = nil },
		"no store":            func(c *Config) { c.Activations = nil },
		"long ticket":         func(c *Config) { c.Signer.Lifetime = time.Hour },
		"bad demo tenant":     func(c *Config) { c.DemoTenant = "X" },
		"signer is issuer": func(c *Config) {
			c.Signer.PrivateKey = good.issuerPrivate
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := cfg
			mutate(&c)
			if _, err := NewHandler(c); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}

func TestTenantFileReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tenants.json")
	if err := os.WriteFile(path, []byte(`{"acme01":{},"slow0001":{"activation_window_days":10}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	file := &TenantFile{Path: path}
	if err := file.Validate(); err != nil {
		t.Fatal(err)
	}
	if tenant, ok, err := file.Lookup("slow0001"); err != nil || !ok || tenant.ActivationWindow != 240*time.Hour {
		t.Fatalf("lookup = %+v %v %v", tenant, ok, err)
	}
	if _, ok, _ := file.Lookup("newco001"); ok {
		t.Fatal("unregistered tenant found")
	}
	if err := os.WriteFile(path, []byte(`{"newco001":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(path, future, future)
	if _, ok, err := file.Lookup("newco001"); err != nil || !ok {
		t.Fatalf("reload not applied: %v %v", ok, err)
	}
	for _, bad := range []string{`{"BAD":{}}`, `{"acme01":{"activation_window_days":-1}}`, `not json`} {
		os.WriteFile(path, []byte(bad), 0o600)
		_ = os.Chtimes(path, future.Add(time.Hour), future.Add(time.Hour))
		if _, _, err := file.Lookup("acme01"); err == nil {
			t.Fatalf("invalid tenants file %q accepted", bad)
		}
	}
}

func TestNonceHelpers(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	now := time.Unix(1_800_000_000, 0)
	nonce := newNonce(key, now)
	if len(nonce) != activation.NonceSize {
		t.Fatalf("nonce length = %d", len(nonce))
	}
	if _, ok := checkNonce(key, nonce, now.Add(29*time.Second)); !ok {
		t.Fatal("fresh nonce rejected")
	}
	if _, ok := checkNonce(key, nonce, now.Add(31*time.Second)); ok {
		t.Fatal("stale nonce accepted")
	}
	if _, ok := checkNonce(key, nonce, now.Add(-10*time.Second)); ok {
		t.Fatal("nonce from the future accepted")
	}
	if _, ok := checkNonce(bytes.Repeat([]byte{2}, 32), nonce, now); ok {
		t.Fatal("nonce accepted under another key")
	}
	tampered := append([]byte(nil), nonce...)
	tampered[3] ^= 1
	if _, ok := checkNonce(key, tampered, now); ok {
		t.Fatal("tampered timestamp accepted")
	}
	_ = hex.EncodeToString
}

func TestNoncesAreUniqueWithinTheSameSecond(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	now := time.Unix(1_800_000_000, 0)
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		n := string(newNonce(key, now))
		if seen[n] {
			t.Fatal("duplicate nonce within one second")
		}
		seen[n] = true
	}
}

func adminHeaders(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

var adminToken = strings.Repeat("t", 40)

func withAdmin(c *Config) { c.AdminToken = []byte(adminToken) }

func TestAdminRoutesDoNotExistWithoutAToken(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/v1/admin/status", "/v1/admin/release"} {
		if response := e.do(http.MethodPost, path, map[string]string{"sid_digest": strings.Repeat("a", 64)}, adminHeaders(adminToken)); response.Code != http.StatusNotFound {
			t.Fatalf("%s without a configured token = %d, want 404", path, response.Code)
		}
	}
	if _, err := NewHandler(Config{AdminToken: []byte("short")}); err == nil {
		t.Fatal("short admin token accepted")
	}
}

func TestAdminRoutesNeedTheToken(t *testing.T) {
	e := newEnv(t, withAdmin)
	body := map[string]string{"sid_digest": strings.Repeat("a", 64)}
	for _, headers := range []map[string]string{nil, adminHeaders("wrong"), adminHeaders(adminToken + "x"), {"Authorization": adminToken}} {
		for _, path := range []string{"/v1/admin/status", "/v1/admin/release"} {
			if response := e.do(http.MethodPost, path, body, headers); response.Code != http.StatusUnauthorized {
				t.Fatalf("%s with %v = %d, want 401", path, headers, response.Code)
			}
		}
	}
	if response := e.do(http.MethodPost, "/v1/admin/status", map[string]string{"sid_digest": "nothex"}, adminHeaders(adminToken)); response.Code != http.StatusBadRequest {
		t.Fatalf("bad digest = %d, want 400", response.Code)
	}
}

func (e *env) adminStatus(sidText string) map[string]any {
	e.t.Helper()
	response := e.do(http.MethodPost, "/v1/admin/status", map[string]string{"sid_digest": e.digest(sidText)}, adminHeaders(adminToken))
	if response.Code != http.StatusOK {
		e.t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		e.t.Fatal(err)
	}
	return result
}

func (e *env) release(sidText string) *httptest.ResponseRecorder {
	return e.do(http.MethodPost, "/v1/admin/release", map[string]string{"sid_digest": e.digest(sidText)}, adminHeaders(adminToken))
}

func TestPoolReleaseAndReassignmentToAnotherDevice(t *testing.T) {
	e := newEnv(t, withAdmin)
	validator := admission.Validator{
		Tickets:   admission.TicketVerifier{PublicKey: e.centerPublic, KeyID: "center-v1", Audience: mainAddress, Now: func() time.Time { return *e.clock }},
		Blocklist: admission.Blocklist{Path: e.blocklistPath},
	}
	oldDevice, newDevice := newDevice(), newDevice()
	sidText := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))
	if got := e.adminStatus(sidText)["state"]; got != "unbound" {
		t.Fatalf("fresh SID state = %v", got)
	}
	first := e.mustVerify(oldDevice, sidText)
	if got := e.adminStatus(sidText)["state"]; got != "bound" {
		t.Fatalf("bound SID state = %v", got)
	}
	if _, err := validator.Check(first.Ticket, first.DeviceID); err != nil {
		t.Fatalf("ticket before release: %v", err)
	}
	if response := e.do(http.MethodPost, "/v1/view", map[string]string{"sid": sidText}, nil); response.Code != http.StatusOK {
		t.Fatalf("view before release = %d", response.Code)
	}

	// A second device cannot take a SID that is still bound.
	if response, _ := e.verify(newDevice, sidText); response.Code != http.StatusUnauthorized {
		t.Fatalf("takeover of a bound SID = %d, want 401", response.Code)
	}

	response := e.release(sidText)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"changed":true`) {
		t.Fatalf("release = %d %s", response.Code, response.Body.String())
	}
	if revoked, _ := (admission.Blocklist{Path: e.blocklistPath}).DeviceRevoked(first.DeviceID, e.now); !revoked {
		t.Fatal("the old device was not revoked on the nodes")
	}
	if _, err := validator.Check(first.Ticket, first.DeviceID); err == nil {
		t.Fatal("the old device's unexpired ticket still passes after the release")
	}
	state := e.adminStatus(sidText)
	if state["state"] != "released" || state["bindable_at"] == nil {
		t.Fatalf("released SID state = %v", state)
	}
	if response := e.do(http.MethodPost, "/v1/view", map[string]string{"sid": sidText}, nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("a released SID can still be viewed: %d", response.Code)
	}
	if response := e.release(sidText); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"changed":false`) {
		t.Fatalf("second release = %d %s", response.Code, response.Body.String())
	}

	// Just released: the new device is told to retry; the old one is refused for good.
	if response, _ := e.verify(newDevice, sidText); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("bind inside the grace = %d, want 503", response.Code)
	}
	if response, _ := e.verify(oldDevice, sidText); response.Code != http.StatusUnauthorized {
		t.Fatalf("former holder = %d, want 401", response.Code)
	}
	if !strings.Contains(e.logs.joined(), "refused for former holder") {
		t.Fatal("the former holder's attempt was not flagged")
	}

	e.now = e.now.Add(activation.ReleaseGrace)
	second := e.mustVerify(newDevice, sidText)
	if second.DeviceID == first.DeviceID || e.claims(second.Ticket).Subject != second.DeviceID {
		t.Fatalf("new binding = %+v", second)
	}
	view := e.do(http.MethodPost, "/v1/view", map[string]string{"sid": sidText}, nil)
	var viewed viewResult
	if view.Code != http.StatusOK || json.Unmarshal(view.Body.Bytes(), &viewed) != nil || viewed.CameraID != second.DeviceID {
		t.Fatalf("view after reassignment = %d %s", view.Code, view.Body.String())
	}
	if response, _ := e.verify(oldDevice, sidText); response.Code != http.StatusUnauthorized {
		t.Fatalf("former holder after reassignment = %d, want 401", response.Code)
	}
	if _, err := validator.Check(second.Ticket, second.DeviceID); err != nil {
		t.Fatalf("the new device's ticket was refused: %v", err)
	}
}

func TestThreeYearWindowAppliesToEveryBind(t *testing.T) {
	e := newEnv(t, withAdmin)
	d1, d2 := newDevice(), newDevice()
	issued := e.now.Add(-3*365*24*time.Hour + 2*time.Hour)
	sidText := e.issue("acme01", sid.FlagPublisher, issued)
	e.mustVerify(d1, sidText)
	if response := e.release(sidText); response.Code != http.StatusOK {
		t.Fatalf("release = %d", response.Code)
	}
	e.now = e.now.Add(3 * time.Hour) // now past issue + 3 years
	if response, _ := e.verify(d2, sidText); response.Code != http.StatusUnauthorized {
		t.Fatalf("rebind of an expired pool SID = %d, want 401", response.Code)
	}
}

func TestReleaseOfUnknownSIDIs404AndUnboundNeverRevokes(t *testing.T) {
	e := newEnv(t, withAdmin)
	sidText := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))
	if response := e.release(sidText); response.Code != http.StatusNotFound {
		t.Fatalf("release of a never-bound SID = %d, want 404", response.Code)
	}
	data, _ := os.ReadFile(e.blocklistPath)
	if len(data) != 0 {
		t.Fatalf("blocklist changed: %q", data)
	}
}

func TestReleaseFailsClosedWhenTheBlocklistCannotBeWritten(t *testing.T) {
	e := newEnv(t, withAdmin)
	d := newDevice()
	sidText := e.issue("acme01", sid.FlagPublisher, e.now.Add(-time.Hour))
	e.mustVerify(d, sidText)
	os.Chmod(e.blocklistPath, 0o400)
	if os.Geteuid() != 0 {
		if response := e.release(sidText); response.Code != http.StatusServiceUnavailable {
			t.Fatalf("release with an unwritable blocklist = %d, want 503", response.Code)
		}
		if got := e.adminStatus(sidText)["state"]; got != "bound" {
			t.Fatalf("a failed release left the SID %v, want bound", got)
		}
	}
}
