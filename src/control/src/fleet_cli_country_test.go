package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"signaling-server/internal/stats"
)

func TestPrintCountriesTopTwentyAndOther(t *testing.T) {
	countries := map[string]stats.CountryCounts{}
	for i := 0; i < 23; i++ {
		cc := fmt.Sprintf("%c%c", 'A'+i/26, 'A'+i%26)
		countries[cc] = stats.CountryCounts{Requests: 100 - i, ViewerConnected: 1}
	}
	countries["VN"] = stats.CountryCounts{Requests: 500, ViewerConnected: 3, ViewerFailed: 1}
	var out bytes.Buffer
	printCountries(&out, countries)
	text := out.String()
	lines := strings.Split(strings.TrimSpace(text), "\n")
	// header + 20 countries + other + footnote
	if len(lines) != 23 || !strings.HasPrefix(lines[1], "VN ") || !strings.Contains(lines[1], "75.0%") ||
		!strings.HasPrefix(lines[21], "other (4)") {
		t.Fatalf("table:\n%s", text)
	}
	out.Reset()
	printCountries(&out, nil)
	if out.Len() != 0 {
		t.Fatalf("empty map printed %q", out.String())
	}
}

func TestPrintQuality(t *testing.T) {
	var out bytes.Buffer
	printQuality(&out, stats.Quality{})
	if out.Len() != 0 {
		t.Fatal("printed an empty quality block")
	}
	printQuality(&out, stats.Quality{Sessions: 4, Streamed: 3, Poor: 1, LossSum: 2, LossCount: 2, RTTSum: 400, RTTCount: 2,
		LossBands: map[string]int{"<1%": 3, ">=10%": 1}, RTTBands: map[string]int{"100-300ms": 2}, DurationS: 400, PLI: 5})
	text := out.String()
	for _, want := range []string{"4 sessions, 3 streamed", "1 poor", "loss 1.00%", "round trip 200 ms", "100 s per session", "Loss: <1% 3, 1-3% 0, 3-10% 0, >=10% 1"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}
