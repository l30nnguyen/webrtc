package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"signaling-server/internal/fleet"
	"signaling-server/internal/relay"
)

// statisticPath shows the relay stream servers reporting to this server and
// their statistics, to the addresses of -statistic-allow only.
const statisticPath = "/admin/statistic"

type relayDay struct {
	Day            string  `json:"day"`
	Relay          string  `json:"relay"`
	MbpsMax        float64 `json:"mbps_max"`
	AllocationsMax int64   `json:"allocations_max"`
	RelayedGB      float64 `json:"relayed_gb"` // sent to peers and clients
	NewAllocations int64   `json:"new_allocations"`
	AuthFailures   int64   `json:"auth_failures"`
	MinutesUp      int     `json:"minutes_up"`
}

type statisticRelay struct {
	relay.Registered
	Probe fleet.RelayStatus `json:"probe"`
	Chart template.HTML     `json:"-"`
}

type statisticPage struct {
	Generated string              `json:"generated"`
	Hours     int                 `json:"hours"`
	Relays    []statisticRelay    `json:"relays"`
	Others    []fleet.RelayStatus `json:"configured,omitempty"` // measured, not reporting here
	Minutes   []relay.Minute      `json:"minutes"`
	Days      []relayDay          `json:"days"`
}

func (s *Server) handleStatistic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	hours := 24
	if text := r.URL.Query().Get("hours"); text != "" {
		if n, err := strconv.Atoi(text); err == nil && n >= 1 && n <= 24*7 {
			hours = n
		}
	}
	now := time.Now().UTC()
	page := statisticPage{Generated: now.Format(time.RFC3339), Hours: hours}
	probes := map[string]fleet.RelayStatus{}
	for _, status := range s.relayStatuses() {
		probes[status.Name] = status
	}
	var minutes, month []relay.Minute
	if s.relayRegistry != nil {
		month = s.relayRegistry.Minutes(now.AddDate(0, 0, -30))
		since := now.Add(-time.Duration(hours) * time.Hour).Format(time.RFC3339)
		for _, m := range month {
			if m.Minute >= since {
				minutes = append(minutes, m)
			}
		}
		for _, registered := range s.relayRegistry.Statuses() {
			entry := statisticRelay{Registered: registered, Probe: probes[registered.Name]}
			delete(probes, registered.Name)
			entry.Chart = relayChart(registered.Name, minutes, now, hours)
			page.Relays = append(page.Relays, entry)
		}
	}
	for _, status := range probes {
		page.Others = append(page.Others, status)
	}
	sort.Slice(page.Others, func(i, j int) bool { return page.Others[i].Name < page.Others[j].Name })
	page.Minutes = minutes
	page.Days = relayDays(month)
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(page)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:")
	if err := statisticTemplate.Execute(w, page); err != nil {
		fmt.Fprintf(w, "<!-- %s -->", template.HTMLEscapeString(err.Error()))
	}
}

// relayDays sums the minutes per relay and day, newest day first.
func relayDays(minutes []relay.Minute) []relayDay {
	byKey := map[string]*relayDay{}
	for _, m := range minutes {
		key := m.Minute[:10] + "\x00" + m.Relay
		d := byKey[key]
		if d == nil {
			d = &relayDay{Day: m.Minute[:10], Relay: m.Relay}
			byKey[key] = d
		}
		d.MbpsMax = math.Max(d.MbpsMax, m.MbpsMax)
		d.AllocationsMax = max(d.AllocationsMax, m.AllocationsMax)
		d.RelayedGB += float64(m.TxBytes) / 1e9
		d.NewAllocations += m.NewAllocations
		d.AuthFailures += m.AuthFailures
		if m.State == "up" {
			d.MinutesUp++
		}
	}
	out := make([]relayDay, 0, len(byKey))
	for _, d := range byKey {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day > out[j].Day
		}
		return out[i].Relay < out[j].Relay
	})
	return out
}

// relayChart draws one relay's Mbps (average line, peak band) and
// allocations over the last hours as inline SVG.
func relayChart(name string, minutes []relay.Minute, now time.Time, hours int) template.HTML {
	const width, height, pad = 720.0, 140.0, 4.0
	start := now.Add(-time.Duration(hours) * time.Hour)
	span := now.Sub(start).Seconds()
	type point struct{ x, mbps, peak, alloc float64 }
	var points []point
	maxMbps, maxAlloc := 1.0, 1.0
	for _, m := range minutes {
		if m.Relay != name {
			continue
		}
		at, err := time.Parse(time.RFC3339, m.Minute)
		if err != nil {
			continue
		}
		p := point{x: at.Sub(start).Seconds() / span, mbps: m.MbpsAvg, peak: m.MbpsMax, alloc: float64(m.AllocationsMax)}
		points = append(points, p)
		maxMbps, maxAlloc = math.Max(maxMbps, p.peak), math.Max(maxAlloc, p.alloc)
	}
	if len(points) == 0 {
		return `<p class="empty">No reports in this period.</p>`
	}
	x := func(f float64) float64 { return pad + f*(width-2*pad) }
	y := func(v, top float64) float64 { return height - pad - v/top*(height-2*pad) }
	line := func(value func(point) float64, top float64) string {
		var b strings.Builder
		for i, p := range points {
			// A gap of more than two minutes breaks the line: the relay was silent.
			if i == 0 || (p.x-points[i-1].x)*span > 150 {
				b.WriteString(" M")
			} else {
				b.WriteString(" L")
			}
			fmt.Fprintf(&b, "%.1f %.1f", x(p.x), y(value(p), top))
		}
		return b.String()
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" class="chart" role="img" aria-label="%s: relayed Mbps and allocations, last %d hours">`,
		width, height, template.HTMLEscapeString(name), hours)
	fmt.Fprintf(&b, `<path class="peak" d="%s"/>`, line(func(p point) float64 { return p.peak }, maxMbps))
	fmt.Fprintf(&b, `<path class="mbps" d="%s"/>`, line(func(p point) float64 { return p.mbps }, maxMbps))
	fmt.Fprintf(&b, `<path class="alloc" d="%s"/>`, line(func(p point) float64 { return p.alloc }, maxAlloc))
	b.WriteString(`</svg>`)
	fmt.Fprintf(&b, `<div class="legend"><span class="k mbps">Mbps (average, peak to %.1f)</span><span class="k alloc">allocations (peak %.0f)</span><span class="range">%s – %s UTC</span></div>`,
		maxMbps, maxAlloc, start.Format("Jan 2 15:04"), now.Format("Jan 2 15:04"))
	return template.HTML(b.String())
}

var statisticTemplate = template.Must(template.New("statistic").Funcs(template.FuncMap{
	"pct": func(mbps float64, capacity int) string {
		if capacity <= 0 {
			return "–"
		}
		return fmt.Sprintf("%.0f%%", 100*mbps/float64(capacity))
	},
	"f1": func(v float64) string { return fmt.Sprintf("%.1f", v) },
	"f2": func(v float64) string { return fmt.Sprintf("%.2f", v) },
	"gb": func(b int64) string { return fmt.Sprintf("%.2f", float64(b)/1e9) },
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Relay statistics</title>
<style>
:root{--bg:#f7f7f5;--card:#fff;--fg:#1d1d1b;--muted:#6b6b66;--line:#e3e2dd;--up:#1f7a4a;--down:#b3261e;--mbps:#2f5fb3;--alloc:#c06a1b}
@media (prefers-color-scheme:dark){:root{--bg:#161615;--card:#1f1f1d;--fg:#ecebe6;--muted:#a3a29b;--line:#34332f;--up:#5cc28b;--down:#f2766d;--mbps:#7aa5f0;--alloc:#e9a25f}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.45 system-ui,-apple-system,"Segoe UI",sans-serif}
main{max-width:1100px;margin:0 auto;padding:24px 16px 48px}h1{font-size:20px;margin:0 0 4px}h2{font-size:16px;margin:28px 0 10px}
.sub{color:var(--muted);margin:0 0 16px}.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:16px;margin:0 0 14px;overflow-x:auto}
table{border-collapse:collapse;width:100%;font-variant-numeric:tabular-nums}th,td{text-align:left;padding:6px 10px;border-bottom:1px solid var(--line);white-space:nowrap}
th{color:var(--muted);font-weight:600;font-size:12px}td.n,th.n{text-align:right}.up{color:var(--up);font-weight:600}.down{color:var(--down);font-weight:600}
.relay-head{display:flex;flex-wrap:wrap;gap:6px 18px;align-items:baseline;margin-bottom:10px}.relay-head b{font-size:15px}.relay-head span{color:var(--muted)}
.chart{width:100%;height:auto;display:block}.chart path{fill:none;stroke-width:1.6;stroke-linejoin:round}.chart .mbps{stroke:var(--mbps)}
.chart .peak{stroke:var(--mbps);opacity:.3}.chart .alloc{stroke:var(--alloc);stroke-dasharray:4 3}
.legend{display:flex;flex-wrap:wrap;gap:4px 16px;color:var(--muted);font-size:12px;margin-top:6px}.legend .k:before{content:"";display:inline-block;width:14px;height:3px;margin-right:6px;vertical-align:middle}
.legend .mbps:before{background:var(--mbps)}.legend .alloc:before{background:var(--alloc)}.legend .range{margin-left:auto}.empty{color:var(--muted);margin:0}
nav a{color:var(--mbps);margin-right:10px}
</style></head><body><main>
<h1>Relay stream servers</h1>
<p class="sub">Relays that report to this control server. Updated {{.Generated}}. <nav>Period: <a href="?hours=1">1 h</a><a href="?hours=24">24 h</a><a href="?hours=168">7 days</a><a href="?format=json&amp;hours={{.Hours}}">JSON</a></nav></p>
{{if not .Relays}}<div class="card"><p class="empty">No relay has reported here yet. Start one with -control pointing at this server, and give its name a shared secret in cfg/relay-secrets.json.</p></div>{{end}}
{{if .Relays}}<div class="card"><table>
<tr><th>Relay</th><th>Region</th><th>State</th><th class="n">Mbps</th><th class="n">Load</th><th class="n">Allocations</th><th class="n">UDP / TCP / TLS</th><th>Ports (TCP, TLS)</th><th>Version</th><th>Last report</th></tr>
{{range .Relays}}<tr><td>{{.Name}}</td><td>{{.Region}}</td>
<td>{{if .Up}}<span class="up">up</span>{{else if eq .State "stopping"}}<span class="down">stopped</span>{{else}}<span class="down">silent {{.Silent}}</span>{{end}}{{with .Probe.TCP}} · tcp {{.}}{{end}}{{with .Probe.TLS}} · tls {{.}}{{end}}</td>
<td class="n">{{f1 .Mbps}}</td><td class="n">{{pct .Mbps .CapacityMbps}} of {{.CapacityMbps}}</td><td class="n">{{.Allocations}}</td>
<td class="n">{{index .ByTransport "udp"}} / {{index .ByTransport "tcp"}} / {{index .ByTransport "tls"}}</td>
<td>{{.Host}}:{{.Port}}{{if .TCPPort}}, {{.TCPPort}}{{end}}{{if .TLSPort}}, {{.TLSHost}}:{{.TLSPort}}{{end}}</td><td>{{.Version}}</td><td>{{.LastSeen}}</td></tr>{{end}}
</table></div>
<h2>Last {{.Hours}} hours</h2>
{{range .Relays}}<div class="card"><div class="relay-head"><b>{{.Name}}</b><span>{{.Region}}</span><span>relayed {{gb .TxBytes}} GB since start ({{.StartedAt}})</span><span>{{.AllocationsTotal}} allocations, {{.AuthFailures}} refused logins</span></div>{{.Chart}}</div>{{end}}{{end}}
{{if .Others}}<h2>Configured relays measured by this server</h2><div class="card"><table>
<tr><th>Relay</th><th>Region</th><th>State</th><th class="n">Mbps</th><th class="n">Allocations</th><th>At</th></tr>
{{range .Others}}<tr><td>{{.Name}}</td><td>{{.Region}}</td><td>{{if .Healthy}}<span class="up">up</span>{{else}}<span class="down">down</span>{{end}} {{.Error}}</td><td class="n">{{if .Measured}}{{f1 .Mbps}}{{else}}–{{end}}</td><td class="n">{{.Allocations}}</td><td>{{.At}}</td></tr>{{end}}
</table></div>{{end}}
{{if .Days}}<h2>Daily, last 30 days</h2><div class="card"><table>
<tr><th>Day (UTC)</th><th>Relay</th><th class="n">Peak Mbps</th><th class="n">Peak allocations</th><th class="n">Relayed GB</th><th class="n">New allocations</th><th class="n">Refused logins</th><th class="n">Minutes up</th></tr>
{{range .Days}}<tr><td>{{.Day}}</td><td>{{.Relay}}</td><td class="n">{{f1 .MbpsMax}}</td><td class="n">{{.AllocationsMax}}</td><td class="n">{{f2 .RelayedGB}}</td><td class="n">{{.NewAllocations}}</td><td class="n">{{.AuthFailures}}</td><td class="n">{{.MinutesUp}}</td></tr>{{end}}
</table></div>{{end}}
</main></body></html>`))
