// Package host measures the machine a server runs on: CPU, memory, disk and
// network, from /proc (Linux). Keep Load in step with hostLoad in
// src/relay/host.go, which relays send in their reports.
package host

import (
	"bufio"
	"bytes"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Load is one measurement of the machine. Percentages are 0-100.
type Load struct {
	CPU     float64 `json:"cpu_pct"`  // all cores, since the previous measurement
	Load1   float64 `json:"load1"`    // 1-minute load average
	Cores   int     `json:"cores"`    // CPUs
	Mem     float64 `json:"mem_pct"`  // memory in use: total less available
	MemMB   float64 `json:"mem_mb"`   // memory in all
	RSSMB   float64 `json:"rss_mb"`   // memory of this process
	Disk    float64 `json:"disk_pct"` // the disk of the server's folder
	NetMbps float64 `json:"net_mbps"` // the busier direction, all interfaces but loopback
	At      string  `json:"at"`       // RFC 3339, UTC
}

// Gauges is the measurement as statistics gauges.
func (l Load) Gauges() map[string]float64 {
	return map[string]float64{"cpu_pct": l.CPU, "load1": l.Load1, "mem_pct": l.Mem, "rss_mb": l.RSSMB,
		"disk_pct": l.Disk, "net_mbps": l.NetMbps}
}

// Sampler measures the machine; CPU and network are the change since the
// previous Sample. It is safe for concurrent use.
type Sampler struct {
	proc string // "/proc", or a test folder
	dir  string // the folder whose disk is measured
	now  func() time.Time

	mu          sync.Mutex
	idle, total uint64
	rx, tx      uint64
	at          time.Time
	last        Load
	measured    bool
}

// NewSampler measures the disk holding dir.
func NewSampler(dir string) *Sampler {
	if dir == "" {
		dir = "."
	}
	return &Sampler{proc: "/proc", dir: dir, now: time.Now}
}

// Sample measures now. It returns false where /proc is missing (not Linux),
// and on a nil Sampler.
func (s *Sampler) Sample() (Load, bool) {
	if s == nil {
		return Load{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	idle, total, ok := s.cpuTimes()
	if !ok {
		return Load{}, false
	}
	l := Load{Cores: runtime.NumCPU(), At: now.UTC().Format(time.RFC3339)}
	// The first measurement is the average since boot.
	if busy, all := float64(total-s.total)-float64(idle-s.idle), float64(total-s.total); all > 0 {
		l.CPU = round(100 * busy / all)
	}
	s.idle, s.total = idle, total
	if data, err := os.ReadFile(s.proc + "/loadavg"); err == nil {
		if fields := strings.Fields(string(data)); len(fields) > 0 {
			l.Load1, _ = strconv.ParseFloat(fields[0], 64)
		}
	}
	memory := s.fields("/meminfo")
	if total := memory["MemTotal:"]; total > 0 {
		l.MemMB = round(float64(total) / 1024)
		l.Mem = round(100 * float64(total-min(memory["MemAvailable:"], total)) / float64(total))
	}
	if rss := s.fields("/self/status")["VmRSS:"]; rss > 0 {
		l.RSSMB = round(float64(rss) / 1024)
	}
	var fs syscall.Statfs_t
	if syscall.Statfs(s.dir, &fs) == nil && fs.Blocks > 0 {
		l.Disk = round(100 * float64(fs.Blocks-fs.Bfree) / float64(fs.Blocks-fs.Bfree+fs.Bavail))
	}
	if rx, tx, ok := s.netBytes(); ok {
		if seconds := now.Sub(s.at).Seconds(); !s.at.IsZero() && seconds > 0 && rx >= s.rx && tx >= s.tx {
			l.NetMbps = round(float64(max(rx-s.rx, tx-s.tx)) * 8 / seconds / 1e6)
		}
		s.rx, s.tx = rx, tx
	}
	s.at, s.last, s.measured = now, l, true
	return l, true
}

// Last is the latest Sample, nil before the first one.
func (s *Sampler) Last() *Load {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.measured {
		return nil
	}
	l := s.last
	return &l
}

// cpuTimes reads the idle (idle and iowait) and total jiffies of all CPUs.
func (s *Sampler) cpuTimes() (idle, total uint64, ok bool) {
	data, err := os.ReadFile(s.proc + "/stat")
	if err != nil {
		return 0, 0, false
	}
	line, _, _ := bytes.Cut(data, []byte("\n"))
	fields := strings.Fields(string(line))
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	// user nice system idle iowait irq softirq steal; guest is inside user.
	for i, field := range fields[1:min(len(fields), 9)] {
		v, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		total += v
		if i == 3 || i == 4 {
			idle += v
		}
	}
	return idle, total, true
}

// fields reads a "Name: value kB" file (meminfo, status) into kB values.
func (s *Sampler) fields(name string) map[string]uint64 {
	out := map[string]uint64{}
	f, err := os.Open(s.proc + name)
	if err != nil {
		return out
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 {
			if v, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				out[fields[0]] = v
			}
		}
	}
	return out
}

// netBytes sums the received and sent bytes of every interface but loopback.
func (s *Sampler) netBytes() (rx, tx uint64, ok bool) {
	data, err := os.ReadFile(s.proc + "/net/dev")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		name, counters, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(name) == "lo" {
			continue
		}
		fields := strings.Fields(counters)
		if len(fields) < 9 {
			continue
		}
		r, err1 := strconv.ParseUint(fields[0], 10, 64)
		t, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 == nil && err2 == nil {
			rx, tx, ok = rx+r, tx+t, true
		}
	}
	return rx, tx, ok
}

func round(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }
