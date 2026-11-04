package main

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

// hostLoad is one measurement of the machine the relay runs on, from /proc
// (Linux), sent in every report. It is a copy of src/control/internal/host
// (the relay is its own module): keep the two in step, as the control server
// reads it as host.Load. Percentages are 0-100.
type hostLoad struct {
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

// hostSampler measures the machine; CPU and network are the change since the
// previous Sample. It is safe for concurrent use.
type hostSampler struct {
	proc string // "/proc", or a test folder
	dir  string // the folder whose disk is measured
	now  func() time.Time

	mu          sync.Mutex
	idle, total uint64
	rx, tx      uint64
	at          time.Time
	last        hostLoad
	measured    bool
}

// newHostSampler measures the disk holding dir.
func newHostSampler(dir string) *hostSampler {
	if dir == "" {
		dir = "."
	}
	return &hostSampler{proc: "/proc", dir: dir, now: time.Now}
}

// Sample measures now. It returns false where /proc is missing (not Linux),
// and on a nil hostSampler.
func (s *hostSampler) Sample() (hostLoad, bool) {
	if s == nil {
		return hostLoad{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	idle, total, ok := s.cpuTimes()
	if !ok {
		return hostLoad{}, false
	}
	l := hostLoad{Cores: runtime.NumCPU(), At: now.UTC().Format(time.RFC3339)}
	// The first measurement is the average since boot.
	if busy, all := float64(total-s.total)-float64(idle-s.idle), float64(total-s.total); all > 0 {
		l.CPU = roundTenth(100 * busy / all)
	}
	s.idle, s.total = idle, total
	if data, err := os.ReadFile(s.proc + "/loadavg"); err == nil {
		if fields := strings.Fields(string(data)); len(fields) > 0 {
			l.Load1, _ = strconv.ParseFloat(fields[0], 64)
		}
	}
	memory := s.fields("/meminfo")
	if total := memory["MemTotal:"]; total > 0 {
		l.MemMB = roundTenth(float64(total) / 1024)
		l.Mem = roundTenth(100 * float64(total-min(memory["MemAvailable:"], total)) / float64(total))
	}
	if rss := s.fields("/self/status")["VmRSS:"]; rss > 0 {
		l.RSSMB = roundTenth(float64(rss) / 1024)
	}
	var fs syscall.Statfs_t
	if syscall.Statfs(s.dir, &fs) == nil && fs.Blocks > 0 {
		l.Disk = roundTenth(100 * float64(fs.Blocks-fs.Bfree) / float64(fs.Blocks-fs.Bfree+fs.Bavail))
	}
	if rx, tx, ok := s.netBytes(); ok {
		if seconds := now.Sub(s.at).Seconds(); !s.at.IsZero() && seconds > 0 && rx >= s.rx && tx >= s.tx {
			l.NetMbps = roundTenth(float64(max(rx-s.rx, tx-s.tx)) * 8 / seconds / 1e6)
		}
		s.rx, s.tx = rx, tx
	}
	s.at, s.last, s.measured = now, l, true
	return l, true
}

// Last is the latest Sample, nil before the first one.
func (s *hostSampler) Last() *hostLoad {
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
func (s *hostSampler) cpuTimes() (idle, total uint64, ok bool) {
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
func (s *hostSampler) fields(name string) map[string]uint64 {
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
func (s *hostSampler) netBytes() (rx, tx uint64, ok bool) {
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

func roundTenth(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }
