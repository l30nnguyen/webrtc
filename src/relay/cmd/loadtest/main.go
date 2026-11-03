// Command loadtest loads a relay for the capacity test (tools/capacity):
// viewing sessions sending camera media through it, with the viewer side
// relayed (the camera sends to the viewer's relay address) or both sides.
// Run it against a test relay with -allow-private-peers and a fixed user.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/logging"
	"github.com/pion/turn/v5"
)

func allocate(server, user, password string) net.PacketConn {
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	logger := logging.NewDefaultLoggerFactory()
	logger.DefaultLogLevel = logging.LogLevelError
	client, err := turn.NewClient(&turn.ClientConfig{STUNServerAddr: server, TURNServerAddr: server, Conn: conn,
		Username: user, Password: password, Realm: "matrix", LoggerFactory: logger})
	if err != nil {
		panic(err)
	}
	if err := client.Listen(); err != nil {
		panic(err)
	}
	relayed, err := client.Allocate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "allocate:", err)
		os.Exit(1)
	}
	return relayed
}

func main() {
	server := flag.String("server", "127.0.0.1:34800", "relay")
	user := flag.String("user", "load", "fixed user")
	password := flag.String("password", "loadpass", "its password")
	sessions := flag.Int("sessions", 10, "viewing sessions")
	pps := flag.Int("pps", 170, "1200-byte packets per second per session, camera to viewer (170 is about 1.6 Mbps)")
	both := flag.Bool("both", false, "relay the camera side too")
	dur := flag.Duration("dur", 20*time.Second, "how long")
	hold := flag.Bool("hold", false, "only allocate and wait (memory per allocation)")
	flag.Parse()
	var sent, received atomic.Int64
	var wg, setup sync.WaitGroup
	payload := make([]byte, 1200)
	ready := make(chan struct{})
	for i := 0; i < *sessions; i++ {
		setup.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			viewer := allocate(*server, *user, *password)
			var camera net.PacketConn
			if *both {
				camera = allocate(*server, *user, *password)
			} else {
				camera, _ = net.ListenPacket("udp4", "127.0.0.1:0")
			}
			// Permissions: the viewer's for the camera, and the camera's for the viewer.
			viewer.WriteTo([]byte("hi"), camera.LocalAddr())
			if *both {
				camera.WriteTo([]byte("hi"), viewer.LocalAddr())
			}
			time.Sleep(200 * time.Millisecond)
			setup.Done()
			<-ready
			if *hold {
				time.Sleep(*dur)
				return
			}
			go func() {
				buf := make([]byte, 1500)
				for {
					n, _, err := viewer.ReadFrom(buf)
					if err != nil {
						return
					}
					if n == 1200 {
						received.Add(1)
					}
				}
			}()
			interval := time.Second / time.Duration(*pps)
			next, end := time.Now(), time.Now().Add(*dur)
			for time.Now().Before(end) {
				camera.WriteTo(payload, viewer.LocalAddr())
				sent.Add(1)
				next = next.Add(interval)
				if d := time.Until(next); d > 0 {
					time.Sleep(d)
				}
			}
			time.Sleep(300 * time.Millisecond)
			viewer.Close()
			camera.Close()
		}()
	}
	setup.Wait()
	fmt.Println("ready")
	close(ready)
	start := time.Now()
	wg.Wait()
	secs := time.Since(start).Seconds()
	fmt.Printf("sent %.0f pps, received %.0f pps (%.2f%% lost)\n", float64(sent.Load())/secs, float64(received.Load())/secs,
		100*float64(sent.Load()-received.Load())/float64(max(sent.Load(), 1)))
}
