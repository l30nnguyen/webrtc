// Package history keeps the statistics of a region's services, the control
// server itself and the relays reporting to it, as time series a person or
// a charting tool can read:
//
//	DIR/day/YYYY-MM-DD.jsonl   one row per source and step (30 minutes), 30 days
//	DIR/month/YYYY-MM.jsonl    the same rows, then 12 months
//	DIR/year/YYYY.jsonl        one row per source and day, kept
//
// A day file older than 30 days is moved into its month file, and a month
// file older than 12 months is summed into days in its year file. So a full
// year keeps the step's resolution. Folding is idempotent: a row already in
// the coarser file is replaced, not added twice.
package history

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Steps a row may cover.
const (
	HalfHour = "30m"
	Hour     = "hour"
	Day      = "day"
)

// stepRank orders the steps, finest first.
var stepRank = map[string]int{HalfHour: 0, Hour: 1, Day: 2}

// ValidStep reports whether a store can keep rows of step.
func ValidStep(step string) bool { return step == HalfHour || step == Hour }

// slotStart is the start of the step that holds t.
func slotStart(t time.Time, step string) time.Time {
	t = t.UTC()
	switch step {
	case HalfHour:
		return t.Truncate(30 * time.Minute)
	case Hour:
		return t.Truncate(time.Hour)
	default:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
}

// Sources of rows.
const (
	SourceControl = "control"
	SourceRelay   = "relay"
)

const (
	// DayFiles and MonthFiles are how many day and month files are kept
	// before they are folded into the next coarser file.
	DayFiles   = 30
	MonthFiles = 12
	maxRows    = 500000
)

// Gauge is a sampled value over a row's time: its peak and its average over
// the minutes with data.
type Gauge struct {
	Max float64 `json:"max"`
	Avg float64 `json:"avg"`
}

// Row is one source's statistics over one step.
type Row struct {
	T      string `json:"t"` // the step's start, RFC 3339 UTC
	Step   string `json:"step"`
	Source string `json:"source"`
	Name   string `json:"name"`
	Region string `json:"region,omitempty"`
	// Minutes is how many minutes of the step had data (a relay's minutes up).
	Minutes int              `json:"minutes"`
	Gauges  map[string]Gauge `json:"gauges,omitempty"`
	Counts  map[string]int64 `json:"counts,omitempty"`
}

func (r Row) key() string { return r.T + "\x00" + r.Source + "\x00" + r.Name }

// open is the current step of one source.
type open struct {
	row     Row
	samples map[string]struct{ sum, n float64 }
	minutes map[int64]bool // minutes with data
}

// Store keeps rows in dir.
type Store struct {
	dir string
	Now func() time.Time
	// Step is the rows' step, HalfHour (default) or Hour.
	Step string

	mu      sync.Mutex
	current map[string]*open // by source and name
}

// Open creates dir's folders and folds what is due. step is HalfHour or Hour.
func Open(dir, step string) (*Store, error) {
	if !ValidStep(step) {
		return nil, fmt.Errorf("step %q: use %s or %s", step, HalfHour, Hour)
	}
	for _, sub := range []string{"day", "month", "year"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	s := &Store{dir: dir, Step: step, current: map[string]*open{}}
	s.fold()
	return s, nil
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// slot returns the source's open step, writing out the one before.
func (s *Store) slot(source, name, region string) *open {
	now := s.now().UTC()
	start := slotStart(now, s.Step).Format(time.RFC3339)
	id := source + "\x00" + name
	o := s.current[id]
	if o != nil && o.row.T != start {
		s.write(o.row)
		o = nil
	}
	if o == nil {
		o = &open{row: Row{T: start, Step: s.Step, Source: source, Name: name,
			Gauges: map[string]Gauge{}, Counts: map[string]int64{}},
			samples: map[string]struct{ sum, n float64 }{}, minutes: map[int64]bool{}}
		s.current[id] = o
	}
	if region != "" {
		o.row.Region = region
	}
	o.minutes[now.Unix()/60] = true
	o.row.Minutes = len(o.minutes)
	return o
}

// Sample records gauges sampled now (several samples a minute make its
// average and peak).
func (s *Store) Sample(source, name, region string, gauges map[string]float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.slot(source, name, region)
	for gauge, v := range gauges {
		sample := o.samples[gauge]
		sample.sum, sample.n = sample.sum+v, sample.n+1
		o.samples[gauge] = sample
		g := o.row.Gauges[gauge]
		g.Max, g.Avg = math.Max(g.Max, v), sample.sum/sample.n
		o.row.Gauges[gauge] = g
	}
}

// Count adds to counters of the current minute.
func (s *Store) Count(source, name, region string, counts map[string]int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.slot(source, name, region)
	for counter, n := range counts {
		if n != 0 {
			o.row.Counts[counter] += n
		}
	}
}

// Flush writes out every step that has ended and folds old files. Run it
// every minute.
func (s *Store) Flush() {
	start := slotStart(s.now(), s.Step).Format(time.RFC3339)
	s.mu.Lock()
	for id, o := range s.current {
		if o.row.T != start {
			s.write(o.row)
			delete(s.current, id)
		}
	}
	s.mu.Unlock()
	s.fold()
}

// Close writes out the open steps too, for a clean shutdown. A step written
// early is completed by a second row of the same time, which Rows adds up.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, o := range s.current {
		s.write(o.row)
		delete(s.current, id)
	}
}

func (s *Store) write(row Row) {
	line, err := json.Marshal(row)
	if err != nil {
		return
	}
	path := filepath.Join(s.dir, "day", row.T[:10]+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		warnOnce("statistics not saved: %v", err)
		return
	}
	defer f.Close()
	f.Write(append(line, '\n'))
}

// fold moves day files older than DayFiles days into month files (same
// step) and month files older than MonthFiles months into year files (days).
func (s *Store) fold() {
	now := s.now().UTC()
	oldestDay := now.AddDate(0, 0, -DayFiles).Format("2006-01-02")
	oldestMonth := now.AddDate(0, -MonthFiles, 0).Format("2006-01")
	days, _ := filepath.Glob(filepath.Join(s.dir, "day", "*.jsonl"))
	sort.Strings(days)
	for _, file := range days {
		day := strings.TrimSuffix(filepath.Base(file), ".jsonl")
		if day >= oldestDay {
			break
		}
		if err := s.foldFile(file, filepath.Join(s.dir, "month", day[:7]+".jsonl"), HalfHour); err != nil {
			warnOnce("statistics: %s not folded: %v", file, err)
			return
		}
	}
	months, _ := filepath.Glob(filepath.Join(s.dir, "month", "*.jsonl"))
	sort.Strings(months)
	for _, file := range months {
		month := strings.TrimSuffix(filepath.Base(file), ".jsonl")
		if month >= oldestMonth {
			break
		}
		if err := s.foldFile(file, filepath.Join(s.dir, "year", month[:4]+".jsonl"), Day); err != nil {
			warnOnce("statistics: %s not folded: %v", file, err)
			return
		}
	}
}

// foldFile adds from's rows, summed to step, to into (replacing rows of the
// same time and source), then removes from.
func (s *Store) foldFile(from, into, step string) error {
	rows, err := readFile(from)
	if err != nil {
		return err
	}
	existing, err := readFile(into)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	merged := map[string]Row{}
	for _, row := range existing {
		merged[row.key()] = row
	}
	for _, row := range Aggregate(rows, step) {
		merged[row.key()] = row
	}
	out := make([]Row, 0, len(merged))
	for _, row := range merged {
		out = append(out, row)
	}
	sortRows(out)
	var b strings.Builder
	for _, row := range out {
		line, _ := json.Marshal(row)
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(into+".tmp", []byte(b.String()), 0o600); err != nil {
		return err
	}
	if err := os.Rename(into+".tmp", into); err != nil {
		return err
	}
	return os.Remove(from)
}

func readFile(path string) ([]Row, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rows []Row
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var row Row
		if json.Unmarshal(scanner.Bytes(), &row) == nil && row.T != "" {
			rows = append(rows, row)
		}
	}
	return rows, scanner.Err()
}

// Query selects rows.
type Query struct {
	From, To time.Time // To is exclusive
	Step     string    // 30m, hour or day; rows already coarser stay so
	Source   string    // "" for every source
	Name     string    // "" for every name
}

// Rows returns the rows in the query's time, from whichever files hold
// that time, summed to the query's step, oldest first.
func (s *Store) Rows(q Query) ([]Row, error) {
	from, to := q.From.UTC().Format(time.RFC3339), q.To.UTC().Format(time.RFC3339)
	var files []string
	for sub, layout := range map[string]string{"day": "2006-01-02", "month": "2006-01", "year": "2006"} {
		all, _ := filepath.Glob(filepath.Join(s.dir, sub, "*.jsonl"))
		for _, file := range all {
			period := strings.TrimSuffix(filepath.Base(file), ".jsonl")
			start, err := time.Parse(layout, period)
			if err != nil {
				continue
			}
			end := map[string]time.Time{"day": start.AddDate(0, 0, 1), "month": start.AddDate(0, 1, 0), "year": start.AddDate(1, 0, 0)}[sub]
			if end.After(q.From) && start.Before(q.To) {
				files = append(files, file)
			}
		}
	}
	keep := func(row Row) bool {
		return row.T >= from && row.T < to && (q.Source == "" || row.Source == q.Source) && (q.Name == "" || row.Name == q.Name)
	}
	var rows []Row
	for _, file := range files {
		got, err := readFile(file)
		if err != nil {
			continue
		}
		for _, row := range got {
			if keep(row) {
				rows = append(rows, row)
			}
		}
		if len(rows) > maxRows {
			return nil, fmt.Errorf("more than %d rows: ask for a shorter time or a coarser step", maxRows)
		}
	}
	s.mu.Lock()
	for _, o := range s.current {
		if keep(o.row) {
			rows = append(rows, o.row)
		}
	}
	s.mu.Unlock()
	return Aggregate(rows, q.Step), nil
}

// Aggregate sums rows to step: gauges keep the peak and the average
// weighted by minutes with data; counts and minutes add up. Rows already
// coarser than step are kept as they are.
func Aggregate(rows []Row, step string) []Row {
	if _, ok := stepRank[step]; !ok {
		step = HalfHour
	}
	type acc struct {
		row           Row
		sums, weights map[string]float64
	}
	byKey := map[string]*acc{}
	for _, row := range rows {
		target := row.Step
		if stepRank[step] > stepRank[row.Step] {
			target = step
		}
		at, err := time.Parse(time.RFC3339, row.T)
		if err != nil {
			continue
		}
		start := slotStart(at, target)
		out := Row{T: start.Format(time.RFC3339), Step: target, Source: row.Source, Name: row.Name}
		a := byKey[out.key()]
		if a == nil {
			out.Gauges, out.Counts = map[string]Gauge{}, map[string]int64{}
			a = &acc{row: out, sums: map[string]float64{}, weights: map[string]float64{}}
			byKey[out.key()] = a
		}
		if row.Region != "" {
			a.row.Region = row.Region
		}
		weight := float64(max(row.Minutes, 1))
		for name, g := range row.Gauges {
			total := a.row.Gauges[name]
			total.Max = math.Max(total.Max, g.Max)
			a.sums[name] += g.Avg * weight
			a.weights[name] += weight
			a.row.Gauges[name] = total
		}
		for name, n := range row.Counts {
			a.row.Counts[name] += n
		}
		a.row.Minutes += max(row.Minutes, 1)
	}
	out := make([]Row, 0, len(byKey))
	for _, a := range byKey {
		for name, g := range a.row.Gauges {
			g.Avg = a.sums[name] / a.weights[name]
			a.row.Gauges[name] = g
		}
		out = append(out, a.row)
	}
	sortRows(out)
	return out
}

func sortRows(rows []Row) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].T != rows[j].T {
			return rows[i].T < rows[j].T
		}
		if rows[i].Source != rows[j].Source {
			return rows[i].Source < rows[j].Source
		}
		return rows[i].Name < rows[j].Name
	})
}

var warnings sync.Map

func warnOnce(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	if _, seen := warnings.LoadOrStore(message, struct{}{}); !seen {
		log.Print(message)
	}
}
