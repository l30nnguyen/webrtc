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
