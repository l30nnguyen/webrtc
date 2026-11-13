package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"signaling-server/internal/events"
	"signaling-server/internal/fleet"
	"signaling-server/internal/history"
	"signaling-server/internal/relay"
)

// statisticPath shows this region's services, the control server and the
// relay stream servers reporting to it, to the -statistic-allow addresses
// only; statisticDataPath serves the same rows for charting tools.
const (
	statisticPath       = "/admin/statistic"
	statisticDataPath   = "/admin/statistic/data"
	statisticEventsPath = "/admin/statistic/events"
	pageEvents          = 50 // connection log rows per page
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
	// The connection log search and what it found.
	Search    eventSearch
	Events    []events.Event
	More      bool
	EventsOff bool
	Control   []source
	Relays    []statisticRelay
	RelayData []source
	Others    []fleet.RelayStatus
	Days      []history.Row
	DataLink  string
	Error     string
}

// eventSearch is the connection log form: times in UTC, as the
// datetime-local input sends them (2026-11-03T08:00).
type eventSearch struct {
	From, To, Camera, Viewer, Type, Result string
	Page                                   int // 1 is the newest page
}

func (q eventSearch) query(now time.Time) (events.Query, error) {
	parse := func(text string, fallback time.Time) (time.Time, error) {
		if text == "" {
			return fallback, nil
		}
		for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05", time.RFC3339, "2006-01-02"} {
			if t, err := time.Parse(layout, text); err == nil {
				return t, nil
			}
		}
		return time.Time{}, fmt.Errorf("%q is not a time (2026-11-03T08:00, UTC)", text)
	}
	from, err := parse(q.From, now.Add(-24*time.Hour))
	if err != nil {
		return events.Query{}, err
	}
	to, err := parse(q.To, now.Add(time.Minute))
	if err != nil {
		return events.Query{}, err
	}
	return events.Query{From: from, To: to, Camera: q.Camera, Viewer: q.Viewer, Type: q.Type, Result: q.Result}, nil
}

func searchFrom(r *http.Request) eventSearch {
	v := r.URL.Query()
	clip := func(s string) string {
		if len(s) > 128 {
			return s[:128]
		}
		return s
	}
	page, err := strconv.Atoi(v.Get("page"))
	if err != nil || page < 1 || page > 1000 {
		page = 1
	}
	return eventSearch{From: clip(v.Get("from")), To: clip(v.Get("to")), Camera: clip(v.Get("camera")),
		Viewer: clip(v.Get("viewer")), Type: clip(v.Get("type")), Result: clip(v.Get("result")), Page: page}
}

// pageStep is the row step a chart over hours uses.
func pageStep(hours int) string {
	switch {
	case hours <= 24*7:
		return history.HalfHour
	case hours <= 24*31:
		return history.Hour
	}
	return history.Day
}

func (s *Server) handleStatistic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	hours := 24
	if n, err := strconv.Atoi(r.URL.Query().Get("hours")); err == nil && n >= 1 && n <= 24*366 {
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
				src.Charts = append(src.Charts, machineCharts(rows, from, now)...)
				src.Totals = append(controlTotals(rows), machineTotals(rows)...)
				page.Control = append(page.Control, src)
			} else {
				src.Charts = []chart{
					{"Clients served", lineChart(rows, from, now, []series{
						{"UDP", "s1", gaugeAvg("clients_udp")}, {"TCP", "s2", gaugeAvg("clients_tcp")}, {"TLS", "s3", gaugeAvg("clients_tls")}})},
					{"Relayed Mbps", lineChart(rows, from, now, []series{
						{"average", "s1", gaugeAvg("mbps")}, {"peak", "s5", gaugeMax("mbps")}})},
				}
				src.Charts = append(src.Charts, machineCharts(rows, from, now)...)
				src.Totals = append(relayTotals(rows), machineTotals(rows)...)
				page.RelayData = append(page.RelayData, src)
			}
		}
		days, _ := s.history.Rows(history.Query{From: now.AddDate(0, 0, -30), To: now.Add(time.Minute), Step: history.Day})
		sort.SliceStable(days, func(i, j int) bool { return days[i].T > days[j].T })
		page.Days = days
	}
	page.Search = searchFrom(r)
	if s.events == nil {
		page.EventsOff = true
	} else if q, err := page.Search.query(now); err != nil {
		page.Error = err.Error()
	} else {
		q.Limit = pageEvents
		q.Offset = (page.Search.Page - 1) * pageEvents
		page.Events, page.More = s.events.Find(q)
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

// measuredMachine reports whether any row has the machine's load (servers
// before 2.6.0 and relays before 1.2.0 don't send it).
func measuredMachine(rows []history.Row) bool {
	for _, row := range rows {
		if _, ok := row.Gauges["mem_pct"]; ok {
			return true
		}
	}
	return false
}

// machineCharts are the CPU, memory and disk of the source's machine.
func machineCharts(rows []history.Row, from, to time.Time) []chart {
	if !measuredMachine(rows) {
		return nil
	}
	return []chart{{"Machine %", lineChart(rows, from, to, []series{
		{"CPU", "s1", gaugeAvg("cpu_pct")}, {"CPU peak", "s5", gaugeMax("cpu_pct")},
		{"memory", "s2", gaugeAvg("mem_pct")}, {"disk", "s3", gaugeMax("disk_pct")}})}}
}

func machineTotals(rows []history.Row) []total {
	if !measuredMachine(rows) {
		return nil
	}
	return []total{
		{"peak CPU / memory / disk %", fmt.Sprintf("%.0f / %.0f / %.0f", peak(rows, "cpu_pct"), peak(rows, "mem_pct"), peak(rows, "disk_pct"))},
		{"peak load / process MB / net Mbps", fmt.Sprintf("%.2f / %.0f / %.1f", peak(rows, "load1"), peak(rows, "rss_mb"), peak(rows, "net_mbps"))},
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
			gap := map[string]time.Duration{history.HalfHour: time.Hour, history.Hour: 2 * time.Hour}[row.Step]
			if gap == 0 {
				gap = 48 * time.Hour
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
// from defaults to a day ago, to to now; step to 30m (rows already
// coarser, older than 12 months or kept per hour, stay so); format to
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
		step = history.HalfHour
	}
	if step != history.HalfHour && step != history.Hour && step != history.Day {
		http.Error(w, "step must be 30m, hour or day", http.StatusBadRequest)
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

// handleStatisticEvents serves the connection log for tools:
//
//	/admin/statistic/events?from=2026-11-03T08:00&to=2026-11-03T09:00&camera=ID&result=failed&format=csv
//
// with the page's search fields (from, to in UTC; camera, viewer, type,
// result), newest first, at most 5000 events; format json (default) or csv.
func (s *Server) handleStatisticEvents(w http.ResponseWriter, r *http.Request) {
	if s.events == nil {
		http.Error(w, "the connection log is off on this server", http.StatusNotFound)
		return
	}
	q, err := searchFrom(r).query(time.Now().UTC())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	found, more := s.events.Find(q)
	w.Header().Set("Cache-Control", "no-store")
	if more {
		w.Header().Set("X-More-Events", "true")
	}
	if r.URL.Query().Get("format") != "csv" {
		w.Header().Set("Content-Type", "application/json")
		if found == nil {
			found = []events.Event{}
		}
		json.NewEncoder(w).Encode(found)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="events.csv"`)
	out := csv.NewWriter(w)
	out.Write([]string{"time", "type", "result", "error", "camera", "viewer", "role", "path", "relay_protocol",
		"relay_offered", "relay_ready", "setup_ms", "relays", "relay", "from", "detail", "version", "country"})
	flag := func(b *bool) string {
		if b == nil {
			return ""
		}
		return strconv.FormatBool(*b)
	}
	for _, e := range found {
		setup := ""
		if e.SetupMillis > 0 {
			setup = strconv.Itoa(e.SetupMillis)
		}
		out.Write([]string{e.T, e.Type, e.Result, e.Error, e.Camera, e.Viewer, e.Role, e.Path, e.RelayProtocol,
			flag(e.RelayOffered), flag(e.RelayReady), setup, strings.Join(e.Relays, " "), e.Relay, e.From, e.Detail, e.Version, e.Country})
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
	"eventTypes": func() []string {
		return []string{events.Request, events.Report, events.Refused, events.Camera, events.Relay}
	},
	"eventResults": func() []string {
		return []string{"connected", "failed", "forwarded", "camera_offline", "online", "offline",
			"no_ticket", "bad_ticket", "too_many_connections", "registered", "restarted", "stopping", "silent", "back"}
	},
	// pageLink is this page with the same search on connection log page n.
	"pageLink": func(q eventSearch, hours, n int) string {
		v := url.Values{}
		for key, value := range map[string]string{"from": q.From, "to": q.To, "camera": q.Camera, "viewer": q.Viewer, "type": q.Type, "result": q.Result} {
			if value != "" {
				v.Set(key, value)
			}
		}
		v.Set("hours", strconv.Itoa(hours))
		v.Set("page", strconv.Itoa(n))
		return "?" + v.Encode() + "#events"
	},
	"add": func(a, b int) int { return a + b },
	"eventsLink": func(q eventSearch, format string) string {
		v := url.Values{}
		for key, value := range map[string]string{"from": q.From, "to": q.To, "camera": q.Camera, "viewer": q.Viewer, "type": q.Type, "result": q.Result} {
			if value != "" {
				v.Set(key, value)
			}
		}
		v.Set("format", format)
		return statisticEventsPath + "?" + v.Encode()
	},
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
.search{display:flex;flex-wrap:wrap;gap:8px 12px;align-items:end;margin-bottom:12px}.search label{display:flex;flex-direction:column;font-size:12px;color:var(--muted);gap:2px}
.search input,.search select,.search button{font:inherit;color:var(--fg);background:var(--bg);border:1px solid var(--line);border-radius:6px;padding:4px 6px}.search button{cursor:pointer}
table.events td{font-size:13px}tr.bad td:nth-child(3),tr.bad td:nth-child(4){color:var(--down);font-weight:600}
.pager{color:var(--muted);margin:10px 0 0;font-size:13px}.legend .range{margin-left:auto}.empty{color:var(--muted);margin:0}a{color:var(--s1)}nav a{margin-right:10px}
</style></head><body><main>
<h1>Region statistics</h1>
<p class="sub">This control server and the relay stream servers reporting to it. {{.Generated}}.<br>
<nav>Period: <a href="?hours=24">24 h</a><a href="?hours=168">7 days</a><a href="?hours=720">30 days</a><a href="?hours=8784">1 year</a>
· Data for charting tools: <a href="{{.DataLink}}&amp;format=csv">CSV</a><a href="{{.DataLink}}&amp;format=json">JSON</a></nav></p>
{{with .Error}}<div class="card"><p class="empty">{{.}}</p></div>{{end}}

<h2 id="events">Connections</h2>
{{if .EventsOff}}<div class="card"><p class="empty">The connection log is off on this server (no -event-dir).</p></div>{{else}}
<div class="card"><form method="get" action="#events" class="search">
<label>From (UTC)<input type="datetime-local" name="from" value="{{.Search.From}}"></label>
<label>To (UTC)<input type="datetime-local" name="to" value="{{.Search.To}}"></label>
<label>Camera<input name="camera" value="{{.Search.Camera}}" size="24"></label>
<label>Viewer<input name="viewer" value="{{.Search.Viewer}}" size="16"></label>
<label>Event<select name="type"><option value="">any</option>{{range $t := eventTypes}}<option{{if eq $t $.Search.Type}} selected{{end}}>{{$t}}</option>{{end}}</select></label>
<label>Result<select name="result"><option value="">any</option>{{range $r := eventResults}}<option{{if eq $r $.Search.Result}} selected{{end}}>{{$r}}</option>{{end}}</select></label>
<input type="hidden" name="hours" value="{{.Hours}}"><button>Search</button>
<a href="{{eventsLink .Search "csv"}}">CSV</a><a href="{{eventsLink .Search "json"}}">JSON</a></form>
{{if .Events}}<table class="events">
<tr><th>Time (UTC)</th><th>Event</th><th>Result</th><th>Cause</th><th>Path</th><th class="n">Setup</th><th>Relays / from</th><th>Camera</th><th>Viewer</th></tr>
{{range .Events}}<tr class="{{if or (eq .Result "failed") (eq .Result "camera_offline") (eq .Result "silent") (eq .Type "refused")}}bad{{end}}"><td>{{.T}}</td><td>{{.Type}}{{with .Role}} ({{.}}){{end}}{{with .Version}} v{{.}}{{end}}{{with .Country}} {{.}}{{end}}</td><td>{{.Result}}</td><td>{{.Error}}{{.Detail}}</td>
<td>{{.Path}}{{with .RelayProtocol}} over {{.}}{{end}}</td><td class="n">{{if .SetupMillis}}{{.SetupMillis}} ms{{end}}</td><td>{{range .Relays}}{{.}} {{end}}{{.Relay}}{{.From}}</td><td>{{.Camera}}</td><td>{{.Viewer}}</td></tr>{{end}}
</table>
<p class="pager">Page {{.Search.Page}}{{if gt .Search.Page 1}} · <a href="{{pageLink .Search .Hours 1}}">newest</a> · <a href="{{pageLink .Search .Hours (add .Search.Page -1)}}">← newer</a>{{end}}{{if .More}} · <a href="{{pageLink .Search .Hours (add .Search.Page 1)}}">older →</a>{{end}}</p>
{{else if gt .Search.Page 1}}<p class="empty">No more events. <a href="{{pageLink .Search .Hours 1}}">Back to the newest</a>.</p>
{{else}}<p class="empty">No events match.</p>{{end}}</div>{{end}}

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
