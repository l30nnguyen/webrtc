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
	// Country is the reporter's country from its ticket ("" when unknown):
	// the viewer's for a viewer report, the camera's for a camera report.
	Country string
}

// UnknownCountry is the Countries key for clients whose country is not known
// (no country in their ticket, or a ticket from a Center before 2.7.0).
const UnknownCountry = "--"

// CountryKey is the Countries key for cc: cc itself when it is a two-letter
// upper-case code, UnknownCountry otherwise. It keeps the map small.
func CountryKey(cc string) string {
	if len(cc) == 2 && cc[0] >= 'A' && cc[0] <= 'Z' && cc[1] >= 'A' && cc[1] <= 'Z' {
		return cc
	}
	return UnknownCountry
}

// CountryCounts are one country's counters for a day (ST-1). Requests and
// viewer reports count by the viewer's country, camera reports by the
// camera's.
type CountryCounts struct {
	Requests        int `json:"requests,omitempty"`
	ViewerConnected int `json:"viewer_connected,omitempty"`
	ViewerFailed    int `json:"viewer_failed,omitempty"`
	CameraConnected int `json:"camera_connected,omitempty"`
	CameraFailed    int `json:"camera_failed,omitempty"`
	// QualitySessions counts end-of-session quality reports, PoorQuality
	// those with Poor quality (by the viewer's country when it is known).
	QualitySessions int `json:"quality_sessions,omitempty"`
	PoorQuality     int `json:"poor_quality,omitempty"`
}

func (c *CountryCounts) add(o CountryCounts) {
	c.Requests += o.Requests
	c.ViewerConnected += o.ViewerConnected
	c.ViewerFailed += o.ViewerFailed
	c.CameraConnected += o.CameraConnected
	c.CameraFailed += o.CameraFailed
	c.QualitySessions += o.QualitySessions
	c.PoorQuality += o.PoorQuality
}

func (d *Day) country(cc string, change func(*CountryCounts)) {
	if d.Countries == nil {
		d.Countries = map[string]CountryCounts{}
	}
	key := CountryKey(cc)
	counts := d.Countries[key]
	change(&counts)
	d.Countries[key] = counts
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
	AdmissionRejected int `json:"admission_rejected"`
	// RateLimited counts requests refused because their source sent too many.
	RateLimited int      `json:"rate_limited,omitempty"`
	Camera      Outcomes `json:"camera"`
	Player      Outcomes `json:"player"`
	// Countries splits requests and outcomes by country (ST-1).
	Countries map[string]CountryCounts `json:"countries,omitempty"`
	// Quality sums the cameras' end-of-session stream quality reports (ST-3).
	Quality Quality `json:"quality"`
}

// Add adds other's counters into d.
func (d *Day) Add(other Day) {
	d.Requests += other.Requests
	d.CameraOffline += other.CameraOffline
	d.AdmissionRejected += other.AdmissionRejected
	d.RateLimited += other.RateLimited
	d.Camera.add(other.Camera)
	d.Player.add(other.Player)
	d.Quality.add(other.Quality)
	for cc, counts := range other.Countries {
		d.country(cc, func(c *CountryCounts) { c.add(counts) })
	}
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
func (c *Counters) Request() { c.RequestFrom("") }

// RequestFrom counts a viewer request from a viewer in country cc ("" when
// unknown).
func (c *Counters) RequestFrom(cc string) {
	c.update(func(d *Day) {
		d.Requests++
		d.country(cc, func(cc *CountryCounts) { cc.Requests++ })
	})
}

// CameraOffline counts a viewer request for a camera that is not connected.
func (c *Counters) CameraOffline() { c.update(func(d *Day) { d.CameraOffline++ }) }

// AdmissionRejected counts a connection refused at the ticket check.
func (c *Counters) AdmissionRejected() { c.update(func(d *Day) { d.AdmissionRejected++ }) }

// RateLimited counts a request refused by a rate limit.
func (c *Counters) RateLimited() { c.update(func(d *Day) { d.RateLimited++ }) }

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
		d.country(r.Country, func(cc *CountryCounts) {
			switch {
			case r.Role == RolePlayer && r.Result == ResultConnected:
				cc.ViewerConnected++
			case r.Role == RolePlayer:
				cc.ViewerFailed++
			case r.Result == ResultConnected:
				cc.CameraConnected++
			default:
				cc.CameraFailed++
			}
		})
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

// QualityReport is one camera's view of a viewing session's stream (ST-3),
// from the viewer's RTCP feedback. A negative value means not measured.
type QualityReport struct {
	Country    string  // the viewer's country, "" when unknown
	DurationS  float64 // seconds since the session connected
	VideoKbps  float64 // video the camera sent, average
	LossPct    float64 // average packet loss the viewer reported
	MaxLossPct float64 // worst single report
	JitterMs   float64
	RTTMs      float64
	REMBKbps   float64 // the viewer's bandwidth estimate, last
	PLI, NACK  int     // keyframe and resend requests from the viewer
}

// Poor reports whether a session's quality was poor: average loss 3 % or
// more, or a round trip of 1 s or more.
func (q QualityReport) Poor() bool { return q.LossPct >= 3 || q.RTTMs >= 1000 }

// Streamed reports whether video went out for at least 5 s: the stricter
// success check (a session that connects but sends nothing is not one).
func (q QualityReport) Streamed() bool { return q.DurationS >= 5 && q.VideoKbps > 0 }

// LossBand and RTTBand name the band a value falls in.
func LossBand(pct float64) string {
	switch {
	case pct < 1:
		return "<1%"
	case pct < 3:
		return "1-3%"
	case pct < 10:
		return "3-10%"
	}
	return ">=10%"
}

func RTTBand(ms float64) string {
	switch {
	case ms < 100:
		return "<100ms"
	case ms < 300:
		return "100-300ms"
	case ms < 1000:
		return "300-1000ms"
	}
	return ">=1s"
}

// Quality sums end-of-session quality reports. Sums with their counts give
// averages; values not measured are left out of both.
type Quality struct {
	Sessions  int            `json:"sessions"`
	Streamed  int            `json:"streamed"`
	Poor      int            `json:"poor"`
	LossBands map[string]int `json:"loss_bands,omitempty"`
	RTTBands  map[string]int `json:"rtt_bands,omitempty"`
	LossSum   float64        `json:"loss_pct_sum"`
	LossCount int            `json:"loss_count"`
	RTTSum    float64        `json:"rtt_ms_sum"`
	RTTCount  int            `json:"rtt_count"`
	JitterSum float64        `json:"jitter_ms_sum"`
	JitterN   int            `json:"jitter_count"`
	KbpsSum   float64        `json:"video_kbps_sum"`
	KbpsCount int            `json:"video_kbps_count"`
	DurationS float64        `json:"duration_s_sum"`
	PLI       int            `json:"pli"`
	NACK      int            `json:"nack"`
}

func (q *Quality) add(o Quality) {
	q.Sessions += o.Sessions
	q.Streamed += o.Streamed
	q.Poor += o.Poor
	q.LossBands = addMap(q.LossBands, o.LossBands)
	q.RTTBands = addMap(q.RTTBands, o.RTTBands)
	q.LossSum += o.LossSum
	q.LossCount += o.LossCount
	q.RTTSum += o.RTTSum
	q.RTTCount += o.RTTCount
	q.JitterSum += o.JitterSum
	q.JitterN += o.JitterN
	q.KbpsSum += o.KbpsSum
	q.KbpsCount += o.KbpsCount
	q.DurationS += o.DurationS
	q.PLI += o.PLI
	q.NACK += o.NACK
}

// RecordQuality counts one end-of-session quality report.
func (c *Counters) RecordQuality(r QualityReport) {
	c.update(func(d *Day) {
		q := &d.Quality
		q.Sessions++
		if r.Streamed() {
			q.Streamed++
		}
		poor := r.Poor()
		if poor {
			q.Poor++
		}
		if r.LossPct >= 0 {
			q.LossBands = addMap(q.LossBands, map[string]int{LossBand(r.LossPct): 1})
			q.LossSum += r.LossPct
			q.LossCount++
		}
		if r.RTTMs >= 0 {
			q.RTTBands = addMap(q.RTTBands, map[string]int{RTTBand(r.RTTMs): 1})
			q.RTTSum += r.RTTMs
			q.RTTCount++
		}
		if r.JitterMs >= 0 {
			q.JitterSum += r.JitterMs
			q.JitterN++
		}
		if r.VideoKbps >= 0 {
			q.KbpsSum += r.VideoKbps
			q.KbpsCount++
		}
		if r.DurationS > 0 {
			q.DurationS += r.DurationS
		}
		q.PLI += r.PLI
		q.NACK += r.NACK
		d.country(r.Country, func(cc *CountryCounts) {
			cc.QualitySessions++
			if poor {
				cc.PoorQuality++
			}
		})
	})
}
