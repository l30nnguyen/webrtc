package main

import (
	"encoding/csv"
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
	"signaling-server/internal/history"
	"signaling-server/internal/relay"
)

// statisticPath shows this region's services, the control server and the
// relay stream servers reporting to it, to the -statistic-allow addresses
// only; statisticDataPath serves the same rows for charting tools.
const (
	statisticPath     = "/admin/statistic"
	statisticDataPath = "/admin/statistic/data"
)

// series is one line of a chart.
type series struct {
	label, class string
	value        func(history.Row) float64
}

func gaugeAvg(name string) func(history.Row) float64 {
	return func(r history.Row) float64 { return r.Gauges[name].Avg }
}

func gaugeMax(name string) func(history.Row) float64 {
	return func(r history.Row) float64 { return r.Gauges[name].Max }
}

func count(names ...string) func(history.Row) float64 {
	return func(r history.Row) float64 {
		var n int64
		for _, name := range names {
			n += r.Counts[name]
		}
		return float64(n)
	}
}

type chart struct {
	Title string
	SVG   template.HTML
}

type source struct {
	Name, Region string
	Charts       []chart
	Totals       []total
}

type total struct {
	Label string
	Value string
}

type statisticRelay struct {
	relay.Registered
	Probe fleet.RelayStatus
}

type statisticPage struct {
	Generated string
	Hours     int
	Step      string
	Control   []source
	Relays    []statisticRelay
	RelayData []source
	Others    []fleet.RelayStatus
	Days      []history.Row
	DataLink  string
	Error     string
}

// pageStep is the row step a chart over hours uses.
func pageStep(hours int) string {
	if hours <= 48 {
		return history.Minute
	}
	return history.Hour
}

func (s *Server) handleStatistic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	hours := 24
	if n, err := strconv.Atoi(r.URL.Query().Get("hours")); err == nil && n >= 1 && n <= 24*31 {
		hours = n
	}
	now := time.Now().UTC()
	from := now.Add(-time.Duration(hours) * time.Hour)
	page := statisticPage{Generated: now.Format("2006-01-02 15:04:05 UTC"), Hours: hours, Step: pageStep(hours),
		DataLink: fmt.Sprintf("%s?from=%s&step=%s", statisticDataPath, from.Format(time.RFC3339), pageStep(hours))}

	probes := map[string]fleet.RelayStatus{}
	for _, status := range s.relayStatuses() {
		probes[status.Name] = status
	}
	if s.relayRegistry != nil {
		for _, registered := range s.relayRegistry.Statuses() {
			page.Relays = append(page.Relays, statisticRelay{Registered: registered, Probe: probes[registered.Name]})
			delete(probes, registered.Name)
		}
	}
	for _, status := range probes {
		page.Others = append(page.Others, status)
	}
	sort.Slice(page.Others, func(i, j int) bool { return page.Others[i].Name < page.Others[j].Name })

	if s.history == nil {
		page.Error = "Statistics are off on this server (no -statistic-dir)."
	} else {
		rows, err := s.history.Rows(history.Query{From: from, To: now.Add(time.Minute), Step: page.Step})
		if err != nil {
			page.Error = err.Error()
		}
		bySource := map[string][]history.Row{}
		var names []string
		for _, row := range rows {
			key := row.Source + "\x00" + row.Name
			if _, ok := bySource[key]; !ok {
				names = append(names, key)
			}
			bySource[key] = append(bySource[key], row)
		}
		sort.Strings(names)
		for _, key := range names {
			rows := bySource[key]
			src := source{Name: rows[0].Name, Region: rows[len(rows)-1].Region}
			if rows[0].Source == history.SourceControl {
				src.Charts = []chart{
					{"Connected", lineChart(rows, from, now, []series{
						{"cameras", "s1", gaugeAvg("cameras")}, {"viewers", "s2", gaugeAvg("viewers")}})},
					{"Viewing sessions per " + page.Step, lineChart(rows, from, now, []series{
						{"requests", "s1", count("requests")}, {"connected", "s3", count("viewer_connected")},
						{"failed", "s4", count("viewer_failed", "camera_offline")}})},
				}
				src.Totals = controlTotals(rows)
				page.Control = append(page.Control, src)
			} else {
				src.Charts = []chart{
					{"Clients served", lineChart(rows, from, now, []series{
						{"UDP", "s1", gaugeAvg("clients_udp")}, {"TCP", "s2", gaugeAvg("clients_tcp")}, {"TLS", "s3", gaugeAvg("clients_tls")}})},
					{"Relayed Mbps", lineChart(rows, from, now, []series{
						{"average", "s1", gaugeAvg("mbps")}, {"peak", "s5", gaugeMax("mbps")}})},
				}
				src.Totals = relayTotals(rows)
				page.RelayData = append(page.RelayData, src)
			}
		}
		days, _ := s.history.Rows(history.Query{From: now.AddDate(0, 0, -30), To: now.Add(time.Minute), Step: history.Day})
		sort.SliceStable(days, func(i, j int) bool { return days[i].T > days[j].T })
		page.Days = days
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	if err := statisticTemplate.Execute(w, page); err != nil {
		fmt.Fprintf(w, "<!-- %s -->", template.HTMLEscapeString(err.Error()))
	}
}

func sumCounts(rows []history.Row, names ...string) int64 {
	var n int64
	for _, row := range rows {
		for _, name := range names {
			n += row.Counts[name]
		}
	}
	return n
}

func peak(rows []history.Row, gauge string) float64 {
	var m float64
	for _, row := range rows {
		m = math.Max(m, row.Gauges[gauge].Max)
	}
	return m
}

func controlTotals(rows []history.Row) []total {
	return []total{
		{"peak cameras", fmt.Sprintf("%.0f", peak(rows, "cameras"))},
		{"peak viewers", fmt.Sprintf("%.0f", peak(rows, "viewers"))},
		{"requests", fmt.Sprint(sumCounts(rows, "requests"))},
		{"connected", fmt.Sprint(sumCounts(rows, "viewer_connected"))},
		{"failed", fmt.Sprint(sumCounts(rows, "viewer_failed"))},
		{"camera offline", fmt.Sprint(sumCounts(rows, "camera_offline"))},
		{"local network / direct / relay", fmt.Sprintf("%d / %d / %d", sumCounts(rows, "viewer_path_lan"),
			sumCounts(rows, "viewer_path_direct"), sumCounts(rows, "viewer_path_relay"))},
		{"relay over UDP / TCP / TLS", fmt.Sprintf("%d / %d / %d", sumCounts(rows, "viewer_relay_udp"),
			sumCounts(rows, "viewer_relay_tcp"), sumCounts(rows, "viewer_relay_tls"))},
		{"refused (ticket / rate)", fmt.Sprintf("%d / %d", sumCounts(rows, "admission_rejected"), sumCounts(rows, "rate_limited"))},
	}
}

func relayTotals(rows []history.Row) []total {
	minutes := 0
	for _, row := range rows {
		minutes += row.Minutes
	}
	return []total{
		{"peak clients", fmt.Sprintf("%.0f", peak(rows, "clients"))},
		{"peak UDP / TCP / TLS", fmt.Sprintf("%.0f / %.0f / %.0f", peak(rows, "clients_udp"), peak(rows, "clients_tcp"), peak(rows, "clients_tls"))},
		{"peak Mbps", fmt.Sprintf("%.1f", peak(rows, "mbps"))},
		{"relayed GB", fmt.Sprintf("%.2f", float64(sumCounts(rows, "tx_bytes"))/1e9)},
		{"new clients", fmt.Sprint(sumCounts(rows, "new_clients"))},
		{"refused logins", fmt.Sprint(sumCounts(rows, "auth_failures"))},
		{"minutes up", fmt.Sprint(minutes)},
	}
}

// lineChart draws series over [from, to] as inline SVG with its own scale.
func lineChart(rows []history.Row, from, to time.Time, lines []series) template.HTML {
	const width, height, pad = 720.0, 130.0, 4.0
	span := to.Sub(from).Seconds()
	top := 1.0
	for _, row := range rows {
		for _, line := range lines {
			top = math.Max(top, line.value(row))
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" class="chart" role="img">`, width, height)
	for _, line := range lines {
		var d strings.Builder
		var lastAt time.Time
		for _, row := range rows {
			at, err := time.Parse(time.RFC3339, row.T)
			if err != nil {
				continue
			}
			// A gap of more than two steps breaks the line: no data then.
			gap := 2 * time.Minute
			if row.Step == history.Hour {
				gap = 2 * time.Hour
			}
			if lastAt.IsZero() || at.Sub(lastAt) > gap {
				d.WriteString(" M")
			} else {
				d.WriteString(" L")
			}
			lastAt = at
			x := pad + at.Sub(from).Seconds()/span*(width-2*pad)
			y := height - pad - line.value(row)/top*(height-2*pad)
			fmt.Fprintf(&d, "%.1f %.1f", x, y)
		}
		fmt.Fprintf(&b, `<path class="%s" d="%s"/>`, line.class, d.String())
	}
	b.WriteString(`</svg><div class="legend">`)
	for _, line := range lines {
		fmt.Fprintf(&b, `<span class="k %s">%s</span>`, line.class, template.HTMLEscapeString(line.label))
	}
	fmt.Fprintf(&b, `<span class="range">top %s · %s – %s UTC</span></div>`,
		strconv.FormatFloat(top, 'f', -1, 64), from.Format("Jan 2 15:04"), to.Format("Jan 2 15:04"))
	return template.HTML(b.String())
}

// parseTime takes RFC 3339 or a UTC date (YYYY-MM-DD).
func parseTime(text string, fallback time.Time) (time.Time, error) {
	if text == "" {
		return fallback, nil
	}
	if t, err := time.Parse(time.RFC3339, text); err == nil {
		return t, nil
	}
	t, err := time.Parse("2006-01-02", text)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not RFC 3339 or YYYY-MM-DD", text)
	}
	return t, nil
}

// handleStatisticData serves rows for charting tools:
//
//	/admin/statistic/data?from=2026-11-01&to=2026-11-03&step=hour&source=relay&name=relay-sg01&format=csv
//
// from defaults to a day ago, to to now; step to minute (rows already
// coarser, older than 30 days or 12 months, stay hour or day); format to
// json (an array of rows); csv has one column per gauge (_max, _avg) and count.
func (s *Server) handleStatisticData(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		http.Error(w, "statistics are off on this server", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	now := time.Now().UTC()
	from, err := parseTime(q.Get("from"), now.Add(-24*time.Hour))
	if err != nil {
		http.Error(w, "from: "+err.Error(), http.StatusBadRequest)
		return
	}
	to, err := parseTime(q.Get("to"), now.Add(time.Minute))
	if err != nil {
		http.Error(w, "to: "+err.Error(), http.StatusBadRequest)
		return
	}
	step := q.Get("step")
	if step == "" {
		step = history.Minute
	}
	if step != history.Minute && step != history.Hour && step != history.Day {
		http.Error(w, "step must be minute, hour or day", http.StatusBadRequest)
		return
	}
	rows, err := s.history.Rows(history.Query{From: from, To: to, Step: step, Source: q.Get("source"), Name: q.Get("name")})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if q.Get("format") != "csv" {
		w.Header().Set("Content-Type", "application/json")
		if rows == nil {
			rows = []history.Row{}
		}
		json.NewEncoder(w).Encode(rows)
		return
	}
	gauges, counts := map[string]bool{}, map[string]bool{}
	for _, row := range rows {
		for name := range row.Gauges {
			gauges[name] = true
		}
		for name := range row.Counts {
			counts[name] = true
		}
	}
	gaugeNames, countNames := sortedKeys(gauges), sortedKeys(counts)
	header := []string{"time", "step", "source", "name", "region", "minutes"}
	for _, name := range gaugeNames {
		header = append(header, name+"_max", name+"_avg")
	}
	header = append(header, countNames...)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="statistic.csv"`)
	out := csv.NewWriter(w)
	out.Write(header)
	for _, row := range rows {
		record := []string{row.T, row.Step, row.Source, row.Name, row.Region, strconv.Itoa(row.Minutes)}
		for _, name := range gaugeNames {
			g := row.Gauges[name]
			record = append(record, strconv.FormatFloat(g.Max, 'f', -1, 64), strconv.FormatFloat(math.Round(g.Avg*1000)/1000, 'f', -1, 64))
		}
		for _, name := range countNames {
			record = append(record, strconv.FormatInt(row.Counts[name], 10))
		}
		out.Write(record)
	}
	out.Flush()
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var statisticTemplate = template.Must(template.New("statistic").Funcs(template.FuncMap{
	"pct": func(mbps float64, capacity int) string {
		if capacity <= 0 {
			return "–"
		}
		return fmt.Sprintf("%.0f%%", 100*mbps/float64(capacity))
	},
	"f1":    func(v float64) string { return fmt.Sprintf("%.1f", v) },
	"f0":    func(v float64) string { return fmt.Sprintf("%.0f", v) },
	"gb":    func(b int64) string { return fmt.Sprintf("%.2f", float64(b)/1e9) },
	"day":   func(t string) string { return t[:10] },
	"isCtl": func(r history.Row) bool { return r.Source == history.SourceControl },
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Region statistics</title>
<style>
:root{--bg:#f7f7f5;--card:#fff;--fg:#1d1d1b;--muted:#6b6b66;--line:#e3e2dd;--up:#1f7a4a;--down:#b3261e;--s1:#2f5fb3;--s2:#c06a1b;--s3:#1f8a70;--s4:#b3261e;--s5:#2f5fb3}
@media (prefers-color-scheme:dark){:root{--bg:#161615;--card:#1f1f1d;--fg:#ecebe6;--muted:#a3a29b;--line:#34332f;--up:#5cc28b;--down:#f2766d;--s1:#7aa5f0;--s2:#e9a25f;--s3:#5cc2a6;--s4:#f2766d;--s5:#7aa5f0}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.45 system-ui,-apple-system,"Segoe UI",sans-serif}
main{max-width:1100px;margin:0 auto;padding:24px 16px 48px}h1{font-size:20px;margin:0 0 4px}h2{font-size:16px;margin:28px 0 10px}h3{font-size:13px;margin:12px 0 4px;color:var(--muted);font-weight:600}
.sub{color:var(--muted);margin:0 0 16px}.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:16px;margin:0 0 14px;overflow-x:auto}
table{border-collapse:collapse;width:100%;font-variant-numeric:tabular-nums}th,td{text-align:left;padding:6px 10px;border-bottom:1px solid var(--line);white-space:nowrap}
th{color:var(--muted);font-weight:600;font-size:12px}td.n,th.n{text-align:right}.up{color:var(--up);font-weight:600}.down{color:var(--down);font-weight:600}
.head{display:flex;flex-wrap:wrap;gap:6px 18px;align-items:baseline}.head b{font-size:15px}.head span{color:var(--muted)}
.totals{display:flex;flex-wrap:wrap;gap:6px 22px;margin:8px 0 4px}.totals div{font-size:12px;color:var(--muted)}.totals b{display:block;font-size:15px;color:var(--fg);font-variant-numeric:tabular-nums}
.chart{width:100%;height:auto;display:block}.chart path{fill:none;stroke-width:1.6;stroke-linejoin:round}
.chart .s1{stroke:var(--s1)}.chart .s2{stroke:var(--s2)}.chart .s3{stroke:var(--s3)}.chart .s4{stroke:var(--s4)}.chart .s5{stroke:var(--s5);opacity:.35}
.legend{display:flex;flex-wrap:wrap;gap:4px 16px;color:var(--muted);font-size:12px;margin-top:4px}.legend .k:before{content:"";display:inline-block;width:14px;height:3px;margin-right:6px;vertical-align:middle}
.legend .s1:before{background:var(--s1)}.legend .s2:before{background:var(--s2)}.legend .s3:before{background:var(--s3)}.legend .s4:before{background:var(--s4)}.legend .s5:before{background:var(--s5);opacity:.35}
.legend .range{margin-left:auto}.empty{color:var(--muted);margin:0}a{color:var(--s1)}nav a{margin-right:10px}
</style></head><body><main>
<h1>Region statistics</h1>
<p class="sub">This control server and the relay stream servers reporting to it. {{.Generated}}.<br>
<nav>Period: <a href="?hours=1">1 h</a><a href="?hours=24">24 h</a><a href="?hours=168">7 days</a><a href="?hours=720">30 days</a>
· Data for charting tools: <a href="{{.DataLink}}&amp;format=csv">CSV</a><a href="{{.DataLink}}&amp;format=json">JSON</a></nav></p>
{{with .Error}}<div class="card"><p class="empty">{{.}}</p></div>{{end}}

<h2>Control server, last {{.Hours}} hours</h2>
{{range .Control}}<div class="card"><div class="head"><b>{{.Name}}</b><span>{{.Region}}</span></div>
<div class="totals">{{range .Totals}}<div>{{.Label}}<b>{{.Value}}</b></div>{{end}}</div>
{{range .Charts}}<h3>{{.Title}}</h3>{{.SVG}}{{end}}</div>{{else}}<div class="card"><p class="empty">No data in this period.</p></div>{{end}}

<h2>Relay stream servers</h2>
{{if .Relays}}<div class="card"><table>
<tr><th>Relay</th><th>Region</th><th>State</th><th class="n">Clients</th><th class="n">UDP / TCP / TLS</th><th class="n">Mbps</th><th class="n">Load</th><th>Address (TCP, TLS)</th><th>Version</th><th>Last report</th></tr>
{{range .Relays}}<tr><td>{{.Name}}</td><td>{{.Region}}</td>
<td>{{if .Up}}<span class="up">up</span>{{else if eq .State "stopping"}}<span class="down">stopped</span>{{else}}<span class="down">silent {{.Silent}}</span>{{end}}{{with .Probe.TCP}} · tcp {{.}}{{end}}{{with .Probe.TLS}} · tls {{.}}{{end}}</td>
<td class="n">{{.Allocations}}</td><td class="n">{{index .ByTransport "udp"}} / {{index .ByTransport "tcp"}} / {{index .ByTransport "tls"}}</td>
<td class="n">{{f1 .Mbps}}</td><td class="n">{{pct .Mbps .CapacityMbps}} of {{.CapacityMbps}}</td>
<td>{{.Host}}:{{.Port}}{{if .TCPPort}}, {{.TCPPort}}{{end}}{{if .TLSPort}}, {{.TLSHost}}:{{.TLSPort}}{{end}}</td><td>{{.Version}}</td><td>{{.LastSeen}}</td></tr>{{end}}
</table></div>{{else}}<div class="card"><p class="empty">No relay reports here. Start one with -control pointing at this server, and give its name a shared secret in cfg/relay-secrets.json.</p></div>{{end}}
{{range .RelayData}}<div class="card"><div class="head"><b>{{.Name}}</b><span>{{.Region}}</span><span>last {{$.Hours}} hours</span></div>
<div class="totals">{{range .Totals}}<div>{{.Label}}<b>{{.Value}}</b></div>{{end}}</div>
{{range .Charts}}<h3>{{.Title}}</h3>{{.SVG}}{{end}}</div>{{end}}
{{if .Others}}<h3>Configured relays measured by this server</h3><div class="card"><table>
<tr><th>Relay</th><th>Region</th><th>State</th><th class="n">Mbps</th><th class="n">Allocations</th><th>At</th></tr>
{{range .Others}}<tr><td>{{.Name}}</td><td>{{.Region}}</td><td>{{if .Healthy}}<span class="up">up</span>{{else}}<span class="down">down</span>{{end}} {{.Error}}</td><td class="n">{{if .Measured}}{{f1 .Mbps}}{{else}}–{{end}}</td><td class="n">{{.Allocations}}</td><td>{{.At}}</td></tr>{{end}}
</table></div>{{end}}

{{if .Days}}<h2>Daily, last 30 days</h2><div class="card"><table>
<tr><th>Day (UTC)</th><th>Service</th><th class="n">Peak cameras / clients</th><th class="n">Peak viewers / Mbps</th><th class="n">Requests / GB relayed</th><th class="n">Connected / new clients</th><th class="n">Failed / refused logins</th><th class="n">Minutes</th></tr>
{{range .Days}}<tr><td>{{day .T}}</td><td>{{.Source}} {{.Name}}</td>
{{if isCtl .}}<td class="n">{{f0 (index .Gauges "cameras").Max}}</td><td class="n">{{f0 (index .Gauges "viewers").Max}}</td><td class="n">{{index .Counts "requests"}}</td><td class="n">{{index .Counts "viewer_connected"}}</td><td class="n">{{index .Counts "viewer_failed"}}</td>
{{else}}<td class="n">{{f0 (index .Gauges "clients").Max}}</td><td class="n">{{f1 (index .Gauges "mbps").Max}}</td><td class="n">{{gb (index .Counts "tx_bytes")}}</td><td class="n">{{index .Counts "new_clients"}}</td><td class="n">{{index .Counts "auth_failures"}}</td>{{end}}
<td class="n">{{.Minutes}}</td></tr>{{end}}
</table></div>{{end}}
</main></body></html>`))
