// Command relaycheck tests a relay stream server the way a viewer uses it:
// for UDP, TCP and TLS it logs in with a per-session credential minted from
// the relay's shared secret (as the control server does), allocates a relay
// address, and sends one packet through it to itself. Used by matrix-provision
// on the admin server; prints one line per protocol and exits 1 on any failure.
//
//	relaycheck -host mx-relay-sg01.5gen.care -ip 203.0.113.7 -secret-file secret \
//	    [-udp 3478] [-tcp 80] [-tls 443] [-name check]
//
// A port of 0 skips that protocol. The secret file holds the shared secret
// only (the "shared" value of the relay's secrets.json).
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/pion/logging"
	"github.com/pion/stun/v4"
	"github.com/pion/turn/v5"
)

func credential(secret []byte, name string) (string, string) {
	user := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10) + ":" + name
	mac := hmac.New(sha1.New, secret)
	mac.Write([]byte(user))
	return user, base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// check allocates through conn and loops one packet back through the relay:
// a plain UDP socket on this host (the "peer") sends to the relayed address,
// and the allocation must receive it. The peer's public address, which the
// allocation must permit, comes from a STUN binding request to the relay.
func check(conn net.PacketConn, server, udpServer, user, password string) (string, error) {
	logger := logging.NewDefaultLoggerFactory()
	logger.DefaultLogLevel = logging.LogLevelDisabled
	client, err := turn.NewClient(&turn.ClientConfig{STUNServerAddr: server, TURNServerAddr: server, Conn: conn,
		Username: user, Password: password, Realm: "matrix", LoggerFactory: logger, RTO: time.Second})
	if err != nil {
		return "", err
	}
	defer client.Close()
	if err := client.Listen(); err != nil {
		return "", err
	}
	relayed, err := client.Allocate()
	if err != nil {
		return "", fmt.Errorf("allocate: %w", err)
	}
	defer relayed.Close()
	at := relayed.LocalAddr().String()
	peer, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return at, err
	}
	defer peer.Close()
	public, err := mappedAddr(peer, udpServer)
	if err != nil {
		return at, fmt.Errorf("allocated %s; loop test skipped: %v", at, err)
	}
	// Writing to the peer creates the permission for its address.
	if _, err := relayed.WriteTo([]byte("open"), public); err != nil {
		return at, fmt.Errorf("allocated %s; permission failed: %w", at, err)
	}
	payload := []byte("matrix relay check")
	buf := make([]byte, 1500)
	for try := 0; try < 3; try++ {
		time.Sleep(300 * time.Millisecond)
		if _, err := peer.WriteTo(payload, relayed.LocalAddr()); err != nil {
			return at, err
		}
		_ = relayed.SetReadDeadline(time.Now().Add(2 * time.Second))
		for {
			n, _, err := relayed.ReadFrom(buf)
			if err != nil {
				break
			}
			if bytes.Equal(buf[:n], payload) {
				return at, nil
			}
		}
	}
	return at, fmt.Errorf("allocated %s, but no data came through", at)
}

// mappedAddr is the public address of socket c as the relay's STUN service sees it.
func mappedAddr(c net.PacketConn, udpServer string) (net.Addr, error) {
	server, err := net.ResolveUDPAddr("udp4", udpServer)
	if err != nil {
		return nil, err
	}
	request := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	buf := make([]byte, 1500)
	for try := 0; try < 3; try++ {
		if _, err := c.WriteTo(request.Raw, server); err != nil {
			return nil, err
		}
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := c.ReadFrom(buf)
		if err != nil {
			continue
		}
		var response stun.Message
		response.Raw = append([]byte(nil), buf[:n]...)
		if response.Decode() != nil || response.TransactionID != request.TransactionID {
			continue
		}
		var mapped stun.XORMappedAddress
		if err := mapped.GetFrom(&response); err != nil {
			return nil, err
		}
		_ = c.SetReadDeadline(time.Time{})
		return &net.UDPAddr{IP: mapped.IP, Port: mapped.Port}, nil
	}
	return nil, fmt.Errorf("no STUN answer from %s", udpServer)
}

func main() {
	host := flag.String("host", "", "relay name (TLS certificate name)")
	ip := flag.String("ip", "", "relay address to connect to (default: resolve -host)")
	secretFile := flag.String("secret-file", "", "file holding the relay's shared secret")
	udpPort := flag.Int("udp", 3478, "UDP port (0: skip)")
	tcpPort := flag.Int("tcp", 80, "TCP port (0: skip)")
	tlsPort := flag.Int("tls", 443, "TLS port (0: skip)")
	name := flag.String("name", "relaycheck", "session id in the credential")
	flag.Parse()
	if *host == "" || *secretFile == "" {
		fmt.Fprintln(os.Stderr, "relaycheck: -host and -secret-file are required")
		os.Exit(2)
	}
	raw, err := os.ReadFile(*secretFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "relaycheck:", err)
		os.Exit(2)
	}
	secret := bytes.TrimSpace(raw)
	addr := *ip
	if addr == "" {
		ips, err := net.LookupHost(*host)
		if err != nil || len(ips) == 0 {
			fmt.Fprintln(os.Stderr, "relaycheck: cannot resolve", *host)
			os.Exit(2)
		}
		addr = ips[0]
	}
	udpServer := net.JoinHostPort(addr, strconv.Itoa(*udpPort))
	if *udpPort == 0 {
		udpServer = net.JoinHostPort(addr, "3478")
	}
	failed := false
	report := func(proto string, relayed string, err error) {
		if err != nil {
			failed = true
			fmt.Printf("FAIL %-4s %v\n", proto, err)
			return
		}
		fmt.Printf("PASS %-4s relay address %s, data looped through\n", proto, relayed)
	}
	if *udpPort != 0 {
		server := net.JoinHostPort(addr, strconv.Itoa(*udpPort))
		conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
		if err != nil {
			report("udp", "", err)
		} else {
			user, pass := credential(secret, *name)
			relayed, err := check(conn, server, udpServer, user, pass)
			report("udp", relayed, err)
		}
	}
	for _, p := range []struct {
		proto string
		port  int
	}{{"tcp", *tcpPort}, {"tls", *tlsPort}} {
		if p.port == 0 {
			continue
		}
		server := net.JoinHostPort(addr, strconv.Itoa(p.port))
		var c net.Conn
		if p.proto == "tls" {
			c, err = tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", server, &tls.Config{ServerName: *host})
		} else {
			c, err = net.DialTimeout("tcp", server, 5*time.Second)
		}
		if err != nil {
			report(p.proto, "", err)
			continue
		}
		user, pass := credential(secret, *name)
		relayed, err := check(turn.NewSTUNConn(c), server, udpServer, user, pass)
		report(p.proto, relayed, err)
	}
	if failed {
		os.Exit(1)
	}
}
