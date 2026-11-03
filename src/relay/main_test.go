package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/turn/v5"
)

const testSecret = "0123456789abcdef-test"

// mint is the control server's relay.Credentials.
func mint(id string, expiry time.Time) (string, string) {
	user := strconv.FormatInt(expiry.Unix(), 10) + ":" + id
	mac := hmac.New(sha1.New, []byte(testSecret))
	mac.Write([]byte(user))
	return user, base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func writeSecrets(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "secrets.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func writeCert(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "relay.test"},
		DNSNames:     []string{"relay.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	return certFile, keyFile
}

func startTest(t *testing.T, allowPrivate bool) (*relay, options) {
	t.Helper()
	dir := t.TempDir()
	certFile, keyFile := writeCert(t, dir)
	o := options{
		publicIP:     "127.0.0.1",
		udpListen:    "127.0.0.1:" + strconv.Itoa(freePort(t)),
		tcpListen:    "127.0.0.1:" + strconv.Itoa(freePort(t)),
		tlsListen:    "127.0.0.1:" + strconv.Itoa(freePort(t)),
		certFile:     certFile,
		keyFile:      keyFile,
		realm:        "matrix",
		secretsFile:  writeSecrets(t, dir, `{"shared": "`+testSecret+`", "users": {"matrix": "fixed-pass"}}`),
		relayMin:     40000,
		relayMax:     40999,
		maxPerUser:   2,
		allowPrivate: allowPrivate,
	}
	logger := logging.NewDefaultLoggerFactory()
	logger.DefaultLogLevel = logging.LogLevelError
	r, err := start(o, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close)
	return r, o
}

// dial connects a TURN client over udp, tcp or tls.
func dial(t *testing.T, o options, transport, user, password string) *turn.Client {
	t.Helper()
	var conn net.PacketConn
	switch transport {
	case "udp":
		c, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		conn = c
	case "tcp":
		c, err := net.Dial("tcp4", o.tcpListen)
		if err != nil {
			t.Fatal(err)
		}
		conn = turn.NewSTUNConn(c)
	case "tls":
		c, err := tls.Dial("tcp4", o.tlsListen, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test certificate
		if err != nil {
			t.Fatal(err)
		}
		conn = turn.NewSTUNConn(c)
	}
	server := map[string]string{"udp": o.udpListen, "tcp": o.tcpListen, "tls": o.tlsListen}[transport]
	logger := logging.NewDefaultLoggerFactory()
	logger.DefaultLogLevel = logging.LogLevelError
	client, err := turn.NewClient(&turn.ClientConfig{
		STUNServerAddr: server,
		TURNServerAddr: server,
		Conn:           conn,
		Username:       user,
		Password:       password,
		Realm:          "matrix",
		LoggerFactory:  logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Listen(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); conn.Close() })
	return client
}

// TestRelaysBetweenTransports runs the real case: a camera allocates over
// UDP, viewers over TCP and TLS, and media goes both ways through the relay.
func TestRelaysBetweenTransports(t *testing.T) {
	r, o := startTest(t, true)
	user, password := mint("camera-1", time.Now().Add(24*time.Hour))
	camera, err := dial(t, o, "udp", user, password).Allocate()
	if err != nil {
		t.Fatalf("camera allocation over udp: %v", err)
	}
	for _, transport := range []string{"tcp", "tls"} {
		t.Run(transport, func(t *testing.T) {
			viewerUser, viewerPassword := mint("camera-1", time.Now().Add(time.Hour))
			viewer, err := dial(t, o, transport, viewerUser, viewerPassword).Allocate()
			if err != nil {
				t.Fatalf("viewer allocation: %v", err)
			}
			defer viewer.Close()
			// Each side writes first, which installs its permission for the other.
			for i := 0; i < 3; i++ {
				camera.WriteTo([]byte("hello"), viewer.LocalAddr())
				viewer.WriteTo([]byte("hi"), camera.LocalAddr())
				time.Sleep(50 * time.Millisecond)
			}
			camera.WriteTo([]byte("video"), viewer.LocalAddr())
			viewer.SetReadDeadline(time.Now().Add(3 * time.Second))
			buf := make([]byte, 1500)
			for {
				n, from, err := viewer.ReadFrom(buf)
				if err != nil {
					t.Fatalf("viewer got nothing from the camera: %v", err)
				}
				if string(buf[:n]) == "video" && from.String() == camera.LocalAddr().String() {
					break
				}
			}
		})
	}
	if r.stats.rxBytes.Load() == 0 || r.stats.txBytes.Load() == 0 {
		t.Fatalf("traffic not counted: rx %d tx %d", r.stats.rxBytes.Load(), r.stats.txBytes.Load())
	}
	// A closed allocation is removed when its close reaches the server.
	for deadline := time.Now().Add(3 * time.Second); r.stats.allocations.Load() != 1 && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	w := httptest.NewRecorder()
	r.stats.handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	for _, want := range []string{"turn_total_allocations 1\n", `matrix_relay_allocations{transport="udp"} 1`, "turn_total_traffic_rcvb ", "matrix_relay_allocations_total 3\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q:\n%s", want, body)
		}
	}
}

func TestCredentials(t *testing.T) {
	_, o := startTest(t, true)
	// Violet's fixed credentials still work, for a drop-in switch.
	if _, err := dial(t, o, "udp", "matrix", "fixed-pass").Allocate(); err != nil {
		t.Fatalf("fixed user: %v", err)
	}
	expired, password := mint("camera-2", time.Now().Add(-time.Minute))
	if _, err := dial(t, o, "udp", expired, password).Allocate(); err == nil {
		t.Fatal("expired credentials allocated")
	}
	tooLong, password := mint("camera-2", time.Now().Add(72*time.Hour))
	if _, err := dial(t, o, "udp", tooLong, password).Allocate(); err == nil {
		t.Fatal("credentials expiring beyond the limit allocated")
	}
	user, _ := mint("camera-2", time.Now().Add(time.Hour))
	if _, err := dial(t, o, "tcp", user, "wrong").Allocate(); err == nil {
		t.Fatal("a wrong password allocated")
	}
}

func TestQuotaPerCamera(t *testing.T) {
	r, o := startTest(t, true)
	for i := 0; i < 2; i++ {
		user, password := mint("camera-3", time.Now().Add(time.Duration(i+1)*time.Hour))
		if _, err := dial(t, o, "udp", user, password).Allocate(); err != nil {
			t.Fatalf("allocation %d: %v", i, err)
		}
	}
	user, password := mint("camera-3", time.Now().Add(3*time.Hour))
	if _, err := dial(t, o, "udp", user, password).Allocate(); err == nil {
		t.Fatal("a third allocation for one camera was allowed")
	}
	other, password := mint("camera-4", time.Now().Add(time.Hour))
	if _, err := dial(t, o, "udp", other, password).Allocate(); err != nil {
		t.Fatalf("another camera: %v", err)
	}
	// A fixed user is shared by many sessions: only the total limits it.
	for i := 0; i < 3; i++ {
		if _, err := dial(t, o, "udp", "matrix", "fixed-pass").Allocate(); err != nil {
			t.Fatalf("fixed user allocation %d: %v", i, err)
		}
	}
	if r.stats.quotaRejects.Load() != 1 {
		t.Fatalf("quota rejects %d, want 1", r.stats.quotaRejects.Load())
	}
}

func TestPeerFilter(t *testing.T) {
	st := newStats(0)
	permit := peerFilter(false, st)
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "172.31.27.147", "192.168.80.166", "169.254.169.254", "0.0.0.0", "224.0.0.1"} {
		if permit(nil, net.ParseIP(ip)) {
			t.Errorf("%s permitted", ip)
		}
	}
	for _, ip := range []string{"3.95.228.203", "115.78.12.186"} {
		if !permit(nil, net.ParseIP(ip)) {
			t.Errorf("%s refused", ip)
		}
	}
}

func TestFlags(t *testing.T) {
	o, err := parseFlags([]string{"-public-ip", "3.95.228.203", "-secrets", "s.json", "-cert", "c.pem", "-key", "k.pem"})
	if err != nil || o.udpListen != "0.0.0.0:3478" || o.tcpListen != "0.0.0.0:80" || o.tlsListen != "0.0.0.0:443" {
		t.Fatalf("defaults %+v %v", o, err)
	}
	// Without a certificate there is no TLS.
	if o, err := parseFlags([]string{"-public-ip", "3.95.228.203", "-secrets", "s.json"}); err != nil || o.tlsListen != "" {
		t.Fatalf("no certificate: %+v %v", o, err)
	}
	if _, err := parseFlags([]string{"-public-ip", "3.95.228.203", "-secrets", "s.json", "-udp", "", "-tcp", ""}); err == nil {
		t.Fatal("no listener accepted")
	}
	if _, err := parseFlags([]string{"-public-ip", "3.95.228.203", "-secrets", "s.json", "-cert", "c.pem"}); err == nil {
		t.Fatal("-cert without -key accepted")
	}
}

func TestSecretsFile(t *testing.T) {
	dir := t.TempDir()
	path := writeSecrets(t, dir, `{"shared": "short"}`)
	if _, err := loadSecrets(path); err == nil {
		t.Error("short secret accepted")
	}
	path = writeSecrets(t, dir, `{"shared": "`+testSecret+`", "extra": 1}`)
	if _, err := loadSecrets(path); err == nil {
		t.Error("unknown field accepted")
	}
	path = writeSecrets(t, dir, `{"shared": "`+testSecret+`"}`)
	os.Chmod(path, 0o644)
	if _, err := loadSecrets(path); err == nil {
		t.Error("group-readable file accepted")
	}
}

func TestTLSCertificateReload(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeCert(t, dir)
	c := &certificate{certFile: certFile, keyFile: keyFile}
	first, err := c.get(nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	writeCert(t, dir)
	c.checked = time.Time{}
	second, _ := c.get(nil)
	if first == second {
		t.Fatal("renewed certificate not loaded")
	}
}
