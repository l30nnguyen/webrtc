// Package stats counts viewing attempts and their outcomes per UTC day, from
// what the server sees and from the reports cameras and viewer apps send after
// each attempt. A node keeps its own counters; Center adds up its nodes'.
package stats

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// KeepDays is how many UTC days of counters are kept.
const KeepDays = 31

// Reporter roles.
const (
	RoleCamera = "camera"
	RolePlayer = "player"
)

// Outcomes a report may carry.
const (
	ResultConnected = "connected"
	ResultFailed    = "failed"
)

// Paths a connected session may use: same network, direct over the internet,
// or through the relay stream server.
var validPaths = map[string]bool{"lan": true, "direct": true, "relay": true}

// Error codes a failed report may carry. Unknown codes are counted as "other".
var validErrors = map[string]bool{
	"unauthorized":   true, // wrong camera password
	"camera_offline": true, // camera not connected to the control server
	"no_offer":       true, // camera never answered the request
	"no_answer":      true, // viewer never answered the camera
	"no_path":        true, // no network path, relay included
	"timeout":        true, // not connected in time
	"closed":         true, // closed by either side before connecting
	"other":          true,
}

// Report is one attempt's outcome as a camera or viewer app reports it.
type Report struct {
	Role   string
	Result string
	Path   string // connected only
	Error  string // failed only
	// RelayOffered is true when the relay stream server was configured for
	// the attempt; RelayReady is true when a relay address was obtained from it.
	RelayOffered bool
	RelayReady   bool
	SetupMillis  int // request to connected, connected only
}

// Normalize validates a report, mapping unknown values to safe ones. It returns
// false when the report is unusable (unknown role or result).
func (r *Report) Normalize() bool {
	if r.Role != RoleCamera && r.Role != RolePlayer {
		return false
	}
	switch r.Result {
	case ResultConnected:
		if !validPaths[r.Path] {
			r.Path = "unknown"
		}
		r.Error = ""
		if r.SetupMillis < 0 || r.SetupMillis > 600000 {
			r.SetupMillis = 0
		}
	case ResultFailed:
		if !validErrors[r.Error] {
			r.Error = "other"
		}
		r.Path, r.SetupMillis = "", 0
	default:
		return false
	}
	if !r.RelayOffered {
		r.RelayReady = false
	}
	return true
}

// Outcomes counts the reports of one role.
type Outcomes struct {
	Connected int            `json:"connected"`
	Failed    int            `json:"failed"`
	Paths     map[string]int `json:"paths,omitempty"`
	Errors    map[string]int `json:"errors,omitempty"`
	// RelayOffered counts reports where the relay stream server was configured,
	// RelayReady those where it handed out a relay address.
	RelayOffered int `json:"relay_offered"`
	RelayReady   int `json:"relay_ready"`
	// SetupMillisSum / SetupCount gives the mean time to connect.
	SetupMillisSum int64 `json:"setup_ms_sum"`
	SetupCount     int   `json:"setup_count"`
}

func (o *Outcomes) add(other Outcomes) {
	o.Connected += other.Connected
	o.Failed += other.Failed
	o.RelayOffered += other.RelayOffered
	o.RelayReady += other.RelayReady
	o.SetupMillisSum += other.SetupMillisSum
	o.SetupCount += other.SetupCount
	o.Paths = addMap(o.Paths, other.Paths)
	o.Errors = addMap(o.Errors, other.Errors)
}

func addMap(into, from map[string]int) map[string]int {
	if len(from) == 0 {
		return into
	}
	if into == nil {
		into = map[string]int{}
	}
	for k, v := range from {
		into[k] += v
	}
	return into
}

// Day holds one UTC day's counters.
type Day struct {
	// Requests counts viewer requests the server forwarded to a camera.
	Requests int `json:"requests"`
	// CameraOffline counts viewer requests for a camera that was not connected.
	CameraOffline int `json:"camera_offline"`
	// AdmissionRejected counts connections refused for a missing or bad ticket.
	AdmissionRejected int      `json:"admission_rejected"`
	Camera            Outcomes `json:"camera"`
	Player            Outcomes `json:"player"`
}

// Add adds other's counters into d.
func (d *Day) Add(other Day) {
	d.Requests += other.Requests
	d.CameraOffline += other.CameraOffline
	d.AdmissionRejected += other.AdmissionRejected
	d.Camera.add(other.Camera)
	d.Player.add(other.Player)
}

// Days maps a UTC date (2006-01-02) to its counters.
type Days map[string]Day

// Add adds every day of other into d.
func (d Days) Add(other Days) {
	for date, day := range other {
		sum := d[date]
		sum.Add(day)
		d[date] = sum
	}
}

// Counters is a node's thread-safe set of daily counters.
type Counters struct {
	mu   sync.Mutex
	days Days
	now  func() time.Time
}

// New returns empty counters; now may be nil for the wall clock.
func New(now func() time.Time) *Counters {
	if now == nil {
		now = time.Now
	}
	return &Counters{days: Days{}, now: now}
}

func (c *Counters) update(change func(*Day)) {
	date := c.now().UTC().Format(time.DateOnly)
	c.mu.Lock()
	day := c.days[date]
	change(&day)
	c.days[date] = day
	c.pruneLocked()
	c.mu.Unlock()
}

// Request counts a viewer request forwarded to a camera.
func (c *Counters) Request() { c.update(func(d *Day) { d.Requests++ }) }

// CameraOffline counts a viewer request for a camera that is not connected.
func (c *Counters) CameraOffline() { c.update(func(d *Day) { d.CameraOffline++ }) }

// AdmissionRejected counts a connection refused at the ticket check.
func (c *Counters) AdmissionRejected() { c.update(func(d *Day) { d.AdmissionRejected++ }) }

// Record counts a normalized report.
func (c *Counters) Record(r Report) {
	c.update(func(d *Day) {
		o := &d.Camera
		if r.Role == RolePlayer {
			o = &d.Player
		}
		if r.RelayOffered {
			o.RelayOffered++
		}
		if r.RelayReady {
			o.RelayReady++
		}
		if r.Result == ResultConnected {
			o.Connected++
			o.Paths = addMap(o.Paths, map[string]int{r.Path: 1})
			if r.SetupMillis > 0 {
				o.SetupMillisSum += int64(r.SetupMillis)
				o.SetupCount++
			}
			return
		}
		o.Failed++
		o.Errors = addMap(o.Errors, map[string]int{r.Error: 1})
	})
}

// Snapshot returns a copy of all kept days.
func (c *Counters) Snapshot() Days {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := Days{}
	out.Add(c.days)
	return out
}

func (c *Counters) pruneLocked() {
	if len(c.days) <= KeepDays {
		return
	}
	dates := make([]string, 0, len(c.days))
	for date := range c.days {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	for _, date := range dates[:len(dates)-KeepDays] {
		delete(c.days, date)
	}
}

// Load merges counters saved by Save; a missing file is not an error.
func (c *Counters) Load(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var days Days
	if err := json.Unmarshal(data, &days); err != nil {
		return err
	}
	c.mu.Lock()
	c.days.Add(days)
	c.pruneLocked()
	c.mu.Unlock()
	return nil
}

// Save writes all kept days atomically (mode 0600).
func (c *Counters) Save(path string) error {
	data, err := json.Marshal(c.Snapshot())
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".stats-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Range keeps the last n days (n <= 0 means all) and returns them with their
// sum, so callers can show totals and a per-day breakdown.
func Range(days Days, n int, now time.Time) (Days, Day) {
	out := Days{}
	var total Day
	oldest := ""
	if n > 0 {
		oldest = now.UTC().AddDate(0, 0, -(n - 1)).Format(time.DateOnly)
	}
	for date, day := range days {
		if date < oldest {
			continue
		}
		out[date] = day
		total.Add(day)
	}
	return out, total
}
