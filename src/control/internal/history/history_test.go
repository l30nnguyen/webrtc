package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func at(text string) time.Time {
	t, err := time.Parse(time.RFC3339, text)
	if err != nil {
		panic(err)
	}
	return t
}

func TestHalfHourRowsFromSamplesAndCounts(t *testing.T) {
	now := at("2026-11-03T08:00:00Z")
	s, err := Open(t.TempDir(), HalfHour)
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return now }
	// Samples every 15 s for 10 minutes, then nothing until 08:40.
	for i := 0; i < 40; i++ {
		s.Sample(SourceControl, "sg00", "SG", map[string]float64{"cameras": float64(i % 4 * 2)})
		s.Count(SourceControl, "sg00", "SG", map[string]int64{"requests": 1})
		now = now.Add(15 * time.Second)
	}
	now = at("2026-11-03T08:40:00Z")
	s.Sample(SourceControl, "sg00", "SG", map[string]float64{"cameras": 10})
	rows, _ := s.Rows(Query{From: at("2026-11-03T00:00:00Z"), To: at("2026-11-04T00:00:00Z"), Step: HalfHour})
	if len(rows) != 2 || rows[0].T != "2026-11-03T08:00:00Z" || rows[1].T != "2026-11-03T08:30:00Z" {
		t.Fatalf("rows %+v", rows)
	}
	if g := rows[0].Gauges["cameras"]; g.Max != 6 || g.Avg != 3 || rows[0].Counts["requests"] != 40 || rows[0].Minutes != 10 || rows[0].Step != HalfHour {
		t.Fatalf("first step %+v", rows[0])
	}
	// Hourly: the peak, and the average weighted by minutes with data.
	hours, _ := s.Rows(Query{From: at("2026-11-03T00:00:00Z"), To: at("2026-11-04T00:00:00Z"), Step: Hour})
	if len(hours) != 1 || hours[0].Minutes != 11 || hours[0].Gauges["cameras"].Max != 10 || hours[0].Counts["requests"] != 40 {
		t.Fatalf("hour %+v", hours)
	}
	if avg := hours[0].Gauges["cameras"].Avg; avg < 3.63 || avg > 3.64 { // (3*10 + 10*1) / 11
		t.Fatalf("hour average %v", avg)
	}
	if _, err := Open(t.TempDir(), "minute"); err == nil {
		t.Fatal("a minute step accepted")
	}
}

func TestARestartMidStepAddsUp(t *testing.T) {
	dir := t.TempDir()
	now := at("2026-11-03T08:05:00Z")
	s, _ := Open(dir, HalfHour)
	s.Now = func() time.Time { return now }
	s.Count(SourceRelay, "relay-sg01", "SG", map[string]int64{"tx_bytes": 100})
	s.Close() // stopped at 08:05
	now = at("2026-11-03T08:20:00Z")
	again, _ := Open(dir, HalfHour)
	again.Now = s.Now
	again.Count(SourceRelay, "relay-sg01", "SG", map[string]int64{"tx_bytes": 50})
	rows, _ := again.Rows(Query{From: at("2026-11-03T00:00:00Z"), To: at("2026-11-04T00:00:00Z"), Step: HalfHour})
	if len(rows) != 1 || rows[0].Counts["tx_bytes"] != 150 || rows[0].Minutes != 2 {
		t.Fatalf("rows %+v", rows)
	}
}

func TestAYearAtFullStepThenDays(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, HalfHour)
	var now time.Time
	s.Now = func() time.Time { return now }
	for _, step := range []string{"2025-01-10T00:00:30Z", "2025-01-10T00:40:00Z", "2025-01-11T09:00:00Z"} {
		now = at(step)
		s.Sample(SourceRelay, "relay-sg01", "SG", map[string]float64{"clients": 4})
		s.Count(SourceRelay, "relay-sg01", "SG", map[string]int64{"tx_bytes": 100})
	}
	now = at("2025-01-11T09:35:00Z")
	s.Flush()
	if _, err := os.Stat(filepath.Join(dir, "day", "2025-01-10.jsonl")); err != nil {
		t.Fatal("day file not written")
	}

	// 31 days later the day files are in the month file, still per 30 minutes.
	now = at("2025-02-12T00:00:00Z")
	s.Flush()
	if _, err := os.Stat(filepath.Join(dir, "day", "2025-01-10.jsonl")); !os.IsNotExist(err) {
		t.Fatal("old day file kept")
	}
	rows, _ := readFile(filepath.Join(dir, "month", "2025-01.jsonl"))
	if len(rows) != 3 || rows[0].Step != HalfHour || rows[1].T != "2025-01-10T00:30:00Z" {
		t.Fatalf("month rows %+v", rows)
	}
	// Folding again (a crash between writing and removing) adds nothing twice.
	os.WriteFile(filepath.Join(dir, "day", "2025-01-11.jsonl"), []byte(`{"t":"2025-01-11T09:00:00Z","step":"30m","source":"relay","name":"relay-sg01","minutes":1,"counts":{"tx_bytes":100}}`+"\n"), 0o600)
	s.Flush()
	if rows, _ := readFile(filepath.Join(dir, "month", "2025-01.jsonl")); len(rows) != 3 {
		t.Fatalf("refold duplicated rows: %+v", rows)
	}
	// Eleven months on, still per 30 minutes.
	now = at("2025-12-20T00:00:00Z")
	s.Flush()
	if got, _ := s.Rows(Query{From: at("2025-01-01T00:00:00Z"), To: at("2025-02-01T00:00:00Z"), Step: HalfHour}); len(got) != 3 {
		t.Fatalf("a year back: %+v", got)
	}
	// After 12 months the month is days in the year file.
	now = at("2026-02-12T00:00:00Z")
	s.Flush()
	days, _ := readFile(filepath.Join(dir, "year", "2025.jsonl"))
	if len(days) != 2 || days[0].Step != Day || days[0].T != "2025-01-10T00:00:00Z" || days[0].Counts["tx_bytes"] != 200 {
		t.Fatalf("year rows %+v", days)
	}
}
