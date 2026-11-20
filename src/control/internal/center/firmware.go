package center

import (
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"signaling-server/internal/firmware"
	"signaling-server/internal/sid"
)

// FirmwarePrefix starts the camera firmware route:
//
//	GET /v1/firmware/<PLATFORM>/matrix?sid=<camera SID>
//
// PLATFORM is the camera's build platform (anycloud, anykav200, anykav500): one
// binary per compiler, chosen by fepe_sdk at build time. It returns
// <FirmwareDir>/<PLATFORM>/matrix.pkg, the signed and encrypted
// package (MXPKG1, server/design/firmware-package.md), only to a camera whose
// SID is valid, of a registered tenant, a publisher and not blocked. The
// camera checks the signature and decrypts; this server holds neither key.
const FirmwarePrefix = "/v1/firmware/"

// firmwareWriteTimeout bounds one package download on a slow camera link.
const firmwareWriteTimeout = 10 * time.Minute

var firmwareModel = regexp.MustCompile(`^[A-Za-z0-9_]{1,16}$`)

// FirmwarePath reports whether path is a firmware route Center serves.
func FirmwarePath(path string) bool { return strings.HasPrefix(path, FirmwarePrefix) }

func (h *Handler) handleFirmware(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET required"})
		return
	}
	model, file, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, FirmwarePrefix), "/")
	if !ok || file != "matrix" || !firmwareModel.MatchString(model) || h.config.FirmwareDir == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	text := r.URL.Query().Get("sid")
	if text == "" || strings.TrimSpace(text) != text {
		h.reject(w)
		return
	}
	verified, err := sid.Verify(text, h.config.SIDPublicKey, h.config.MinIssued)
	if err != nil || !verified.Publisher() {
		h.reject(w)
		return
	}
	digest := hex.EncodeToString(verified.Digest[:])
	short := digest[:12]
	_, registered, err := h.config.Tenants.Lookup(verified.Tenant)
	if err != nil {
		h.unavailable(w)
		return
	}
	if !registered {
		h.config.Logf("center: firmware refused to SID %s: unknown tenant %q", short, verified.Tenant)
		h.reject(w)
		return
	}
	blocked, err := h.config.Blocklist.Blocked(digest, verified.Tenant)
	if err != nil {
		h.unavailable(w)
		return
	}
	if blocked {
		h.config.Logf("center: firmware refused to blocked SID %s", short)
		h.reject(w)
		return
	}
	// Channels (R7): the canary group gets the canary version, others stable.
	path, version, channel := h.config.Channels.Resolve(h.config.FirmwareDir, model, "matrix.pkg",
		firmware.Who{Addr: remoteAddr(r), Tenant: verified.Tenant, SIDDigest: digest})
	if version == "" {
		path = filepath.Join(h.config.FirmwareDir, model, "matrix.pkg")
		version, channel = "unversioned", "-"
	}
	f, err := os.Open(path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no firmware for this model"})
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no firmware for this model"})
		return
	}
	// The listener-wide write deadline would cut a slow camera link off.
	_ = http.NewResponseController(w).SetWriteDeadline(h.now().Add(firmwareWriteTimeout))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		h.config.Logf("center: firmware %s %s (%s, %d bytes) to SID %s from %s", model, version, channel, info.Size(), short, remoteAddr(r))
	}
	// ServeContent sets Content-Length, which the camera's downloader needs.
	http.ServeContent(w, r, "matrix", info.ModTime(), f)
}
