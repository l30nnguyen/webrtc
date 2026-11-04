package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	relayVersion = "1.2.0"
	// reportInterval is how often the relay reports to its control server,
	// which counts a relay silent for three intervals as gone.
	reportInterval = 15 * time.Second
	reportTimeout  = 5 * time.Second
	// ReportPath is the control server's endpoint for relay reports.
	reportPath = "/v1/relay/report"
	// signatureHeader carries "t=<unix seconds>,sig=<base64url HMAC-SHA256>"
	// of signedPrefix + t + "\n" + body, keyed with the relay's shared secret.
	signatureHeader = "X-Relay-Signature"
	signedPrefix    = "matrix-relay-report\n"
)

// report is what the relay tells its control server: who it is and where
// viewers and cameras reach it (the control server keeps this), and its
// load (the control server picks relays for new sessions by it and keeps it
// as statistics). Keep in step with Report in
// src/control/internal/relay/registry.go.
type report struct {
	Name         string `json:"name"`
	Region       string `json:"region"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	TCPPort      int    `json:"tcp_port,omitempty"`
	TLSPort      int    `json:"tls_port,omitempty"`
	TLSHost      string `json:"tls_host,omitempty"`
	CapacityMbps int    `json:"capacity_mbps"`
	Version      string `json:"version"`
	StartedAt    string `json:"started_at"`
	// State is "up", or "stopping" in the last report before it exits.
	State string `json:"state"`

	Allocations      int64            `json:"allocations"`
	ByTransport      map[string]int64 `json:"by_transport"`
	Mbps             float64          `json:"mbps"` // since the previous report
	RxBytes          int64            `json:"rx_bytes"`
	TxBytes          int64            `json:"tx_bytes"`
	AllocationsTotal int64            `json:"allocations_total"`
	AuthFailures     int64            `json:"auth_failures"`
	QuotaRejects     int64            `json:"quota_rejects"`
	PeersDenied      int64            `json:"peers_denied"`
	// Machine is the relay host's CPU, memory, disk and network, since the
	// previous report (absent where /proc is missing).
	Machine *hostLoad `json:"machine,omitempty"`
}

// reporter sends reports to the control server named by -control.
type reporter struct {
	endpoint string
	identity report // the fixed part of every report
	stats    *stats
	secrets  *secrets
	host     *hostSampler
	client   *http.Client
	now      func() time.Time

	lastAt         time.Time
	lastRx, lastTx int64
	failing        bool
	reported       bool
}

func newReporter(o options, st *stats, s *secrets) (*reporter, error) {
	control, err := url.Parse(o.control)
	if err != nil || control.Host == "" || control.Path != "" && control.Path != "/" || control.User != nil {
		return nil, errors.New("-control must be https://host[:port] of a control server")
	}
	if control.Scheme != "https" && !(control.Scheme == "http" && isLoopback(control.Hostname())) {
		return nil, errors.New("-control must use https")
	}
	if len(s.sharedSecret()) == 0 {
		return nil, errors.New("-control needs a shared secret in the secrets file: reports are signed with it")
	}
	if !validName(o.name) {
		return nil, errors.New("-name must be 1-32 characters of [a-z0-9-]")
	}
	if o.region == "" {
		return nil, errors.New("-region is required with -control")
	}
	if o.udpListen == "" {
		return nil, errors.New("-control needs -udp: cameras reach the relay over UDP")
	}
	identity := report{
		Name: o.name, Region: o.region, Host: o.host, CapacityMbps: o.capacityMbps,
		Version: relayVersion, StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if identity.Port, err = listenPort(o.udpListen); err != nil {
		return nil, fmt.Errorf("-udp: %w", err)
	}
	if o.tcpListen != "" {
		if identity.TCPPort, err = listenPort(o.tcpListen); err != nil {
			return nil, fmt.Errorf("-tcp: %w", err)
		}
	}
	if o.tlsListen != "" {
		if identity.TLSPort, err = listenPort(o.tlsListen); err != nil {
			return nil, fmt.Errorf("-tls: %w", err)
		}
		identity.TLSHost = o.tlsHost
		if identity.TLSHost == "" {
			identity.TLSHost, err = certificateName(o.certFile, o.keyFile)
			if err != nil {
				return nil, err
			}
		}
	}
	return &reporter{
		endpoint: control.Scheme + "://" + control.Host + reportPath,
		identity: identity, stats: st, secrets: s, host: newHostSampler("/"),
		client: &http.Client{Timeout: reportTimeout}, now: time.Now,
	}, nil
}

// build fills a report with the counters, and the bandwidth since the last.
func (r *reporter) build(state string) report {
	now := r.now()
	out := r.identity
	out.State = state
	out.Allocations = r.stats.allocations.Load()
	out.AllocationsTotal = r.stats.allocationsTotal.Load()
	out.AuthFailures = r.stats.authFailures.Load()
	out.QuotaRejects = r.stats.quotaRejects.Load()
	out.PeersDenied = r.stats.peersDenied.Load()
	out.RxBytes, out.TxBytes = r.stats.rxBytes.Load(), r.stats.txBytes.Load()
	out.ByTransport = r.stats.transports()
	if !r.lastAt.IsZero() && now.After(r.lastAt) {
		seconds := now.Sub(r.lastAt).Seconds()
		out.Mbps = float64(max(out.RxBytes-r.lastRx, out.TxBytes-r.lastTx)) * 8 / 1e6 / seconds
	}
	r.lastAt, r.lastRx, r.lastTx = now, out.RxBytes, out.TxBytes
	if load, ok := r.host.Sample(); ok {
		out.Machine = &load
	}
	return out
}

func (r *reporter) send(ctx context.Context, state string) error {
	body, err := json.Marshal(r.build(state))
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(signatureHeader, sign(r.secrets.sharedSecret(), r.now(), body))
	response, err := r.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK {
		text, _ := io.ReadAll(io.LimitReader(response.Body, 200))
		return fmt.Errorf("control server answered %d %s", response.StatusCode, bytes.TrimSpace(text))
	}
	return nil
}

// run reports now and every reportInterval until ctx ends, then once more
// as "stopping" so the control server stops handing the relay out at once.
// It logs only when reporting starts or stops working.
func (r *reporter) run(ctx context.Context) {
	report := func(sendCtx context.Context, state string) {
		if err := r.send(sendCtx, state); err != nil {
			if !r.failing {
				log.Printf("relay: report to the control server failed: %v (retrying every %s)", err, reportInterval)
			}
			r.failing = true
		} else if r.failing || !r.reported {
			log.Printf("relay: reporting to the control server %s as %s", r.endpoint, r.identity.Name)
			r.failing, r.reported = false, true
		}
	}
	report(ctx, "up")
	ticker := time.NewTicker(reportInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			report(ctx, "up")
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			report(final, "stopping")
			cancel()
			return
		}
	}
}

func sign(secret []byte, at time.Time, body []byte) string {
	t := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signedPrefix + t + "\n"))
	mac.Write(body)
	return "t=" + t + ",sig=" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func listenPort(address string) (int, error) {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(port)
}

// certificateName is the first DNS name of the certificate, the name
// viewers check it for.
func certificateName(certFile, keyFile string) (string, error) {
	pair, err := (&certificate{certFile: certFile, keyFile: keyFile}).get(nil)
	if err != nil {
		return "", fmt.Errorf("TLS certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return "", fmt.Errorf("TLS certificate: %w", err)
	}
	if len(leaf.DNSNames) == 0 {
		return "", errors.New("the TLS certificate names no DNS host: set -tls-host")
	}
	return leaf.DNSNames[0], nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validName(name string) bool {
	if len(name) == 0 || len(name) > 32 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}
