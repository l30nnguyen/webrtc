// Command matrix-relay is the relay stream server (TURN) of the matrix fleet,
// built on pion/turn. It replaces violet without changes to cameras or the
// control server:
//
//   - UDP (cameras, and viewers first), TCP and TLS (viewers behind VPNs and
//     captive networks that pass only ports 80 and 443), each on its own
//     configurable address.
//   - Credentials are the control server's expiring ones (relay.Credentials:
//     "<expiry>:<camera id>", base64(HMAC-SHA1(secret, user))) and, for a
//     drop-in switch from violet, fixed user names and passwords.
//   - A metrics endpoint gives the node monitor live allocation and traffic
//     counters in the names it already reads (turn_total_*).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/pion/logging"
	"github.com/pion/turn/v5"
)

type options struct {
	publicIP       string
	udpListen      string
	tcpListen      string
	tlsListen      string
	certFile       string
	keyFile        string
	realm          string
	secretsFile    string
	relayMin       int
	relayMax       int
	maxAllocations int
	maxPerUser     int
	metricsListen  string
	allowPrivate   bool
}

func parseFlags(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("matrix-relay", flag.ContinueOnError)
	fs.StringVar(&o.publicIP, "public-ip", "", "public IPv4 address given to clients as the relay address (required)")
	fs.StringVar(&o.udpListen, "udp", "0.0.0.0:3478", "UDP address (off when empty)")
	fs.StringVar(&o.tcpListen, "tcp", "0.0.0.0:80", "TCP address (off when empty)")
	fs.StringVar(&o.tlsListen, "tls", "0.0.0.0:443", "TLS address, used with -cert and -key (off when empty)")
	fs.StringVar(&o.certFile, "cert", "", "TLS certificate chain (PEM), reread when it changes")
	fs.StringVar(&o.keyFile, "key", "", "TLS private key (PEM)")
	fs.StringVar(&o.realm, "realm", "matrix", "realm")
	fs.StringVar(&o.secretsFile, "secrets", "", "owner-only JSON file with the shared secret and fixed users (required)")
	fs.IntVar(&o.relayMin, "relay-min", 49152, "lowest relay port")
	fs.IntVar(&o.relayMax, "relay-max", 65535, "highest relay port")
	fs.IntVar(&o.maxAllocations, "max", 1000, "allocations in all (0: no limit)")
	fs.IntVar(&o.maxPerUser, "max-per-user", 16, "allocations per camera with minted credentials (0: no limit); fixed users count only toward -max")
	fs.StringVar(&o.metricsListen, "metrics", "127.0.0.1:9641", "metrics address (off when empty)")
	fs.BoolVar(&o.allowPrivate, "allow-private-peers", false, "relay to loopback, private and link-local addresses")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if ip := net.ParseIP(o.publicIP); ip == nil || ip.To4() == nil {
		return o, errors.New("-public-ip must be an IPv4 address")
	}
	if o.secretsFile == "" {
		return o, errors.New("-secrets is required")
	}
	if (o.certFile == "") != (o.keyFile == "") {
		return o, errors.New("-cert and -key go together")
	}
	if o.certFile == "" {
		o.tlsListen = ""
	}
	if o.udpListen == "" && o.tcpListen == "" && o.tlsListen == "" {
		return o, errors.New("no listener: -udp, -tcp and -tls (with -cert) are all off")
	}
	if o.relayMin < 1024 || o.relayMax > 65535 || o.relayMin > o.relayMax {
		return o, errors.New("relay ports must satisfy 1024 <= -relay-min <= -relay-max <= 65535")
	}
	return o, nil
}

// relay is one running server and what it was started with.
type relay struct {
	server  *turn.Server
	stats   *stats
	secrets *secrets
	closers []func() error
}

func start(o options, logger logging.LoggerFactory) (*relay, error) {
	secrets, err := loadSecrets(o.secretsFile)
	if err != nil {
		return nil, err
	}
	tlsPort := 0
	if o.tlsListen != "" {
		_, port, err := net.SplitHostPort(o.tlsListen)
		if err != nil {
			return nil, fmt.Errorf("-tls: %w", err)
		}
		tlsPort, _ = strconv.Atoi(port)
	}
	st := newStats(tlsPort)
	r := &relay{stats: st, secrets: secrets}
	fail := func(err error) (*relay, error) {
		r.close()
		return nil, err
	}

	generator := func() turn.RelayAddressGenerator {
		return &countingGenerator{
			next: &turn.RelayAddressGeneratorPortRange{
				RelayAddress: net.ParseIP(o.publicIP),
				Address:      "0.0.0.0",
				MinPort:      uint16(o.relayMin),
				MaxPort:      uint16(o.relayMax),
			},
			stats: st,
		}
	}
	permit := peerFilter(o.allowPrivate, st)

	var packetConns []turn.PacketConnConfig
	var listeners []turn.ListenerConfig
	if o.udpListen != "" {
		udp, err := net.ListenPacket("udp4", o.udpListen)
		if err != nil {
			return fail(err)
		}
		r.closers = append(r.closers, udp.Close)
		packetConns = append(packetConns, turn.PacketConnConfig{PacketConn: udp, RelayAddressGenerator: generator(), PermissionHandler: permit})
	}
	if o.tcpListen != "" {
		tcp, err := net.Listen("tcp4", o.tcpListen)
		if err != nil {
			return fail(err)
		}
		r.closers = append(r.closers, tcp.Close)
		listeners = append(listeners, turn.ListenerConfig{Listener: tcp, RelayAddressGenerator: generator(), PermissionHandler: permit})
	}
	if o.tlsListen != "" {
		certs := &certificate{certFile: o.certFile, keyFile: o.keyFile}
		if _, err := certs.get(nil); err != nil {
			return fail(fmt.Errorf("TLS certificate: %w", err))
		}
		tlsListener, err := listenTLS(o.tlsListen, certs)
		if err != nil {
			return fail(err)
		}
		r.closers = append(r.closers, tlsListener.Close)
		listeners = append(listeners, turn.ListenerConfig{Listener: tlsListener, RelayAddressGenerator: generator(), PermissionHandler: permit})
	}

	r.server, err = turn.NewServer(turn.ServerConfig{
		Realm:             o.realm,
		AuthHandler:       authHandler(secrets, st, time.Now),
		QuotaHandler:      st.quota(o.maxAllocations, o.maxPerUser),
		EventHandler:      st.events(),
		LoggerFactory:     logger,
		PacketConnConfigs: packetConns,
		ListenerConfigs:   listeners,
	})
	if err != nil {
		return fail(err)
	}
	// The server closes its own listeners.
	r.closers = nil
	return r, nil
}

func (r *relay) close() {
	if r.server != nil {
		r.server.Close()
	}
	for _, c := range r.closers {
		c()
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	o, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		log.Printf("relay: %v", err)
		os.Exit(2)
	}
	logger := logging.NewDefaultLoggerFactory()
	logger.DefaultLogLevel = logging.LogLevelWarn
	r, err := start(o, logger)
	if err != nil {
		log.Fatalf("relay: %v", err)
	}
	if o.metricsListen != "" {
		metrics := &http.Server{Addr: o.metricsListen, Handler: r.stats.handler(), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := metrics.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("relay: metrics: %v", err)
			}
		}()
		defer metrics.Shutdown(context.Background())
	}
	off := func(address string) string {
		if address == "" {
			return "off"
		}
		return address
	}
	log.Printf("relay: serving udp %s, tcp %s, tls %s, relay ports %d-%d, public %s",
		off(o.udpListen), off(o.tcpListen), off(o.tlsListen), o.relayMin, o.relayMax, o.publicIP)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for s := range signals {
		if s == syscall.SIGHUP {
			if err := r.secrets.reload(); err != nil {
				log.Printf("relay: secrets not reloaded: %v", err)
			} else {
				log.Print("relay: secrets reloaded")
			}
			continue
		}
		break
	}
	log.Print("relay: stopping with " + strconv.FormatInt(r.stats.allocations.Load(), 10) + " allocations")
	r.close()
}
