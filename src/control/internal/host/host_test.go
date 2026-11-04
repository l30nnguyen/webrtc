package host

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeProc(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSampleMeasuresTheChangeSinceThePreviousOne(t *testing.T) {
	proc := t.TempDir()
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	s := &Sampler{proc: proc, dir: t.TempDir(), now: func() time.Time { return now }}
	writeProc(t, proc, map[string]string{
		"stat":        "cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 1 2 3 4\n",
		"loadavg":     "0.42 0.30 0.20 1/100 1234\n",
		"meminfo":     "MemTotal:        1024000 kB\nMemFree:  100000 kB\nMemAvailable:     256000 kB\n",
		"self/status": "Name:\tmatrix_cc\nVmRSS:\t   51200 kB\n",
		"net/dev": "Inter-|   Receive\n face |bytes packets errs drop fifo frame compressed multicast|bytes\n" +
			"    lo: 999999 1 0 0 0 0 0 0 999999 1 0 0 0 0 0 0\n" +
			"  eth0: 1000 1 0 0 0 0 0 0 2000 1 0 0 0 0 0 0\n",
	})
	if s.Last() != nil {
		t.Fatal("Last before any sample")
	}
	first, ok := s.Sample()
	// Since boot: 200 busy of 1000.
	if !ok || first.CPU != 20 || first.Load1 != 0.42 || first.MemMB != 1000 || first.Mem != 75 || first.RSSMB != 50 ||
		first.NetMbps != 0 || first.Disk <= 0 {
		t.Fatalf("first sample = %+v", first)
	}

	now = now.Add(10 * time.Second)
	writeProc(t, proc, map[string]string{
		// 600 more jiffies, 150 of them idle or iowait: 75% busy.
		"stat": "cpu  400 0 250 800 150 0 0 0 0 0\n",
		// eth0 received 1 MB and sent 12.5 MB in 10 s: the busier is 10 Mbps.
		"net/dev": "  eth0: 1001000 2 0 0 0 0 0 0 12502000 2 0 0 0 0 0 0\n",
	})
	second, _ := s.Sample()
	if second.CPU != 75 || second.NetMbps != 10 || second.At != "2026-11-04T10:00:10Z" {
		t.Fatalf("second sample = %+v", second)
	}
	if last := s.Last(); last == nil || *last != second {
		t.Fatalf("Last = %+v", last)
	}
}

func TestSampleWithoutProc(t *testing.T) {
	s := &Sampler{proc: filepath.Join(t.TempDir(), "none"), dir: ".", now: time.Now}
	if _, ok := s.Sample(); ok || s.Last() != nil {
		t.Fatal("measured without /proc")
	}
}

func TestSampleOnThisMachine(t *testing.T) {
	l, ok := NewSampler(t.TempDir()).Sample()
	if _, err := os.Stat("/proc/stat"); err != nil {
		t.Skip("no /proc")
	}
	if !ok || l.MemMB <= 0 || l.RSSMB <= 0 || l.Cores <= 0 {
		t.Fatalf("sample = %+v %v", l, ok)
	}
}

// The relay sends its own copy (src/relay/host.go) under these names.
func TestLoadJSONNames(t *testing.T) {
	data, _ := json.Marshal(Load{})
	if string(data) != `{"cpu_pct":0,"load1":0,"cores":0,"mem_pct":0,"mem_mb":0,"rss_mb":0,"disk_pct":0,"net_mbps":0,"at":""}` {
		t.Fatalf("JSON %s", data)
	}
}
