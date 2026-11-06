// Package events keeps what happened to each connection, so an operator
// can look up whether a viewing attempt at a given time worked and, if not,
// why: one JSON line per event in DIR/YYYY-MM-DD.jsonl (UTC), for a set
// number of days.
package events

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Types of events.
const (
	Request = "request" // a viewer asked for a camera
	Report  = "report"  // a camera or viewer told how the attempt ended
	Refused = "refused" // a connection or request was turned away
	Camera  = "camera"  // a camera came online or went offline
	Relay   = "relay"   // a relay stream server registered, stopped, went silent
)

const (
	// DefaultDays is how long events are kept by default.
	DefaultDays = 30
	// DefaultMaxDayBytes caps one day's file, so a flood of attempts can't
	// fill the disk; later events of that day are dropped (and counted).
	DefaultMaxDayBytes = 512 << 20
	maxResults         = 5000
)

// Event is one thing that happened. Fields that don't apply are left out.
type Event struct {
	T      string `json:"t"` // RFC 3339 UTC, milliseconds
	Type   string `json:"type"`
	Camera string `json:"camera,omitempty"` // device ID
	Viewer string `json:"viewer,omitempty"` // viewer client ID
	// Role is who sent a report: camera or viewer.
	Role string `json:"role,omitempty"`
	// Result: connected, failed (report); forwarded, camera_offline
	// (request); online, offline (camera); registered, restarted, stopping,
	// silent, back (relay); the refusal (refused).
	Result        string   `json:"result"`
	Error         string   `json:"error,omitempty"` // a failed report's cause
	Path          string   `json:"path,omitempty"`  // lan, direct, relay
	RelayProtocol string   `json:"relay_protocol,omitempty"`
	RelayOffered  *bool    `json:"relay_offered,omitempty"`
	RelayReady    *bool    `json:"relay_ready,omitempty"`
	SetupMillis   int      `json:"setup_ms,omitempty"`
	Relays        []string `json:"relays,omitempty"` // relays handed out for a request
	Relay         string   `json:"relay,omitempty"`  // a relay event's relay
	From          string   `json:"from,omitempty"`   // source address of a refusal
	Detail        string   `json:"detail,omitempty"`
	Version       string   `json:"version,omitempty"` // the reporter's software version
}

// Log appends events to day files and removes files older than Days.
type Log struct {
	dir         string
	Days        int
	MaxDayBytes int64
	Now         func() time.Time

	mu      sync.Mutex
	day     string
	file    *os.File
	size    int64
	dropped int64
}

// Open creates dir and removes what is past days.
func Open(dir string, days int) (*Log, error) {
	if days < 1 || days > 366 {
		return nil, fmt.Errorf("days %d: use 1 to 366", days)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	l := &Log{dir: dir, Days: days, MaxDayBytes: DefaultMaxDayBytes}
	l.prune(l.now())
	return l, nil
}

func (l *Log) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Add records an event now.
func (l *Log) Add(e Event) {
	if l == nil {
		return
	}
	now := l.now().UTC()
	e.T = now.Format("2006-01-02T15:04:05.000Z")
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	line = append(line, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	day := now.Format("2006-01-02")
	if day != l.day {
		l.openDay(day, now)
	}
	if l.file == nil {
		return
	}
	if l.size+int64(len(line)) > l.MaxDayBytes {
		if l.dropped == 0 {
			log.Printf("events: %s reached %d bytes; dropping the rest of the day's events", day, l.MaxDayBytes)
		}
		l.dropped++
		return
	}
	n, _ := l.file.Write(line)
	l.size += int64(n)
}

func (l *Log) openDay(day string, now time.Time) {
	if l.file != nil {
		if l.dropped > 0 {
			log.Printf("events: %d events of %s were dropped (over the day's size cap)", l.dropped, l.day)
		}
		l.file.Close()
		l.file = nil
	}
	l.day, l.size, l.dropped = day, 0, 0
	f, err := os.OpenFile(filepath.Join(l.dir, day+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("events: %v", err)
		return
	}
	if info, err := f.Stat(); err == nil {
		l.size = info.Size()
	}
	l.file = f
	l.prune(now)
}

// prune removes the day files older than Days.
func (l *Log) prune(now time.Time) {
	oldest := now.UTC().AddDate(0, 0, -l.Days).Format("2006-01-02")
	files, _ := filepath.Glob(filepath.Join(l.dir, "*.jsonl"))
	for _, file := range files {
		if strings.TrimSuffix(filepath.Base(file), ".jsonl") < oldest {
			os.Remove(file)
		}
	}
}

// Close closes the current file.
func (l *Log) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		l.file.Close()
		l.file = nil
		l.day = ""
	}
}

// Query selects events.
type Query struct {
	From, To time.Time // To is exclusive
	Type     string
	Camera   string
	Viewer   string
	Result   string // e.g. failed
	Relay    string
	Limit    int // newest first; 0 means maxResults
	Offset   int // matches skipped before the first returned (paging)
}

// Find returns the matching events, newest first, after skipping q.Offset of
// them, and whether more matched than the limit.
func (l *Log) Find(q Query) ([]Event, bool) {
	limit := q.Limit
	if limit <= 0 || limit > maxResults {
		limit = maxResults
	}
	from, to := q.From.UTC().Format("2006-01-02T15:04:05.000Z"), q.To.UTC().Format("2006-01-02T15:04:05.000Z")
	var days []string
	for d := q.From.UTC().Truncate(24 * time.Hour); d.Before(q.To); d = d.AddDate(0, 0, 1) {
		days = append(days, d.Format("2006-01-02"))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))
	var out []Event
	skip := q.Offset
	for _, day := range days {
		f, err := os.Open(filepath.Join(l.dir, day+".jsonl"))
		if err != nil {
			continue
		}
		var matched []Event
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		for scanner.Scan() {
			var e Event
			if json.Unmarshal(scanner.Bytes(), &e) != nil || e.T < from || e.T >= to {
				continue
			}
			if (q.Type != "" && e.Type != q.Type) || (q.Camera != "" && e.Camera != q.Camera) ||
				(q.Viewer != "" && e.Viewer != q.Viewer) || (q.Result != "" && e.Result != q.Result) ||
				(q.Relay != "" && e.Relay != q.Relay && !contains(e.Relays, q.Relay)) {
				continue
			}
			matched = append(matched, e)
		}
		f.Close()
		for i := len(matched) - 1; i >= 0; i-- {
			if skip > 0 {
				skip--
				continue
			}
			if len(out) == limit {
				return out, true
			}
			out = append(out, matched[i])
		}
	}
	return out, false
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
