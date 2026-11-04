// Package link is the protocol between a control node and Center: one
// WebSocket per node, opened by the node to Center at Path, on the public
// port (or a private link port).
//
//  0. The node's upgrade request carries HeaderNode (its address), HeaderTime
//     (Unix seconds), HeaderNonce (NonceSize random bytes, base64url) and
//     HeaderSig: its signature over HeaderMessage(time, nonce, center,
//     address). Center answers 404, as for any unknown path, unless the
//     request comes from one of that node's IP addresses (config.json
//     link_from, or what its host name resolves to), the time is within
//     HeaderWindow and the nonce is new. Nothing is upgraded before that.
//  1. Center sends {"type":"challenge","nonce","sig"}: sig is Center's
//     signature over CenterMessage(node nonce, nonce, center, address), so the
//     node knows it reached the real Center. The node answers within
//     LoginTimeout with {"type":"login","address","sig"} (at most
//     MaxLoginBytes): an Ed25519 signature with the node's own key over
//     LoginMessage(nonce, center, address), where center is the host name the
//     node verified Center's certificate for. Center checks it against the
//     node's public key in config.json.
//  2. The node sends {"type":"hello"}: its full camera list, client count,
//     counters, its measurements of its region's relays, the configuration
//     version it runs, its server version and its machine's load. Then {"type":"camera"} as cameras come and go, and
//     {"type":"stats"} (counters, relays and machine load) every StatsInterval.
//     Center answers {"type":"welcome"} once it has taken the login and the
//     list; a refused node is just disconnected.
//  3. Center pushes {"type":"config"}: the configuration signed with the
//     Center ticket key over ConfigMessage(version, config). The node accepts
//     only a valid signature and a version newer than its own, then answers
//     {"type":"ack"} or {"type":"config_error"}.
//
// Center pings every PingInterval; either side drops a link silent for
// ReadTimeout.
package link

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strconv"
	"time"

	"signaling-server/internal/fleet"
	"signaling-server/internal/host"
	"signaling-server/internal/stats"
)

const (
	// Path is the link's HTTP path.
	Path = "/v1/link"
	// The upgrade request's headers (step 0).
	HeaderNode  = "Matrix-Link-Node"
	HeaderTime  = "Matrix-Link-Time"
	HeaderNonce = "Matrix-Link-Nonce"
	HeaderSig   = "Matrix-Link-Sig"
	// HeaderWindow is how far HeaderTime may be from Center's clock.
	HeaderWindow = 60 * time.Second
	// NonceSize is the size of Center's login challenge.
	NonceSize = 32
	// LoginTimeout is how long Center waits for the login.
	LoginTimeout = 5 * time.Second
	// MaxLoginBytes bounds every message before login.
	MaxLoginBytes = 1024
	// MaxMessageBytes bounds a message after login (a full camera list).
	MaxMessageBytes = 16 << 20
	// PingInterval is how often Center pings a node.
	PingInterval = 10 * time.Second
	// ReadTimeout drops a link that has been silent this long.
	ReadTimeout = 3*PingInterval + 5*time.Second
	// StatsInterval is how often a node sends its counters.
	StatsInterval = 60 * time.Second
	// MaxPendingLogins bounds connections that have not logged in yet.
	MaxPendingLogins = 32

	loginContext  = "matrix-node-link-v1"
	headerContext = "matrix-node-upgrade-v1"
	centerContext = "matrix-center-link-v1"
	configContext = "matrix-config-v1"
)

// Message is every link message; Type says which fields are set.
type Message struct {
	Type string `json:"type"`

	// challenge
	Nonce string `json:"nonce,omitempty"`
	// login (Address also names the node in hello)
	Address string `json:"address,omitempty"`
	Sig     string `json:"sig,omitempty"`
	// hello
	Cameras []string `json:"cameras,omitempty"`
	// hello, stats
	Clients int                 `json:"clients,omitempty"`
	Stats   stats.Days          `json:"stats,omitempty"`
	Relays  []fleet.RelayStatus `json:"relays,omitempty"`
	// The node's machine and server version (since 2.6.0).
	Machine       *host.Load `json:"machine,omitempty"`
	ServerVersion string     `json:"server_version,omitempty"`
	// hello, config, ack, config_error
	Version int `json:"version,omitempty"`
	// camera
	ID     string `json:"id,omitempty"`
	Online bool   `json:"online,omitempty"`
	// config: base64url of the exact configuration bytes that were signed
	Config string `json:"config,omitempty"`
	// config_error
	Error string `json:"error,omitempty"`
}

// LoginMessage is what a node signs to log in.
func LoginMessage(nonce []byte, center, address string) []byte {
	message := make([]byte, 0, len(loginContext)+len(nonce)+len(center)+len(address)+3)
	message = append(message, loginContext...)
	message = append(message, 0)
	message = append(message, nonce...)
	message = append(message, center...)
	message = append(message, 0)
	message = append(message, address...)
	return message
}

// HeaderMessage is what a node signs in its upgrade request.
func HeaderMessage(unix int64, nonce []byte, center, address string) []byte {
	message := append([]byte(headerContext), 0)
	message = strconv.AppendInt(message, unix, 10)
	message = append(message, 0)
	message = append(message, nonce...)
	message = append(message, center...)
	message = append(message, 0)
	message = append(message, address...)
	return message
}

// CenterMessage is what Center signs in its challenge: the node's upgrade
// nonce and Center's own.
func CenterMessage(nodeNonce, nonce []byte, center, address string) []byte {
	message := append([]byte(centerContext), 0)
	message = append(message, nodeNonce...)
	message = append(message, nonce...)
	message = append(message, center...)
	message = append(message, 0)
	message = append(message, address...)
	return message
}

// ConfigMessage is what Center signs to push a configuration.
func ConfigMessage(version int, config []byte) []byte {
	message := make([]byte, 0, len(configContext)+len(config)+24)
	message = append(message, configContext...)
	message = append(message, 0)
	message = strconv.AppendInt(message, int64(version), 10)
	message = append(message, 0)
	message = append(message, config...)
	return message
}

// SignConfig builds the config message for version.
func SignConfig(key ed25519.PrivateKey, version int, config []byte) Message {
	sig := ed25519.Sign(key, ConfigMessage(version, config))
	return Message{Type: "config", Version: version,
		Config: base64.RawURLEncoding.EncodeToString(config), Sig: base64.RawURLEncoding.EncodeToString(sig)}
}

// VerifyConfig checks a config message's signature and returns the signed
// configuration bytes.
func VerifyConfig(key ed25519.PublicKey, m Message) ([]byte, error) {
	config, err := base64.RawURLEncoding.DecodeString(m.Config)
	if err != nil {
		return nil, errors.New("config is not base64url")
	}
	sig, err := base64.RawURLEncoding.DecodeString(m.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, errors.New("bad signature encoding")
	}
	if m.Version < 1 || !ed25519.Verify(key, ConfigMessage(m.Version, config), sig) {
		return nil, errors.New("signature does not verify")
	}
	return config, nil
}
