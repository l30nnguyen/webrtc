package main

import (
	"crypto/tls"
	"net"
	"os"
	"sync"
	"time"
)

// certificate rereads the certificate files when they change, so a renewed
// certificate is served without a restart.
type certificate struct {
	certFile, keyFile string

	mu      sync.Mutex
	current *tls.Certificate
	modTime time.Time
	checked time.Time
}

func (c *certificate) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current != nil && time.Since(c.checked) < time.Minute {
		return c.current, nil
	}
	c.checked = time.Now()
	info, err := os.Stat(c.certFile)
	if err != nil {
		if c.current != nil {
			return c.current, nil
		}
		return nil, err
	}
	if c.current != nil && info.ModTime().Equal(c.modTime) {
		return c.current, nil
	}
	pair, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		if c.current != nil {
			return c.current, nil
		}
		return nil, err
	}
	c.current, c.modTime = &pair, info.ModTime()
	return c.current, nil
}

func listenTLS(address string, c *certificate) (net.Listener, error) {
	return tls.Listen("tcp4", address, &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: c.get})
}
