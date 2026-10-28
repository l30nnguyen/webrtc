// Package fleet is Center's view of the control server nodes: which nodes are
// alive, how loaded they are, which cameras are online on each, and which node
// a camera should be sent to.
//
// Each node keeps a link open to Center (see package link): after logging in
// with its key it sends the full list of its cameras, then every camera that
// comes or goes, and its counters every minute. A node without a link is down:
// no new cameras go to it and its cameras count as offline. The node that runs
// Center itself is read in-process.
package fleet

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"

	"signaling-server/internal/admission"
	"signaling-server/internal/stats"
)

const (
	// pendingWindow is how long an assignment counts toward a node's load
	// before the camera shows up on that node.
	pendingWindow = 30 * time.Second
	// DefaultCapacity is the camera count a node is filled to when its entry
	// names none (the planning density in the design document).
	DefaultCapacity = 60000
	// MaxReportCameras bounds one report.
	MaxReportCameras = 200000
)

// Node is one entry of the configuration's node list.
type Node struct {
	// Name is an optional short label (for example "eu00").
	Name string `json:"name,omitempty"`
	// Address is the node's exact WSS root, also its ticket audience.
	Address string `json:"address"`
	Region  string `json:"region"`
	// Countries lists ISO 3166 codes whose cameras prefer this node, unless
	// the configuration's routes say otherwise.
	Countries []string `json:"countries,omitempty"`
	Capacity  int      `json:"capacity,omitempty"`
	// PublicKey is the node's link key (raw Ed25519, base64url). The node
	// that runs Center has none; a node without one cannot connect.
	PublicKey string `json:"public_key,omitempty"`
	// LinkFrom lists the IP addresses and CIDRs the node's link may come
	// from. Empty means the addresses its host name resolves to.
	LinkFrom []string `json:"link_from,omitempty"`
	// TokenSHA256 belonged to the phase 2 HTTP report. It is accepted so old
	// files still load, and ignored.
	TokenSHA256 string `json:"token_sha256,omitempty"`
}

// LinkKey decodes the node's public key; ok is false when it has none.
func (n Node) LinkKey() (key ed25519.PublicKey, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(n.PublicKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, false
	}
	return ed25519.PublicKey(raw), true
}

// Report is what a node sends to Center.
type Report struct {
	Address string     `json:"address"`
	Cameras []string   `json:"cameras"`
	Clients int        `json:"clients"`
	Stats   stats.Days `json:"stats,omitempty"`
}

// RelayStatus is one relay's last measurement by a node of its region.
type RelayStatus struct {
	Name         string  `json:"name"`
	Region       string  `json:"region"`
	Healthy      bool    `json:"healthy"`
	Mbps         float64 `json:"mbps"`
	Measured     bool    `json:"measured"` // Mbps comes from metrics
	Allocations  int     `json:"allocations"`
	CapacityMbps int     `json:"capacity_mbps"`
	At           string  `json:"at"` // RFC 3339, UTC
	Error        string  `json:"error,omitempty"`
	MeasuredBy   string  `json:"measured_by,omitempty"`
}

// MaxRelayStatuses bounds the relay measurements one node reports.
const MaxRelayStatuses = 256

// Local reads the node that runs Center.
type Local func() Report

type nodeState struct {
	Node
	local    bool
	link     uint64 // generation of the current link, 0 when none
	acked    int    // configuration version the node runs
	lastSeen time.Time
	cameras  map[string]struct{}
	clients  int
	stats    stats.Days
	relays   []RelayStatus
	pending  []time.Time
}

// Registry is Center's fleet state. It is safe for concurrent use.
type Registry struct {
	mu       sync.Mutex
	nodes    []*nodeState
	byAddr   map[string]*nodeState
	location map[string]*nodeState // camera device ID -> node it is online on
	country  map[string]string     // camera device ID -> country of its last activation
	routes   map[string]string
	version  int
	config   Config // the active configuration, as applied
	links    uint64 // last link generation handed out
	self     string
	local    Local
	now      func() time.Time
}

// ErrUnknownNode is returned for a link from an address that is not a node
// of the configuration (or is the Center's own node).
var ErrUnknownNode = errors.New("not a node of this fleet")

// Load reads a configuration file. selfAddress is the node running Center; it
// is added with default settings when the file does not list it.
func Load(path, selfAddress string, local Local, now func() time.Time) (*Registry, error) {
	config, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	return NewFromConfig(config, selfAddress, local, now)
}

// New builds a registry from node entries.
func New(nodes []Node, selfAddress string, local Local, now func() time.Time) (*Registry, error) {
	return NewFromConfig(Config{Nodes: nodes}, selfAddress, local, now)
}

// NewFromConfig builds a registry from a configuration.
func NewFromConfig(config Config, selfAddress string, local Local, now func() time.Time) (*Registry, error) {
	if now == nil {
		now = time.Now
	}
	r := &Registry{byAddr: map[string]*nodeState{}, location: map[string]*nodeState{}, country: map[string]string{},
		self: selfAddress, local: local, now: now}
	if err := r.Apply(config); err != nil {
		return nil, err
	}
	return r, nil
}

// Apply replaces the configuration. Nodes that stay keep their reported
// state (cameras, last report); cameras already connected are not moved, the
// new routes apply to their next connection. An invalid configuration
// changes nothing.
func (r *Registry) Apply(config Config) error {
	if err := config.Validate(r.self); err != nil {
		return err
	}
	config.Normalize()
	r.mu.Lock()
	defer r.mu.Unlock()
	nodes := make([]*nodeState, 0, len(config.Nodes)+1)
	byAddr := make(map[string]*nodeState, len(config.Nodes)+1)
	hasSelf := false
	for _, n := range config.Nodes {
		self := n.Address == r.self
		hasSelf = hasSelf || self
		state := r.byAddr[n.Address]
		if state == nil {
			state = &nodeState{cameras: map[string]struct{}{}}
		} else if state.PublicKey != n.PublicKey {
			r.unlinkLocked(state) // a new key must log in again
		}
		state.Node, state.local = n, self
		nodes = append(nodes, state)
		byAddr[n.Address] = state
	}
	if !hasSelf {
		state := r.byAddr[r.self]
		if state == nil {
			state = &nodeState{cameras: map[string]struct{}{}}
		}
		state.Node, state.local = Node{Address: r.self, Capacity: DefaultCapacity}, true
		nodes = append([]*nodeState{state}, nodes...)
		byAddr[r.self] = state
	}
	for _, n := range r.nodes {
		if byAddr[n.Address] != n {
			r.unlinkLocked(n)
		}
	}
	r.nodes, r.byAddr, r.routes, r.version, r.config = nodes, byAddr, config.Routes, config.Version, config
	return nil
}

// Config returns the active configuration.
func (r *Registry) Config() Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.config
}

// Version is the version of the active configuration.
func (r *Registry) Version() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.version
}

// LinkKey returns the key the node at address logs in with.
func (r *Registry) LinkKey(address string) (ed25519.PublicKey, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.byAddr[address]
	if n == nil || n.local {
		return nil, false
	}
	return n.LinkKey()
}

// Linked records a node that logged in, with its full state. It returns the
// link's generation, which the other link calls carry: a call from a link
// that has since been replaced or dropped is ignored.
func (r *Registry) Linked(report Report, configVersion int) (uint64, error) {
	if len(report.Cameras) > MaxReportCameras {
		return 0, errors.New("too many cameras")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.byAddr[report.Address]
	if n == nil || n.local {
		return 0, ErrUnknownNode
	}
	r.links++
	n.link = r.links
	n.acked = configVersion
	r.applyLocked(n, report)
	return n.link, nil
}

// Current reports whether link is still the node's live link.
func (r *Registry) Current(address string, link uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.byAddr[address]
	return n != nil && link != 0 && n.link == link
}

// Camera records a camera coming online on, or leaving, a linked node.
func (r *Registry) Camera(address string, link uint64, device string, online bool) {
	if !admission.ValidDeviceID(device) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.byAddr[address]
	if n == nil || n.link != link || link == 0 {
		return
	}
	n.lastSeen = r.now()
	if online {
		if len(n.cameras) >= MaxReportCameras {
			return
		}
		n.cameras[device] = struct{}{}
		r.location[device] = n
		return
	}
	delete(n.cameras, device)
	if r.location[device] == n {
		delete(r.location, device)
	}
}

// NodeStats records a linked node's client count, counters and relay
// measurements.
func (r *Registry) NodeStats(address string, link uint64, clients int, days stats.Days, relays []RelayStatus) {
	if len(relays) > MaxRelayStatuses {
		relays = relays[:MaxRelayStatuses]
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if n := r.byAddr[address]; n != nil && n.link == link && link != 0 {
		n.clients, n.stats, n.relays, n.lastSeen = clients, days, relays, r.now()
	}
}

// Relays merges the relay measurements of the linked nodes with local (the
// Center node's own), keeping the latest one per relay listed in the active
// configuration. Relays nobody measured are listed without a measurement.
func (r *Registry) Relays(local []RelayStatus) []RelayStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	latest := map[string]RelayStatus{}
	consider := func(status RelayStatus, by string) {
		if status.MeasuredBy == "" {
			status.MeasuredBy = by
		}
		if old, ok := latest[status.Name]; !ok || status.At > old.At {
			latest[status.Name] = status
		}
	}
	for _, status := range local {
		consider(status, r.self)
	}
	for _, n := range r.nodes {
		if n.link != 0 {
			for _, status := range n.relays {
				consider(status, n.Address)
			}
		}
	}
	out := make([]RelayStatus, 0, len(r.config.Relays))
	for _, relay := range r.config.Relays {
		status, ok := latest[relay.Name]
		if !ok {
			status = RelayStatus{Name: relay.Name, Region: relay.Region, CapacityMbps: relay.CapacityMbps}
		}
		out = append(out, status)
	}
	return out
}

// ConfigAck records the configuration version a linked node runs.
func (r *Registry) ConfigAck(address string, link uint64, version int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n := r.byAddr[address]; n != nil && n.link == link && link != 0 {
		n.acked = version
	}
}

// Unlinked records that a node's link ended: the node is down and its
// cameras count as offline.
func (r *Registry) Unlinked(address string, link uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n := r.byAddr[address]; n != nil && n.link == link && link != 0 {
		r.unlinkLocked(n)
	}
}

func (r *Registry) unlinkLocked(n *nodeState) {
	n.link = 0
	for id := range n.cameras {
		if r.location[id] == n {
			delete(r.location, id)
		}
	}
	n.cameras = map[string]struct{}{}
}

func (r *Registry) applyLocked(n *nodeState, report Report) {
	for id := range n.cameras {
		if r.location[id] == n {
			delete(r.location, id)
		}
	}
	n.cameras = make(map[string]struct{}, len(report.Cameras))
	for _, id := range report.Cameras {
		if admission.ValidDeviceID(id) {
			n.cameras[id] = struct{}{}
			r.location[id] = n
		}
	}
	n.clients = report.Clients
	n.stats = report.Stats
	n.lastSeen = r.now()
}

// refreshLocal reads the Center node in-process.
func (r *Registry) refreshLocalLocked() {
	if r.local == nil {
		return
	}
	for _, n := range r.nodes {
		if n.local {
			r.applyLocked(n, r.local())
			return
		}
	}
}

func (r *Registry) healthyLocked(n *nodeState, now time.Time) bool {
	return n.local || n.link != 0
}

// loadLocked is the node's fill ratio, counting recent assignments that have
// not shown up in its report yet.
func (r *Registry) loadLocked(n *nodeState, now time.Time) float64 {
	kept := n.pending[:0]
	for _, at := range n.pending {
		if now.Sub(at) < pendingWindow {
			kept = append(kept, at)
		}
	}
	n.pending = kept
	return float64(len(n.cameras)+len(n.pending)) / float64(n.Capacity)
}

// Assign picks the node for a camera: its current node when that is healthy,
// not full and in the camera's region; otherwise the least loaded healthy node
// of the camera's region, or of the whole fleet. The region comes from the
// routes for the country, else from the nodes' own country lists, else from
// the "*" route. device is the camera's unauthenticated hint (may be empty),
// country its ISO code (may be empty). It returns "" only when every node is
// down or full.
func (r *Registry) Assign(device, country string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.refreshLocalLocked()
	country = strings.ToUpper(country)
	var candidates []*nodeState
	for _, n := range r.nodes {
		if r.healthyLocked(n, now) && r.loadLocked(n, now) < 1 {
			candidates = append(candidates, n)
		}
	}
	if regional := r.regionalLocked(candidates, country); len(regional) > 0 {
		candidates = regional
	}
	if len(candidates) == 0 {
		return ""
	}
	if current := r.location[device]; current != nil {
		for _, n := range candidates {
			if n == current {
				n.pending = append(n.pending, now)
				return n.Address
			}
		}
	}
	best := candidates[0]
	for _, n := range candidates[1:] {
		if r.loadLocked(n, now) < r.loadLocked(best, now) {
			best = n
		}
	}
	best.pending = append(best.pending, now)
	return best.Address
}

// regionalLocked keeps the candidates of the camera's region.
func (r *Registry) regionalLocked(candidates []*nodeState, country string) []*nodeState {
	var regional []*nodeState
	for _, n := range candidates {
		if inRegion(r.routes, candidates, n.Node, country) {
			regional = append(regional, n)
		}
	}
	return regional
}

// inRegion reports whether node n is in the region of a camera from country,
// among the given nodes: by the routes for the country, else by the nodes'
// own country lists, else by the "*" route.
func inRegion(routes map[string]string, nodes []*nodeState, n Node, country string) bool {
	if region, ok := routes[country]; ok && country != "" {
		return n.Region == region
	}
	if country != "" {
		for _, other := range nodes {
			if contains(other.Countries, country) {
				return contains(n.Countries, country)
			}
		}
	}
	if region, ok := routes["*"]; ok {
		return n.Region == region
	}
	return false
}

// Placed records the country a camera activated from, so that Moves can
// tell which connected cameras a new configuration would place elsewhere.
// It is called only after the camera proved its key.
func (r *Registry) Placed(device, country string) {
	if !admission.ValidDeviceID(device) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.country[device] = strings.ToUpper(country)
}

// Moves counts the cameras online now that config would place on another
// node at their next connection: their node leaves the fleet, or it is not
// in the region config gives their country. Cameras whose country is not
// known (connected before Center started) are counted in unknown.
func (r *Registry) Moves(config Config) (moved, online, unknown int, err error) {
	if err := config.Validate(r.self); err != nil {
		return 0, 0, 0, err
	}
	config.Normalize()
	nodes := make([]*nodeState, 0, len(config.Nodes))
	byAddr := map[string]Node{}
	for _, n := range config.Nodes {
		nodes = append(nodes, &nodeState{Node: n})
		byAddr[n.Address] = n
	}
	if _, ok := byAddr[r.self]; !ok {
		byAddr[r.self] = Node{Address: r.self}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.refreshLocalLocked()
	for device, current := range r.location {
		if !r.healthyLocked(current, now) {
			continue
		}
		online++
		next, kept := byAddr[current.Address]
		if !kept {
			moved++
			continue
		}
		country, known := r.country[device]
		if !known {
			unknown++
			continue
		}
		// A camera with no region (no route matches) may stay anywhere.
		hasRegion := false
		for _, n := range byAddr {
			if inRegion(config.Routes, nodes, n, country) {
				hasRegion = true
				break
			}
		}
		if hasRegion && !inRegion(config.Routes, nodes, next, country) {
			moved++
		}
	}
	return moved, online, unknown, nil
}

func contains(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}

// Locate returns the node a camera is online on, if a healthy node reports it.
func (r *Registry) Locate(device string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocalLocked()
	n := r.location[device]
	if n == nil || !r.healthyLocked(n, r.now()) {
		return "", false
	}
	return n.Address, true
}

// Known reports whether address is a node of this fleet.
func (r *Registry) Known(address string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byAddr[address] != nil
}

// NodeOrigin reports whether origin (a browser Origin header) is the HTTPS
// origin of a node of this fleet, so a page served by that node may call
// Center: wss://host:443/ is https://host, wss://host:8443/ is
// https://host:8443.
func (r *Registry) NodeOrigin(origin string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range r.nodes {
		host := strings.TrimSuffix(strings.TrimPrefix(n.Address, "wss://"), "/")
		if strings.HasSuffix(host, ":443") {
			host = strings.TrimSuffix(host, ":443")
		}
		if origin == "https://"+host {
			return true
		}
	}
	return false
}

// NodeStatus is one node in the fleet overview.
type NodeStatus struct {
	Name     string `json:"name,omitempty"`
	Address  string `json:"address"`
	Region   string `json:"region,omitempty"`
	Healthy  bool   `json:"healthy"`
	LastSeen string `json:"last_seen,omitempty"`
	Cameras  int    `json:"cameras"`
	Clients  int    `json:"clients"`
	Capacity int    `json:"capacity"`
	// ConfigVersion is the configuration version the node runs (the
	// Center's own node always runs the active one).
	ConfigVersion int        `json:"config_version"`
	Stats         stats.Days `json:"-"`
}

// Status returns every node with its state and counters.
func (r *Registry) Status() []NodeStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.refreshLocalLocked()
	out := make([]NodeStatus, 0, len(r.nodes))
	for _, n := range r.nodes {
		status := NodeStatus{Name: n.Name, Address: n.Address, Region: n.Region, Healthy: r.healthyLocked(n, now),
			Cameras: len(n.cameras), Clients: n.clients, Capacity: n.Capacity, ConfigVersion: n.acked, Stats: stats.Days{}}
		if n.local {
			status.ConfigVersion = r.version
		}
		if !n.lastSeen.IsZero() {
			status.LastSeen = n.lastSeen.UTC().Format(time.RFC3339)
		}
		status.Stats.Add(n.stats)
		out = append(out, status)
	}
	return out
}
