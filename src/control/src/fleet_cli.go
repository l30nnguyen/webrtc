package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"signaling-server/internal/admission"
	"signaling-server/internal/fleet"
	"signaling-server/internal/host"
	"signaling-server/internal/stats"
)

const fleetUsage = `usage: matrix_cc fleet [options] COMMAND

  status             nodes: up/down, cameras, clients, load; configuration version
  stats [DAYS]       viewing counters for the last DAYS UTC days (default 7)
  check FILE         validate FILE and show what would change
  apply FILE         validate, stamp the next version, keep the active file in
                     the history folder and replace it; then reload the server
  rollback [VERSION] re-apply an earlier version (default: the one before the
                     active one) as a new version
  keygen FILE        on a node: create its link key (-node-key) and print the
                     public_key line for config.json
  pubkey FILE        print the public_key of an existing link key
  blocklist-check FILE
                     validate a blocklist before installing it (a broken one
                     refuses every connection)

options:
`

// fleetCommand runs "matrix_cc fleet ..." and returns the exit code.
func fleetCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("fleet", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "/opt/matrix/cfg/config.json", "active fleet configuration")
	historyDir := flags.String("history", "", "folder keeping every applied version (default: config.d next to -config)")
	self := flags.String("self", "", "the Center's own node address, wss://host:port/ (it needs no report token)")
	geoipPath := flags.String("geoip", "", "IP-to-country CSV; warns about route countries it never names")
	socket := flags.String("socket", "", "the server's admin socket (-admin-socket); needed by status and stats, and lets apply/rollback reload the server and check count the cameras that would move")
	asJSON := flags.Bool("json", false, "status and stats: print the server's JSON answer")
	flags.Usage = func() {
		fmt.Fprint(stderr, fleetUsage)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *historyDir == "" {
		*historyDir = filepath.Join(filepath.Dir(*configPath), "config.d")
	}
	c := fleetCLI{configPath: *configPath, historyDir: *historyDir, self: *self, geoipPath: *geoipPath,
		admin: adminClient{socket: *socket}, json: *asJSON, out: stdout}
	rest := flags.Args()
	var err error
	switch {
	case len(rest) == 1 && rest[0] == "status":
		err = c.status()
	case len(rest) >= 1 && len(rest) <= 2 && rest[0] == "stats":
		days := 7
		if len(rest) == 2 {
			if days, err = strconv.Atoi(rest[1]); err != nil || days < 1 {
				fmt.Fprintln(stderr, "stats: DAYS must be a positive number")
				return 2
			}
		}
		err = c.stats(days)
	case len(rest) == 2 && rest[0] == "keygen":
		err = c.keygen(rest[1])
	case len(rest) == 2 && rest[0] == "pubkey":
		err = c.pubkey(rest[1])
	case len(rest) == 2 && rest[0] == "blocklist-check":
		if err = (admission.Blocklist{Path: rest[1]}).Validate(); err == nil {
			fmt.Fprintf(stdout, "%s is a valid blocklist.\n", rest[1])
		}
	case len(rest) == 2 && rest[0] == "check":
		err = c.check(rest[1])
	case len(rest) == 2 && rest[0] == "apply":
		err = c.apply(rest[1])
	case len(rest) <= 2 && len(rest) >= 1 && rest[0] == "rollback":
		version := 0
		if len(rest) == 2 {
			if version, err = strconv.Atoi(rest[1]); err != nil || version < 0 {
				fmt.Fprintln(stderr, "rollback: VERSION must be a number")
				return 2
			}
		}
		err = c.rollback(version)
	default:
		flags.Usage()
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	return 0
}

type fleetCLI struct {
	configPath, historyDir, self, geoipPath string
	admin                                   adminClient
	json                                    bool
	out                                     io.Writer
}

// active reads the active configuration; a missing file is an empty
// configuration at version 0.
func (c fleetCLI) active() (fleet.Config, error) {
	config, err := fleet.LoadConfig(c.configPath)
	if errors.Is(err, fs.ErrNotExist) {
		return fleet.Config{}, nil
	}
	if err != nil {
		return config, fmt.Errorf("active configuration %s: %w", c.configPath, err)
	}
	return config, nil
}

// candidate reads and validates a new configuration and prints what it
// changes. It returns the active configuration too.
func (c fleetCLI) candidate(path string) (next, active fleet.Config, changed bool, err error) {
	if c.self == "" {
		return next, active, false, errors.New("-self is required (the Center's own node address)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return next, active, false, err
	}
	if next, err = fleet.ParseConfig(data); err != nil {
		return next, active, false, fmt.Errorf("%s: %w", path, err)
	}
	if err = next.Validate(c.self); err != nil {
		return next, active, false, fmt.Errorf("%s: %w", path, err)
	}
	if active, err = c.active(); err != nil {
		return next, active, false, err
	}
	next.Normalize()
	fmt.Fprintf(c.out, "%s is valid: %d node(s), %d route(s).\n", path, len(next.Nodes), len(next.Routes))
	for _, warning := range next.Warnings(c.self) {
		fmt.Fprintln(c.out, "Warning:", warning)
	}
	c.warn(next)
	diff := fleet.Diff(active, next)
	if len(diff) == 0 {
		fmt.Fprintf(c.out, "No changes from the active version %d.\n", active.Version)
		return next, active, false, nil
	}
	fmt.Fprintf(c.out, "Changes from the active version %d:\n", active.Version)
	for _, line := range diff {
		fmt.Fprintln(c.out, "  "+line)
	}
	fmt.Fprintln(c.out, "Cameras already connected stay where they are; the change applies to new connections.")
	if c.admin.socket != "" {
		var moves MovesResult
		body, _ := json.Marshal(next)
		if err := c.admin.call(http.MethodPost, "/v1/moves", body, &moves); err != nil {
			fmt.Fprintf(c.out, "Cannot count the cameras that would move: %v\n", err)
		} else {
			fmt.Fprintf(c.out, "Of %d camera(s) online now, %d would go to another node at their next connection", moves.Online, moves.Moved)
			if moves.Unknown > 0 {
				fmt.Fprintf(c.out, " (%d connected before Center started, country unknown, not counted)", moves.Unknown)
			}
			fmt.Fprintln(c.out, ".")
		}
	}
	return next, active, true, nil
}

// warn flags route countries the GeoIP data never names (for example "UK"
// instead of "GB"): valid codes that would never match a camera.
func (c fleetCLI) warn(config fleet.Config) {
	if c.geoipPath == "" {
		return
	}
	countries, err := fleet.LoadCountries(c.geoipPath)
	if err != nil {
		fmt.Fprintf(c.out, "Warning: cannot read %s, country codes not checked: %v\n", c.geoipPath, err)
		return
	}
	var unknown []string
	for country := range config.Routes {
		if country != "*" && !countries.Has(country) {
			unknown = append(unknown, country)
		}
	}
	for _, n := range config.Nodes {
		for _, country := range n.Countries {
			if !countries.Has(country) {
				unknown = append(unknown, country)
			}
		}
	}
	sort.Strings(unknown)
	for _, country := range unknown {
		fmt.Fprintf(c.out, "Warning: no IP range is in country %q; no camera will match it\n", country)
	}
}

func (c fleetCLI) check(path string) error {
	_, _, _, err := c.candidate(path)
	return err
}

func (c fleetCLI) apply(path string) error {
	next, active, changed, err := c.candidate(path)
	if err != nil || !changed {
		return err
	}
	return c.install(next, active)
}

func (c fleetCLI) rollback(version int) error {
	versions, err := c.history()
	if err != nil {
		return err
	}
	active, err := c.active()
	if err != nil {
		return err
	}
	if version == 0 {
		version = -1
		for _, v := range versions {
			if v < active.Version {
				version = v
			}
		}
		if version < 0 {
			return fmt.Errorf("no version before the active version %d in %s", active.Version, c.historyDir)
		}
	}
	fmt.Fprintf(c.out, "Rolling back to the content of version %d.\n", version)
	next, active, changed, err := c.candidate(c.historyFile(version))
	if err != nil || !changed {
		return err
	}
	return c.install(next, active)
}

var historyName = regexp.MustCompile(`^v([0-9]+)\.json$`)

// history lists the versions kept in the history folder, oldest first.
func (c fleetCLI) history() ([]int, error) {
	entries, err := os.ReadDir(c.historyDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var versions []int
	for _, e := range entries {
		if m := historyName.FindStringSubmatch(e.Name()); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil {
				versions = append(versions, v)
			}
		}
	}
	sort.Ints(versions)
	return versions, nil
}

func (c fleetCLI) historyFile(version int) string {
	return filepath.Join(c.historyDir, fmt.Sprintf("v%d.json", version))
}

// install stamps the next version, records it (and the active one, if it was
// never recorded) in the history folder and replaces the active file
// atomically, keeping its owner and mode.
func (c fleetCLI) install(next, active fleet.Config) error {
	next.Version = active.Version + 1
	if err := os.MkdirAll(c.historyDir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(c.configPath); err == nil {
		if _, err := os.Stat(c.historyFile(active.Version)); errors.Is(err, fs.ErrNotExist) {
			if err := writeConfigFile(c.historyFile(active.Version), active, c.configPath); err != nil {
				return err
			}
		}
	}
	if err := writeConfigFile(c.historyFile(next.Version), next, c.configPath); err != nil {
		return err
	}
	if err := writeConfigFile(c.configPath, next, c.configPath); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Installed version %d as %s (kept in %s).\n", next.Version, c.configPath, c.historyFile(next.Version))
	if c.admin.socket == "" {
		fmt.Fprintln(c.out, "Reload the server to use it (systemctl reload matrix).")
		return nil
	}
	var reloaded struct {
		Version int `json:"version"`
	}
	if err := c.admin.call(http.MethodPost, "/v1/reload", nil, &reloaded); err != nil {
		return fmt.Errorf("the server did not take version %d: %w", next.Version, err)
	}
	fmt.Fprintf(c.out, "The server now runs version %d.\n", reloaded.Version)
	return nil
}

// writeConfigFile writes config to path through a temporary file and a
// rename, with the owner and mode of like (0644 when like does not exist).
func writeConfigFile(path string, config fleet.Config, like string) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	mode := os.FileMode(0o644)
	uid, gid := -1, -1
	if info, err := os.Stat(like); err == nil {
		mode = info.Mode().Perm()
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(st.Uid), int(st.Gid)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	if uid >= 0 && os.Geteuid() == 0 {
		if err := os.Chown(tmp.Name(), uid, gid); err != nil {
			return err
		}
	}
	return os.Rename(tmp.Name(), path)
}

// adminClient calls the server's admin socket.
type adminClient struct {
	socket string
}

func (a adminClient) call(method, path string, body []byte, into any) error {
	if a.socket == "" {
		return errors.New("no admin socket given (-socket)")
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", a.socket)
		}},
	}
	request, err := http.NewRequest(method, "http://matrix"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("server not reachable on %s: %w", a.socket, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &failure) == nil && failure.Error != "" {
			return errors.New(failure.Error)
		}
		return fmt.Errorf("server answered %d", response.StatusCode)
	}
	if raw, ok := into.(*json.RawMessage); ok {
		*raw = data
		return nil
	}
	return json.Unmarshal(data, into)
}

func (c fleetCLI) status() error {
	var raw json.RawMessage
	if err := c.admin.call(http.MethodGet, "/v1/status", nil, &raw); err != nil {
		return err
	}
	if c.json {
		_, err := c.out.Write(raw)
		return err
	}
	var status FleetStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return err
	}
	if status.Fleet {
		fmt.Fprintf(c.out, "Configuration version %d\n\n", status.ConfigVersion)
	} else if status.Center != "" {
		state := "linked"
		if !status.Linked {
			state = "NOT linked (see journalctl -u matrix)"
		}
		fmt.Fprintf(c.out, "Control node, %s to Center %s, configuration version %d\n\n", state, status.Center, status.ConfigVersion)
	} else {
		fmt.Fprintln(c.out, "No fleet configuration: this server serves its own cameras only.")
	}
	table := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "NODE\tADDRESS\tREGION\tSTATE\tCAMERAS\tCLIENTS\tLOAD\tCPU\tMEM\tDISK\tNET\tVERSION\tCONFIG\tLAST REPORT")
	for _, n := range status.Nodes {
		state := "DOWN"
		if n.Healthy {
			state = "up"
		}
		load := "-"
		if n.Capacity > 0 {
			load = fmt.Sprintf("%.1f%%", 100*float64(n.Cameras)/float64(n.Capacity))
		}
		config := "-"
		if n.Healthy {
			config = fmt.Sprintf("v%d", n.ConfigVersion)
			if status.Fleet && n.ConfigVersion != status.ConfigVersion {
				config += " (old)"
			}
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\n", dash(n.Name), n.Address, dash(n.Region), state,
			n.Cameras, n.Clients, load, machineColumns(n.Machine), dash(n.Version), config, dash(n.LastSeen))
	}
	if err := table.Flush(); err != nil || len(status.Relays) == 0 {
		return err
	}
	fmt.Fprintln(c.out)
	table = tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "RELAY\tREGION\tSTATE\tMBPS\tLOAD\tALLOCATIONS\tCPU\tMEM\tDISK\tNET\tVERSION\tMEASURED BY\tAT")
	for _, r := range status.Relays {
		state, mbps, load, allocations := "not measured", "-", "-", "-"
		if r.At != "" {
			state = "DOWN"
			if r.Healthy {
				state = "up"
			}
			if r.Error != "" && r.Healthy {
				state = "up (" + r.Error + ")"
			}
			for _, port := range []struct{ name, state string }{{"tcp", r.TCP}, {"tls", r.TLS}} {
				if port.state == "down" {
					state += " " + port.name + ":DOWN"
				} else if port.state != "" {
					state += " " + port.name + ":" + port.state
				}
			}
			if r.Measured {
				mbps = fmt.Sprintf("%.1f/%d", r.Mbps, r.CapacityMbps)
				load = fmt.Sprintf("%.0f%%", 100*r.Mbps/float64(max(r.CapacityMbps, 1)))
			}
			if r.Allocations > 0 {
				allocations = fmt.Sprint(r.Allocations)
			}
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.Region, state, mbps, load, allocations,
			machineColumns(r.Machine), dash(r.Version), dash(r.MeasuredBy), dash(r.At))
	}
	return table.Flush()
}

func (c fleetCLI) stats(days int) error {
	var raw json.RawMessage
	if err := c.admin.call(http.MethodGet, fmt.Sprintf("/v1/stats?days=%d", days), nil, &raw); err != nil {
		return err
	}
	if c.json {
		_, err := c.out.Write(raw)
		return err
	}
	var report struct {
		Days  int        `json:"days"`
		Total stats.Day  `json:"total"`
		ByDay stats.Days `json:"by_day"`
		Nodes []struct {
			fleet.NodeStatus
			Total stats.Day `json:"total"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		return err
	}
	t := report.Total
	fmt.Fprintf(c.out, "Last %d day(s) (UTC), whole fleet\n\n", report.Days)
	fmt.Fprintf(c.out, "Viewing requests        %d\n", t.Requests)
	fmt.Fprintf(c.out, "Camera was offline      %d\n", t.CameraOffline)
	fmt.Fprintf(c.out, "Refused at the ticket   %d\n", t.AdmissionRejected)
	fmt.Fprintf(c.out, "Refused, too many       %d\n\n", t.RateLimited)

	table := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "REPORTED BY\tCONNECTED\tFAILED\tLAN\tDIRECT\tRELAY\tRELAY READY/OFFERED\tAVG SETUP")
	for _, row := range []struct {
		name string
		o    stats.Outcomes
	}{{"camera", t.Camera}, {"viewer", t.Player}} {
		setup := "-"
		if row.o.SetupCount > 0 {
			setup = fmt.Sprintf("%d ms", row.o.SetupMillisSum/int64(row.o.SetupCount))
		}
		fmt.Fprintf(table, "%s\t%d\t%d\t%d\t%d\t%d\t%d/%d\t%s\n", row.name, row.o.Connected, row.o.Failed,
			row.o.Paths["lan"], row.o.Paths["direct"], row.o.Paths["relay"], row.o.RelayReady, row.o.RelayOffered, setup)
	}
	table.Flush()
	for _, row := range []struct {
		name string
		o    stats.Outcomes
	}{{"camera", t.Camera}, {"viewer", t.Player}} {
		if len(row.o.Errors) > 0 {
			fmt.Fprintf(c.out, "Failures seen by the %s: %s\n", row.name, countList(row.o.Errors))
		}
	}

	fmt.Fprintln(c.out)
	table = tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "DAY\tREQUESTS\tCAMERA OK/FAILED\tVIEWER OK/FAILED")
	dates := make([]string, 0, len(report.ByDay))
	for date := range report.ByDay {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	for _, date := range dates {
		d := report.ByDay[date]
		fmt.Fprintf(table, "%s\t%d\t%d/%d\t%d/%d\n", date, d.Requests, d.Camera.Connected, d.Camera.Failed, d.Player.Connected, d.Player.Failed)
	}
	table.Flush()

	fmt.Fprintln(c.out)
	table = tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "NODE\tADDRESS\tREQUESTS\tCAMERA OK/FAILED\tVIEWER OK/FAILED")
	for _, n := range report.Nodes {
		d := n.Total
		fmt.Fprintf(table, "%s\t%s\t%d\t%d/%d\t%d/%d\n", dash(n.Name), n.Address, d.Requests, d.Camera.Connected, d.Camera.Failed, d.Player.Connected, d.Player.Failed)
	}
	return table.Flush()
}

// machineColumns are the CPU, MEM, DISK and NET columns of a machine's load
// (tab-separated), dashes when it was not measured.
func machineColumns(m *host.Load) string {
	if m == nil {
		return "-\t-\t-\t-"
	}
	return fmt.Sprintf("%.0f%% (%.2f/%d)\t%.0f%% of %.0f MB\t%.0f%%\t%.1f Mbps", m.CPU, m.Load1, m.Cores, m.Mem, m.MemMB, m.Disk, m.NetMbps)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// countList prints {"no_path": 3, "closed": 1} as "no_path 3, closed 1",
// largest first.
func countList(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, counts[k])
	}
	return strings.Join(parts, ", ")
}

// keygen creates a node's link key; the file must not exist yet.
func (c fleetCLI) keygen(path string) error {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Created %s (keep it on this node only). Add to this node's entry in Center's config.json:\n", path)
	fmt.Fprintf(c.out, "  \"public_key\": \"%s\"\n", base64.RawURLEncoding.EncodeToString(public))
	return nil
}

func (c fleetCLI) pubkey(path string) error {
	key, err := readNodeKey(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "\"public_key\": \"%s\"\n", base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)))
	return nil
}
