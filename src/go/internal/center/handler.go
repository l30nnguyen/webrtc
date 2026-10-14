package center

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"signaling-server/internal/activation"
	"signaling-server/internal/admission"
	"signaling-server/internal/sid"
)

const MaxRequestBytes = 4096

// Activations is the binding store Center needs; *activation.Store implements it.
type Activations interface {
	Get(digest string) (activation.Record, bool)
	Bind(record activation.Record, now time.Time) (activation.Record, activation.BindResult, error)
	Release(digest string, now time.Time) (activation.Record, bool, error)
	LookupRef(tenant, ref string) (activation.Record, bool)
}

type Config struct {
	SIDPublicKey ed25519.PublicKey
	MinIssued    time.Time
	Signer       admission.TicketSigner
	MainAddress  string
	Blocklist    admission.Blocklist
	Tenants      TenantSource
	Activations  Activations
	// ChallengeKey keys the stateless challenge MAC (at least 32 bytes). All
	// Center instances behind one address must share it.
	ChallengeKey []byte
	// TrustProxyTLS accepts plain HTTP carrying "X-Forwarded-Proto: https", for a
	// Center behind a TLS-terminating load balancer. Only enable it when the
	// listener is reachable solely from that proxy.
	TrustProxyTLS bool
	// DemoTenant, when set, enables unauthenticated GET /v1/demo/resolve for that
	// one tenant so the demo web player can look up cameras. Never in production.
	DemoTenant string
	// AdminToken, when set (at least 32 bytes), enables the pairing system's
	// POST /v1/admin/status and /v1/admin/release, which require it as a bearer
	// token. Without it those routes do not exist.
	AdminToken []byte
	// ViewOrigin is an optional exact HTTPS origin allowed to call POST /v1/view
	// from a browser on another origin. Same-origin deployments leave it empty.
	ViewOrigin string
	Now        func() time.Time
	Logf       func(format string, args ...any)
}

type Handler struct {
	config  Config
	nonces  *admission.ReplayCache
	tickets admission.TicketVerifier
}

func NewHandler(config Config) (*Handler, error) {
	if len(config.SIDPublicKey) != ed25519.PublicKeySize || len(config.Signer.PrivateKey) != ed25519.PrivateKeySize || !admission.ValidKeyID(config.Signer.KeyID) {
		return nil, errors.New("invalid Center key configuration")
	}
	minUnix := config.MinIssued.Unix()
	if minUnix < 0 || uint64(minUnix) > math.MaxUint32 || (config.MinIssued.Nanosecond() != 0 && uint64(minUnix) == math.MaxUint32) {
		return nil, errors.New("minimum SID issue time is outside the SID timestamp range")
	}
	if bytes.Equal(config.Signer.PrivateKey.Public().(ed25519.PublicKey), config.SIDPublicKey) {
		return nil, errors.New("Center signing key must be separate from the SID issuer key")
	}
	if config.MainAddress == "" || config.Signer.Audience != config.MainAddress || admission.ValidateAudience(config.MainAddress) != nil {
		return nil, errors.New("MainServer address and ticket audience must be the same exact WSS root URL")
	}
	if config.Signer.Lifetime > admission.MaxTicketLifetime {
		return nil, errors.New("ticket lifetime exceeds the maximum")
	}
	if config.Blocklist.Path == "" {
		return nil, errors.New("blocklist file is required")
	}
	if err := config.Blocklist.Validate(); err != nil {
		return nil, err
	}
	if config.Tenants == nil || config.Activations == nil {
		return nil, errors.New("tenant registry and activation store are required")
	}
	if len(config.ChallengeKey) < 32 {
		return nil, errors.New("challenge key must be at least 32 bytes")
	}
	if len(config.AdminToken) != 0 && len(config.AdminToken) < 32 {
		return nil, errors.New("admin token must be at least 32 bytes")
	}
	if config.DemoTenant != "" && !sid.ValidTenant(config.DemoTenant) {
		return nil, errors.New("invalid demo tenant")
	}
	if config.ViewOrigin != "" {
		u, err := url.Parse(config.ViewOrigin)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.String() != config.ViewOrigin {
			return nil, errors.New("view origin must be an exact HTTPS origin")
		}
	}
	if config.Logf == nil {
		config.Logf = log.Printf
	}
	h := &Handler{config: config}
	h.nonces = &admission.ReplayCache{Now: h.now}
	h.tickets = admission.TicketVerifier{PublicKey: config.Signer.PrivateKey.Public().(ed25519.PublicKey), KeyID: config.Signer.KeyID, Audience: config.MainAddress, Now: h.now}
	return h, nil
}

func (h *Handler) now() time.Time {
	if h.config.Now != nil {
		return h.config.Now()
	}
	return time.Now()
}

func (h *Handler) secure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return h.config.TrustProxyTLS && r.Header.Get("X-Forwarded-Proto") == "https"
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.secure(r) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "HTTPS required"})
		return
	}
	if r.URL.Path == "/v1/view" && h.handleViewCORS(w, r) {
		return
	}
	switch r.URL.Path {
	case "/verify":
		h.handleVerify(w, r)
	case "/v1/challenge":
		h.handleChallenge(w, r)
	case "/v1/resolve":
		h.handleResolve(w, r)
	case "/v1/view":
		h.handleView(w, r)
	case "/v1/demo/resolve":
		h.handleDemoResolve(w, r)
	case "/v1/admin/status":
		h.handleAdminStatus(w, r)
	case "/v1/admin/release":
		h.handleAdminRelease(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// handleViewCORS handles only the browser preflight for the explicit viewing
// endpoint. Requests without an Origin header remain available to native clients.
func (h *Handler) handleViewCORS(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	sameOrigin := "https://" + r.Host
	if r.TLS == nil && h.config.TrustProxyTLS && r.Header.Get("X-Forwarded-Proto") == "https" {
		// Host is preserved by the supported TLS-terminating proxy setup.
		sameOrigin = "https://" + r.Host
	}
	if origin != sameOrigin && (h.config.ViewOrigin == "" || origin != h.config.ViewOrigin) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "origin not allowed"})
		return true
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	if r.Method == http.MethodOptions {
		if r.Header.Get("Access-Control-Request-Method") != http.MethodPost {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "origin not allowed"})
			return true
		}
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

func requirePOST(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return false
	}
	return true
}

// decodeBody reads one strict JSON object. A missing Content-Length is fine
// (proxies strip it); the body is bounded either way.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.ContentLength > MaxRequestBytes {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return false
	}
	body := http.MaxBytesReader(w, r.Body, MaxRequestBytes)
	defer body.Close()
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(dst) != nil || decoder.Decode(new(any)) != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return false
	}
	return true
}

func (h *Handler) handleChallenge(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	_, _ = io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, MaxRequestBytes))
	nonce := newNonce(h.config.ChallengeKey, h.now())
	writeJSON(w, http.StatusOK, struct {
		Nonce     string `json:"nonce"`
		ExpiresIn int    `json:"expires_in"`
	}{base64.RawURLEncoding.EncodeToString(nonce), int(ChallengeLifetime.Seconds())})
}

type verifyRequest struct {
	Proto     int    `json:"proto"`
	SID       string `json:"sid"`
	DevicePub string `json:"device_pub"`
	Nonce     string `json:"nonce"`
	Sig       string `json:"sig"`
}

func decodeFixed(text string, size int) ([]byte, bool) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(text)
	if err != nil || len(raw) != size || base64.RawURLEncoding.EncodeToString(raw) != text {
		return nil, false
	}
	return raw, true
}

func ticketRole(verified sid.Verified) string {
	if verified.Publisher() {
		return admission.RoleCamera
	}
	return admission.RolePlayer
}

func (h *Handler) reject(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "SID rejected"})
}

func (h *Handler) unavailable(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "verification unavailable"})
}

func (h *Handler) handleVerify(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var request verifyRequest
	if !decodeBody(w, r, &request) {
		return
	}
	devicePub, okPub := decodeFixed(request.DevicePub, ed25519.PublicKeySize)
	nonce, okNonce := decodeFixed(request.Nonce, activation.NonceSize)
	signature, okSig := decodeFixed(request.Sig, ed25519.SignatureSize)
	if request.Proto != 2 || !okPub || !okNonce || !okSig || strings.TrimSpace(request.SID) != request.SID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	verified, err := sid.Verify(request.SID, h.config.SIDPublicKey, h.config.MinIssued)
	if err != nil {
		h.reject(w)
		return
	}
	digest := hex.EncodeToString(verified.Digest[:])
	short := digest[:12]

	tenant, registered, err := h.config.Tenants.Lookup(verified.Tenant)
	if err != nil {
		h.unavailable(w)
		return
	}
	if !registered {
		h.config.Logf("center: rejected SID %s: unknown tenant %q", short, verified.Tenant)
		h.reject(w)
		return
	}
	blocked, err := h.config.Blocklist.Blocked(digest, verified.Tenant)
	if err != nil {
		h.unavailable(w)
		return
	}
	if blocked {
		h.reject(w)
		return
	}

	// The device proves it holds the key it presents, answering a challenge
	// this Center issued, bound to this SID and to the MainServer origin.
	expires, fresh := checkNonce(h.config.ChallengeKey, nonce, h.now())
	if !fresh {
		h.reject(w)
		return
	}
	message, err := activation.Message(nonce, request.SID, h.config.MainAddress)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(devicePub), message, signature) {
		h.reject(w)
		return
	}
	if !h.nonces.Use(request.Nonce, expires) {
		h.reject(w) // challenge already answered once
		return
	}

	deviceID := activation.DeviceID(ed25519.PublicKey(devicePub))
	role := ticketRole(verified)
	record, known := h.config.Activations.Get(digest)
	if !known || !record.Bound() {
		// First bind, or a bind after the pairing system released the SID. The
		// activation window applies to every bind, counted from issue time.
		window := tenant.ActivationWindow
		if window <= 0 {
			window = DefaultActivationWindow
		}
		if h.now().Sub(verified.IssuedAt) > window {
			h.config.Logf("center: rejected SID %s tenant=%s: not bound within %s of issue", short, verified.Tenant, window)
			h.reject(w)
			return
		}
		candidate := activation.Record{
			Digest: digest, Tenant: verified.Tenant, Role: role, DeviceID: deviceID,
			DevicePub: request.DevicePub, CameraRef: sid.CameraRef(request.SID),
			IssuedAt: verified.IssuedAt.Unix(), ActivatedAt: h.now().Unix(),
		}
		var result activation.BindResult
		record, result, err = h.config.Activations.Bind(candidate, h.now())
		if err != nil {
			h.config.Logf("center: activation store failure: %v", err)
			h.unavailable(w)
			return
		}
		switch result {
		case activation.BindCreated:
			h.config.Logf("center: activated SID %s tenant=%s role=%s device=%s", short, verified.Tenant, role, deviceID)
		case activation.BindRebound:
			h.config.Logf("center: re-assigned SID %s tenant=%s role=%s to device=%s", short, verified.Tenant, role, deviceID)
		case activation.BindFormerHolder:
			h.config.Logf("center: ALERT released SID %s tenant=%s refused for former holder %s", short, verified.Tenant, deviceID)
			h.reject(w)
			return
		case activation.BindTooSoon:
			h.config.Logf("center: SID %s tenant=%s was just released; device %s should retry", short, verified.Tenant, deviceID)
			h.unavailable(w)
			return
		}
	}
	if record.DevicePub != request.DevicePub {
		// A second key presented a SID whose seat is taken: either a leaked
		// SID or a device that lost its key. Surface it for operators.
		h.config.Logf("center: ALERT SID %s tenant=%s already bound to device %s; rejected key %s", short, verified.Tenant, record.DeviceID, deviceID)
		h.reject(w)
		return
	}

	signer := h.config.Signer
	signer.Now = h.now
	ticket, err := signer.Sign(deviceID, digest, verified.Tenant, role)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "verification unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, struct {
		MainServerAddress string `json:"main_server_address"`
		Ticket            string `json:"ticket"`
		ExpiresIn         int    `json:"expires_in"`
		DeviceID          string `json:"device_id"`
		Tenant            string `json:"tenant"`
		Role              string `json:"role"`
	}{h.config.MainAddress, ticket, int(signer.EffectiveLifetime().Seconds()), deviceID, verified.Tenant, role})
}

type resolveResponse struct {
	Found             bool   `json:"found"`
	DeviceID          string `json:"device_id,omitempty"`
	MainServerAddress string `json:"main_server_address,omitempty"`
}

// lookupCamera resolves a camera reference inside one tenant. Unknown,
// blocked or non-camera bindings all look the same: not found.
func (h *Handler) lookupCamera(tenant, ref string) (resolveResponse, error) {
	if len(ref) != 64 {
		return resolveResponse{}, nil
	}
	if _, err := hex.DecodeString(ref); err != nil {
		return resolveResponse{}, nil
	}
	record, ok := h.config.Activations.LookupRef(tenant, strings.ToLower(ref))
	if !ok || record.Role != admission.RoleCamera {
		return resolveResponse{}, nil
	}
	blocked, err := h.config.Blocklist.Blocked(record.Digest, record.Tenant)
	if err != nil {
		return resolveResponse{}, err
	}
	if blocked {
		return resolveResponse{}, nil
	}
	return resolveResponse{Found: true, DeviceID: record.DeviceID, MainServerAddress: h.config.MainAddress}, nil
}

// handleResolve lets an activated player find a camera of its own tenant from
// the camera's reference. The player authenticates with a ticket Center issued.
func (h *Handler) handleResolve(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	header := r.Header.Get("Authorization")
	token := strings.TrimPrefix(header, "Bearer ")
	if token == header || token == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "ticket required"})
		return
	}
	claims, err := h.tickets.VerifyAny(token)
	if err != nil || claims.Role != admission.RolePlayer {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "ticket required"})
		return
	}
	if blocked, err := h.config.Blocklist.Blocked(claims.Digest, claims.Tenant); err != nil {
		h.unavailable(w)
		return
	} else if blocked {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "ticket required"})
		return
	}
	var request struct {
		Ref string `json:"ref"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	result, err := h.lookupCamera(claims.Tenant, request.Ref)
	if err != nil {
		h.unavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type viewRequest struct {
	SID string `json:"sid"`
}

// handleView exchanges a camera SID for a short-lived, camera-scoped viewer
// ticket. The camera SID is never returned or logged by this path.
func (h *Handler) handleView(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if !requirePOST(w, r) {
		return
	}
	var request viewRequest
	if !decodeBody(w, r, &request) {
		return
	}
	if request.SID == "" || strings.TrimSpace(request.SID) != request.SID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	verified, err := sid.Verify(request.SID, h.config.SIDPublicKey, h.config.MinIssued)
	if err != nil || !verified.Publisher() {
		h.reject(w)
		return
	}
	tenant, ok, err := h.config.Tenants.Lookup(verified.Tenant)
	_ = tenant // The registry lookup, not tenant-specific settings, gates viewing.
	if err != nil {
		h.unavailable(w)
		return
	}
	if !ok {
		h.reject(w)
		return
	}
	digest := Digest(request.SID)
	blocked, err := h.config.Blocklist.Blocked(digest, verified.Tenant)
	if err != nil {
		h.unavailable(w)
		return
	}
	if blocked {
		h.reject(w)
		return
	}
	activation, active := h.config.Activations.Get(digest)
	if !active || !activation.Bound() || activation.Role != admission.RoleCamera || activation.Tenant != verified.Tenant {
		h.reject(w)
		return
	}
	blocked, err = h.config.Blocklist.Blocked(activation.Digest, activation.Tenant)
	if err != nil {
		h.unavailable(w)
		return
	}
	if blocked {
		h.reject(w)
		return
	}
	var idBytes [32]byte
	if _, err := io.ReadFull(rand.Reader, idBytes[:]); err != nil {
		h.unavailable(w)
		return
	}
	viewerID := base64.RawURLEncoding.EncodeToString(idBytes[:])
	signer := h.config.Signer
	signer.Lifetime = time.Minute
	signer.Now = h.now
	ticket, err := signer.SignCameraViewer(viewerID, digest, verified.Tenant, activation.DeviceID)
	if err != nil {
		h.unavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		MainServerAddress string `json:"main_server_address"`
		Ticket            string `json:"ticket"`
		ExpiresIn         int    `json:"expires_in"`
		DeviceID          string `json:"device_id"`
		CameraID          string `json:"camera_id"`
		Tenant            string `json:"tenant"`
		Role              string `json:"role"`
	}{h.config.MainAddress, ticket, 60, viewerID, activation.DeviceID, verified.Tenant, admission.RolePlayer})
}

// adminAuthorized checks the pairing system's bearer token in constant time.
// With no token configured the admin routes do not exist.
func (h *Handler) adminAuthorized(w http.ResponseWriter, r *http.Request) bool {
	if len(h.config.AdminToken) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return false
	}
	header := r.Header.Get("Authorization")
	token := strings.TrimPrefix(header, "Bearer ")
	want := sha256.Sum256(h.config.AdminToken)
	got := sha256.Sum256([]byte(token))
	if token == header || subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return false
	}
	return true
}

type adminRequest struct {
	SIDDigest string `json:"sid_digest"`
}

func (h *Handler) adminDigest(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !requirePOST(w, r) || !h.adminAuthorized(w, r) {
		return "", false
	}
	var request adminRequest
	if !decodeBody(w, r, &request) {
		return "", false
	}
	digest := strings.ToLower(request.SIDDigest)
	if raw, err := hex.DecodeString(digest); err != nil || len(raw) != sha256.Size {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return "", false
	}
	return digest, true
}

// handleAdminStatus tells the pairing system whether a SID may be handed out:
// "unbound" (never bound), "bound", or "released" with when it can bind again.
func (h *Handler) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	digest, ok := h.adminDigest(w, r)
	if !ok {
		return
	}
	record, known := h.config.Activations.Get(digest)
	if !known {
		writeJSON(w, http.StatusOK, map[string]any{"state": "unbound"})
		return
	}
	result := map[string]any{"state": record.State(), "tenant": record.Tenant, "role": record.Role}
	if !record.Bound() {
		result["bindable_at"] = time.Unix(record.ReleasedAt, 0).Add(activation.ReleaseGrace).Unix()
	}
	writeJSON(w, http.StatusOK, result)
}

// RevocationLifetime is how long a released device stays revoked on the nodes:
// a ticket's lifetime, the node sweep that drops live connections, and a margin.
func (h *Handler) revocationLifetime() time.Duration {
	signer := h.config.Signer
	return signer.EffectiveLifetime() + time.Minute
}

// handleAdminRelease ends the current device's seat on a SID so the pairing
// system can hand it to another device. The old device is revoked on the nodes
// first, and the release is written second, so no bind can overtake the
// revocation. Releasing a released or never-bound SID is not an error.
func (h *Handler) handleAdminRelease(w http.ResponseWriter, r *http.Request) {
	digest, ok := h.adminDigest(w, r)
	if !ok {
		return
	}
	now := h.now()
	record, known := h.config.Activations.Get(digest)
	if !known {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "SID was never bound"})
		return
	}
	if record.Bound() {
		if err := h.config.Blocklist.RevokeDevice(record.DeviceID, now.Add(h.revocationLifetime())); err != nil {
			h.config.Logf("center: cannot revoke device for release of SID %s: %v", digest[:12], err)
			h.unavailable(w)
			return
		}
	}
	previous, changed, err := h.config.Activations.Release(digest, now)
	if err != nil {
		h.config.Logf("center: release of SID %s failed: %v", digest[:12], err)
		h.unavailable(w)
		return
	}
	if changed {
		h.config.Logf("center: released SID %s tenant=%s from device %s", digest[:12], previous.Tenant, previous.DeviceID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"released": true, "changed": changed,
		"bindable_at": now.Add(activation.ReleaseGrace).Unix(),
	})
}

// handleDemoResolve is the unauthenticated lookup for the demo web player.
func (h *Handler) handleDemoResolve(w http.ResponseWriter, r *http.Request) {
	if h.config.DemoTenant == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, OPTIONS")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	result, err := h.lookupCamera(h.config.DemoTenant, r.URL.Query().Get("ref"))
	if err != nil {
		h.unavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// Digest returns the lowercase hex SHA-256 of a SID, as used in tickets.
func Digest(sidText string) string {
	sum := sha256.Sum256([]byte(sidText))
	return hex.EncodeToString(sum[:])
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		data = []byte(`{"error":"internal error"}`)
	}
	data = append(data, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
