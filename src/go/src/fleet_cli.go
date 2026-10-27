package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"syscall"

	"signaling-server/internal/fleet"
)

const fleetUsage = `usage: matrix_cc fleet [options] check FILE | apply FILE | rollback [VERSION]

  check FILE         validate FILE and show what would change
  apply FILE         validate, stamp the next version, keep the active file in
                     the history folder and replace it; then reload the server
  rollback [VERSION] re-apply an earlier version (default: the one before the
                     active one) as a new version

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
	c := fleetCLI{configPath: *configPath, historyDir: *historyDir, self: *self, geoipPath: *geoipPath, out: stdout}
	rest := flags.Args()
	var err error
	switch {
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
