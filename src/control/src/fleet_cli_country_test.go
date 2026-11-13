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
