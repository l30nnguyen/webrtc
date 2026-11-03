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

func TestMinutesFromSamplesAndCounts(t *testing.T) {
	now := at("2026-11-03T08:00:00Z")
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return now }
	for _, v := range []float64{2, 4, 6} {
		s.Sample(SourceControl, "sg00", "SG", map[string]float64{"cameras": v})
		s.Count(SourceControl, "sg00", "SG", map[string]int64{"requests": 1})
		now = now.Add(15 * time.Second)
	}
	now = at("2026-11-03T08:01:30Z")
	s.Sample(SourceControl, "sg00", "SG", map[string]float64{"cameras": 10})
	rows, _ := s.Rows(Query{From: at("2026-11-03T00:00:00Z"), To: at("2026-11-04T00:00:00Z"), Step: Minute})
	if len(rows) != 2 {
		t.Fatalf("rows %+v", rows)
	}
	if g := rows[0].Gauges["cameras"]; g.Max != 6 || g.Avg != 4 || rows[0].Counts["requests"] != 3 || rows[0].Region != "SG" {
		t.Fatalf("first minute %+v", rows[0])
	}
	// Hourly: the peak, and the average over the minutes with data.
	hours, _ := s.Rows(Query{From: at("2026-11-03T00:00:00Z"), To: at("2026-11-04T00:00:00Z"), Step: Hour})
	if len(hours) != 1 || hours[0].Minutes != 2 || hours[0].Gauges["cameras"].Max != 10 || hours[0].Gauges["cameras"].Avg != 7 || hours[0].Counts["requests"] != 3 {
		t.Fatalf("hour %+v", hours)
	}
}

func TestOldFilesFoldIntoMonthsAndYears(t *testing.T) {
	dir := t.TempDir()
	now := at("2025-01-10T00:00:30Z")
	s, _ := Open(dir)
	s.Now = func() time.Time { return now }
	// Two minutes on 2025-01-10, one more the day after; one relay sample a day.
	for _, minute := range []string{"2025-01-10T00:00:30Z", "2025-01-10T00:01:30Z", "2025-01-11T09:00:00Z"} {
		now = at(minute)
		s.Sample(SourceRelay, "relay-sg01", "SG", map[string]float64{"clients": 4})
		s.Count(SourceRelay, "relay-sg01", "SG", map[string]int64{"tx_bytes": 100})
	}
	now = at("2025-01-11T09:05:00Z")
	s.Flush()
	if _, err := os.Stat(filepath.Join(dir, "day", "2025-01-10.jsonl")); err != nil {
		t.Fatal("day file not written")
	}

	// 31 days later the day files are hours in the month file.
	now = at("2025-02-12T00:00:00Z")
	s.Flush()
	if _, err := os.Stat(filepath.Join(dir, "day", "2025-01-10.jsonl")); !os.IsNotExist(err) {
		t.Fatal("old day file kept")
	}
	rows, _ := readFile(filepath.Join(dir, "month", "2025-01.jsonl"))
	if len(rows) != 2 || rows[0].Step != Hour || rows[0].Minutes != 2 || rows[0].Counts["tx_bytes"] != 200 {
		t.Fatalf("month rows %+v", rows)
	}
	// Folding again (a crash between writing and removing) adds nothing twice.
	os.WriteFile(filepath.Join(dir, "day", "2025-01-11.jsonl"), []byte(`{"t":"2025-01-11T09:00:00Z","step":"minute","source":"relay","name":"relay-sg01","minutes":1,"counts":{"tx_bytes":100}}`+"\n"), 0o600)
	s.Flush()
	if rows, _ := readFile(filepath.Join(dir, "month", "2025-01.jsonl")); len(rows) != 2 {
		t.Fatalf("refold duplicated rows: %+v", rows)
	}

	// 13 months later the month is days in the year file.
	now = at("2026-02-12T00:00:00Z")
	s.Flush()
	days, _ := readFile(filepath.Join(dir, "year", "2025.jsonl"))
	if len(days) != 2 || days[0].Step != Day || days[0].T != "2025-01-10T00:00:00Z" || days[0].Counts["tx_bytes"] != 200 {
		t.Fatalf("year rows %+v", days)
	}
	// A query over the whole year reads the year file.
	got, _ := s.Rows(Query{From: at("2025-01-01T00:00:00Z"), To: at("2026-01-01T00:00:00Z"), Step: Minute, Source: SourceRelay})
	if len(got) != 2 || got[1].Counts["tx_bytes"] != 100 {
		t.Fatalf("query %+v", got)
	}
}
