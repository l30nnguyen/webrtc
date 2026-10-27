package relay

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"signaling-server/internal/fleet"
)

const (
	// MeasureInterval is how often a node measures its region's relays.
	MeasureInterval = 15 * time.Second
	// staleAfter is how long a measurement counts.
	staleAfter     = 4 * MeasureInterval
	probeTimeout   = 2 * time.Second
	metricsTimeout = 5 * time.Second
	maxMetrics     = 4 << 20
)

// Status is one relay's last measurement.
type Status = fleet.RelayStatus

// Monitor measures relays: health by asking the relay port itself (a STUN
// binding request, which violet and coturn answer without credentials), load
// from the relay's metrics endpoint. Violet has no metrics of its own, so run
// node_exporter on the relay host: bandwidth comes from its network counters
// (node_network_*_bytes_total), which move as traffic flows. coturn's own
// endpoint works too (its traffic counters only grow when a session ends, so
// node_exporter is still better) and adds the allocation count
// (turn_total_allocations).
type Monitor struct {
	Client *http.Client
	Now    func() time.Time

	mu      sync.Mutex
	last    map[string]sample
	results map[string]Status
	when    map[string]time.Time
}

type sample struct {
	at     time.Time
	rx, tx float64
}

func (m *Monitor) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Measure measures each relay once, in parallel.
func (m *Monitor) Measure(ctx context.Context, relays []fleet.Relay) {
	var wg sync.WaitGroup
	for _, r := range relays {
		wg.Add(1)
		go func(r fleet.Relay) {
			defer wg.Done()
			m.measure(ctx, r)
		}(r)
	}
	wg.Wait()
}

func (m *Monitor) measure(ctx context.Context, r fleet.Relay) {
	now := m.now()
	status := Status{Name: r.Name, Region: r.Region, CapacityMbps: capacity(r), At: now.UTC().Format(time.RFC3339)}
	if err := Probe(ctx, r.Host, r.Port); err != nil {
		status.Error = err.Error()
	} else {
		status.Healthy = true
	}
	if r.Metrics != "" {
		if metrics, err := m.fetch(ctx, r.Metrics); err != nil {
			if status.Error == "" {
				status.Error = "metrics: " + err.Error()
			}
		} else {
			status.Allocations = metrics.allocations
			m.mu.Lock()
			if m.last == nil {
				m.last = map[string]sample{}
			}
			if metrics.hasTraffic {
				if prev, ok := m.last[r.Name]; ok && now.After(prev.at) && metrics.rx >= prev.rx && metrics.tx >= prev.tx {
					seconds := now.Sub(prev.at).Seconds()
					rx := (metrics.rx - prev.rx) * 8 / 1e6 / seconds
					tx := (metrics.tx - prev.tx) * 8 / 1e6 / seconds
					status.Mbps = max(rx, tx)
					status.Measured = true
				}
				m.last[r.Name] = sample{at: now, rx: metrics.rx, tx: metrics.tx}
			}
			m.mu.Unlock()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.results == nil {
		m.results, m.when = map[string]Status{}, map[string]time.Time{}
	}
	m.results[r.Name], m.when[r.Name] = status, now
}

// Load reports a relay's last measurement in the form Choose takes: known
// only while recent. A healthy relay without bandwidth figures counts as
// empty, so measured-healthy relays win over unmeasured ones.
func (m *Monitor) Load(name string) (fraction float64, healthy, known bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	status, ok := m.results[name]
	if !ok || m.now().Sub(m.when[name]) > staleAfter {
		return 0, false, false
	}
	if status.CapacityMbps > 0 {
		fraction = status.Mbps / float64(status.CapacityMbps)
	}
	return fraction, status.Healthy, true
}

// Statuses returns the recent measurements.
func (m *Monitor) Statuses() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Status
	for name, status := range m.results {
		if m.now().Sub(m.when[name]) <= staleAfter {
			out = append(out, status)
		}
	}
	return out
}

type metrics struct {
	rx, tx      float64
	hasTraffic  bool
	allocations int
}

func (m *Monitor) fetch(ctx context.Context, url string) (metrics, error) {
	client := m.Client
	if client == nil {
		client = &http.Client{Timeout: metricsTimeout}
	}
	ctx, cancel := context.WithTimeout(ctx, metricsTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return metrics{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return metrics{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return metrics{}, fmt.Errorf("answered %d", response.StatusCode)
	}
	return ParseMetrics(io.LimitReader(response.Body, maxMetrics))
}

// ParseMetrics reads Prometheus text: node_network_{receive,transmit}_bytes_total
// summed over every device but lo (else coturn's turn_total_traffic_rcvb and
// _sentb), and turn_total_allocations summed over its labels.
func ParseMetrics(r io.Reader) (metrics, error) {
	var m metrics
	var nicRx, nicTx, turnRx, turnTx float64
	var hasNIC, hasTurn bool
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		name, labels, value, ok := parseSample(line)
		if !ok {
			continue
		}
		switch name {
		case "node_network_receive_bytes_total", "node_network_transmit_bytes_total":
			if strings.Contains(labels, `device="lo"`) {
				continue
			}
			hasNIC = true
			if name == "node_network_receive_bytes_total" {
				nicRx += value
			} else {
				nicTx += value
			}
		case "turn_total_traffic_rcvb":
			hasTurn, turnRx = true, turnRx+value
		case "turn_total_traffic_sentb":
			hasTurn, turnTx = true, turnTx+value
		case "turn_total_allocations":
			m.allocations += int(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return m, err
	}
	switch {
	case hasNIC:
		m.rx, m.tx, m.hasTraffic = nicRx, nicTx, true
	case hasTurn:
		m.rx, m.tx, m.hasTraffic = turnRx, turnTx, true
	}
	return m, nil
}

// parseSample splits `name{labels} value [timestamp]`.
func parseSample(line string) (name, labels string, value float64, ok bool) {
	rest := line
	if i := strings.IndexAny(rest, "{ "); i < 0 {
		return "", "", 0, false
	} else {
		name, rest = rest[:i], rest[i:]
	}
	if strings.HasPrefix(rest, "{") {
		end := strings.LastIndex(rest, "}")
		if end < 0 {
			return "", "", 0, false
		}
		labels, rest = rest[1:end], rest[end+1:]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", "", 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return "", "", 0, false
	}
	return name, labels, v, true
}

// Probe sends a STUN binding request to the relay's port and waits for the
// matching success answer.
func Probe(ctx context.Context, host string, port int) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "udp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	request := make([]byte, 20)
	binary.BigEndian.PutUint16(request[0:], 0x0001) // binding request
	binary.BigEndian.PutUint32(request[4:], 0x2112A442)
	if _, err := rand.Read(request[8:20]); err != nil {
		return err
	}
	if _, err := conn.Write(request); err != nil {
		return err
	}
	answer := make([]byte, 1500)
	for {
		n, err := conn.Read(answer)
		if err != nil {
			return errors.New("no answer from the relay port")
		}
		if n >= 20 && binary.BigEndian.Uint16(answer[0:]) == 0x0101 && bytes.Equal(answer[8:20], request[8:20]) {
			return nil
		}
	}
}
