package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"runtime"
	"time"
)

func rate(name string, d time.Duration, op func()) {
	n, start := 0, time.Now()
	for time.Since(start) < d {
		op()
		n++
	}
	fmt.Printf("%-28s %12.0f /s\n", name, float64(n)/time.Since(start).Seconds())
}

func main() {
	runtime.GOMAXPROCS(1)
	d := time.Second
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	digest := sha256.Sum256([]byte("x"))
	rate("ecdsa-p256 sign", d, func() { ecdsa.SignASN1(rand.Reader, key, digest[:]) })
	priv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	peer, _ := ecdh.X25519().GenerateKey(rand.Reader)
	rate("x25519 ecdh", d, func() { priv.ECDH(peer.PublicKey()) })
	block, _ := aes.NewCipher(make([]byte, 16))
	gcm, _ := cipher.NewGCM(block)
	buf, nonce := make([]byte, 1200), make([]byte, 12)
	out := make([]byte, 0, 1300)
	rate("aes-gcm seal 1200B", d, func() { out = gcm.Seal(out[:0], nonce, buf, nil) })
	msg := map[string]any{"type": "offer", "id": "Player123", "sdp": string(make([]byte, 3000))}
	rate("json marshal+unmarshal 3KB", d, func() {
		b, _ := json.Marshal(msg)
		var m map[string]any
		json.Unmarshal(b, &m)
	})
	a, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	c, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	rbuf := make([]byte, 1500)
	rate("udp send+recv 1200B", d, func() {
		a.WriteTo(buf, c.LocalAddr())
		c.ReadFrom(rbuf)
	})
}
