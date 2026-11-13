package stats

import "testing"

func TestCountersSplitByCountry(t *testing.T) {
	c := New(nil)
	c.RequestFrom("VN")
	c.RequestFrom("VN")
	c.RequestFrom("")
	c.Record(Report{Role: RolePlayer, Result: ResultConnected, Path: "lan", Country: "VN"})
	c.Record(Report{Role: RolePlayer, Result: ResultFailed, Error: "no_path", Country: "VN"})
	c.Record(Report{Role: RoleCamera, Result: ResultConnected, Path: "relay", Country: "SG"})
	c.Record(Report{Role: RoleCamera, Result: ResultFailed, Error: "timeout", Country: "bad"})
	var day Day
	for _, d := range c.Snapshot() {
		day = d
	}
	want := map[string]CountryCounts{
		"VN": {Requests: 2, ViewerConnected: 1, ViewerFailed: 1},
		"SG": {CameraConnected: 1},
		"--": {Requests: 1, CameraFailed: 1},
	}
	if len(day.Countries) != len(want) {
		t.Fatalf("countries = %+v", day.Countries)
	}
	for cc, w := range want {
		if day.Countries[cc] != w {
			t.Fatalf("%s = %+v, want %+v", cc, day.Countries[cc], w)
		}
	}
	if day.Requests != 3 || day.Player.Connected != 1 || day.Camera.Failed != 1 {
		t.Fatalf("totals changed: %+v", day)
	}
}

func TestDayAddMergesCountries(t *testing.T) {
	a := Day{Countries: map[string]CountryCounts{"VN": {Requests: 1, ViewerConnected: 1}}}
	b := Day{Countries: map[string]CountryCounts{"VN": {Requests: 2}, "US": {CameraConnected: 3}}}
	a.Add(b)
	if a.Countries["VN"] != (CountryCounts{Requests: 3, ViewerConnected: 1}) || a.Countries["US"] != (CountryCounts{CameraConnected: 3}) {
		t.Fatalf("merged = %+v", a.Countries)
	}
	b.Countries["US"] = CountryCounts{}
	if a.Countries["US"].CameraConnected != 3 {
		t.Fatal("Add shares the other day's map")
	}
}

func TestRecordQuality(t *testing.T) {
	c := New(nil)
	c.RecordQuality(QualityReport{Country: "VN", DurationS: 300, VideoKbps: 900, LossPct: 0.5, MaxLossPct: 2, JitterMs: 10, RTTMs: 180, REMBKbps: -1, PLI: 2, NACK: 30})
	c.RecordQuality(QualityReport{Country: "VN", DurationS: 3, VideoKbps: 0, LossPct: 12, MaxLossPct: 20, JitterMs: -1, RTTMs: 1500, REMBKbps: -1})
	c.RecordQuality(QualityReport{DurationS: 60, VideoKbps: 500, LossPct: -1, MaxLossPct: -1, JitterMs: -1, RTTMs: -1, REMBKbps: -1})
	var day Day
	for _, d := range c.Snapshot() {
		day = d
	}
	q := day.Quality
	if q.Sessions != 3 || q.Streamed != 2 || q.Poor != 1 || q.LossCount != 2 || q.LossSum != 12.5 || q.RTTCount != 2 ||
		q.LossBands["<1%"] != 1 || q.LossBands[">=10%"] != 1 || q.RTTBands["100-300ms"] != 1 || q.RTTBands[">=1s"] != 1 ||
		q.JitterN != 1 || q.KbpsCount != 3 || q.PLI != 2 || q.NACK != 30 || q.DurationS != 363 {
		t.Fatalf("quality = %+v", q)
	}
	if day.Countries["VN"] != (CountryCounts{QualitySessions: 2, PoorQuality: 1}) || day.Countries["--"] != (CountryCounts{QualitySessions: 1}) {
		t.Fatalf("countries = %+v", day.Countries)
	}
	var sum Day
	sum.Add(day)
	sum.Add(day)
	if sum.Quality.Sessions != 6 || sum.Quality.LossBands["<1%"] != 2 {
		t.Fatalf("Add = %+v", sum.Quality)
	}
}
