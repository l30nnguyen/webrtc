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
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

// Node is one entry of the nodes file.
type Node struct {
	// Address is the node's exact WSS root, also its ticket audience.
	Address string `json:"address"`
	Region  string `json:"region"`
	// Countries lists ISO 3166 codes whose cameras prefer this node.
	Countries []string `json:"countries,omitempty"`
	Capacity  int      `json:"capacity,omitempty"`
	// TokenSHA256 is the hex SHA-256 of the token the node reports with.
	// The node that runs Center has none.
	TokenSHA256 string `json:"token_sha256,omitempty"`
}

type file struct {
	Nodes []Node `json:"nodes"`
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
	local    Local
	now      func() time.Time
}

// ErrUnauthorized is returned for a report from an unknown node or with a
// wrong token.
var ErrUnauthorized = errors.New("unknown node or wrong token")

// Load reads the nodes file. selfAddress is the node running Center; it is
// added with default settings when the file does not list it.
func Load(path, selfAddress string, local Local, now func() time.Time) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f file
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&f); err != nil {
		return nil, fmt.Errorf("nodes file: %w", err)
	}
	return New(f.Nodes, selfAddress, local, now)
}

// New builds a registry from node entries.
func New(nodes []Node, selfAddress string, local Local, now func() time.Time) (*Registry, error) {
	if now == nil {
		now = time.Now
	}
	r := &Registry{byAddr: map[string]*nodeState{}, location: map[string]*nodeState{}, local: local, now: now}
	hasSelf := false
	for _, n := range nodes {
		if admission.ValidateAudience(n.Address) != nil {
			return nil, fmt.Errorf("node address %q must be an exact wss://host[:port]/ URL", n.Address)
		}
		if r.byAddr[n.Address] != nil {
			return nil, fmt.Errorf("node %s is listed twice", n.Address)
		}
		if n.Capacity < 0 {
			return nil, fmt.Errorf("node %s has a negative capacity", n.Address)
		}
		if n.Capacity == 0 {
			n.Capacity = DefaultCapacity
		}
		for i, c := range n.Countries {
			n.Countries[i] = strings.ToUpper(c)
		}
		self := n.Address == selfAddress
		if self {
			hasSelf = true
		} else if raw, err := hex.DecodeString(n.TokenSHA256); err != nil || len(raw) != sha256.Size {
			return nil, fmt.Errorf("node %s needs token_sha256 (64 hex characters)", n.Address)
		}
		state := &nodeState{Node: n, local: self, cameras: map[string]struct{}{}}
		r.nodes = append(r.nodes, state)
		r.byAddr[n.Address] = state
	}
	if !hasSelf {
		state := &nodeState{Node: Node{Address: selfAddress, Capacity: DefaultCapacity}, local: true, cameras: map[string]struct{}{}}
		r.nodes = append([]*nodeState{state}, r.nodes...)
		r.byAddr[selfAddress] = state
	}
	return r, nil
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
// of the camera's region (by country), or of the whole fleet. device is the
// camera's unauthenticated hint (may be empty), country its ISO code (may be
// empty). It returns "" only when every node is down or full.
func (r *Registry) Assign(device, country string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.refreshLocalLocked()
	country = strings.ToUpper(country)
	var candidates, regional []*nodeState
	for _, n := range r.nodes {
		if !r.healthyLocked(n, now) || r.loadLocked(n, now) >= 1 {
			continue
		}
		candidates = append(candidates, n)
		if country != "" && contains(n.Countries, country) {
			regional = append(regional, n)
		}
	}
	if len(regional) > 0 {
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
		status := NodeStatus{Address: n.Address, Region: n.Region, Healthy: r.healthyLocked(n, now),
			Cameras: len(n.cameras), Clients: n.clients, Capacity: n.Capacity, Stats: stats.Days{}}
		if !n.lastSeen.IsZero() {
			status.LastSeen = n.lastSeen.UTC().Format(time.RFC3339)
		}
		status.Stats.Add(n.stats)
		out = append(out, status)
	}
	return out
}
