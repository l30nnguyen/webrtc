// Package fleet is Center's view of the control server nodes: which nodes are
// alive, how loaded they are, which cameras are online on each, and which node
// a camera should be sent to.
//
// Nodes report to Center every ReportInterval with the full list of cameras
// connected to them and their counters. A node that has not reported for
// StaleAfter is treated as down: no new cameras go to it and its cameras count
// as offline. The node that runs Center itself is read in-process.
package fleet

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"signaling-server/internal/admission"
	"signaling-server/internal/stats"
)

const (
	// ReportInterval is how often a node reports to Center.
	ReportInterval = 15 * time.Second
	// StaleAfter is how long Center trusts a node's last report.
	StaleAfter = 3*ReportInterval + 5*time.Second
	// pendingWindow is how long an assignment counts toward a node's load
	// before the camera shows up in that node's report.
	pendingWindow = 2 * ReportInterval
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
	// TokenSHA256 is the hex SHA-256 of the token the node reports with.
	// The node that runs Center has none.
	TokenSHA256 string `json:"token_sha256,omitempty"`
}

// Report is what a node sends to Center.
type Report struct {
	Address string     `json:"address"`
	Cameras []string   `json:"cameras"`
	Clients int        `json:"clients"`
	Stats   stats.Days `json:"stats,omitempty"`
}

// Local reads the node that runs Center.
type Local func() Report

type nodeState struct {
	Node
	local    bool
	lastSeen time.Time
	cameras  map[string]struct{}
	clients  int
	stats    stats.Days
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
	self     string
	local    Local
	now      func() time.Time
}

// ErrUnauthorized is returned for a report from an unknown node or with a
// wrong token.
var ErrUnauthorized = errors.New("unknown node or wrong token")

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
	for id, n := range r.location {
		if byAddr[n.Address] != n {
			delete(r.location, id)
		}
	}
	r.nodes, r.byAddr, r.routes, r.version = nodes, byAddr, config.Routes, config.Version
	return nil
}

// Version is the version of the active configuration.
func (r *Registry) Version() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.version
}

// Accept stores a node's report after checking its token.
func (r *Registry) Accept(token string, report Report) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.byAddr[report.Address]
	if n == nil || n.local {
		return ErrUnauthorized
	}
	want, _ := hex.DecodeString(n.TokenSHA256)
	got := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(want, got[:]) != 1 {
		return ErrUnauthorized
	}
	if len(report.Cameras) > MaxReportCameras {
		return errors.New("report lists too many cameras")
	}
	r.applyLocked(n, report)
	return nil
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
	return n.local || (!n.lastSeen.IsZero() && now.Sub(n.lastSeen) <= StaleAfter)
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

// NodeStatus is one node in the fleet overview.
type NodeStatus struct {
	Name     string     `json:"name,omitempty"`
	Address  string     `json:"address"`
	Region   string     `json:"region,omitempty"`
	Healthy  bool       `json:"healthy"`
	LastSeen string     `json:"last_seen,omitempty"`
	Cameras  int        `json:"cameras"`
	Clients  int        `json:"clients"`
	Capacity int        `json:"capacity"`
	Stats    stats.Days `json:"-"`
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
			Cameras: len(n.cameras), Clients: n.clients, Capacity: n.Capacity, Stats: stats.Days{}}
		if !n.lastSeen.IsZero() {
			status.LastSeen = n.lastSeen.UTC().Format(time.RFC3339)
		}
		status.Stats.Add(n.stats)
		out = append(out, status)
	}
	return out
}
