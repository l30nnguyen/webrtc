package center

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"time"

	"signaling-server/internal/activation"
)

const (
	// ChallengeLifetime is how long an issued nonce can be answered.
	ChallengeLifetime = 30 * time.Second
	challengeContext  = "center-challenge-v1"

	nonceTimeSize   = 4
	nonceRandomSize = 10
	nonceMACSize    = activation.NonceSize - nonceTimeSize - nonceRandomSize // 10
)

// newNonce returns an unforgeable challenge:
//
//	4B unix time (BE) || 10B random || 10B HMAC-SHA256(key, context || time || random)
//
// Center keeps no per-challenge state. The random part makes every challenge
// unique, so two devices asking in the same second never share a nonce, and
// single use is enforced separately by a replay cache keyed on the nonce.
func newNonce(key []byte, now time.Time) []byte {
	nonce := make([]byte, activation.NonceSize)
	binary.BigEndian.PutUint32(nonce[:nonceTimeSize], uint32(now.Unix()))
	if _, err := rand.Read(nonce[nonceTimeSize : nonceTimeSize+nonceRandomSize]); err != nil {
		panic("center: no randomness available: " + err.Error())
	}
	copy(nonce[nonceTimeSize+nonceRandomSize:], nonceMAC(key, nonce[:nonceTimeSize+nonceRandomSize]))
	return nonce
}

func nonceMAC(key, body []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(challengeContext))
	mac.Write(body)
	return mac.Sum(nil)[:nonceMACSize]
}

// checkNonce reports whether nonce was issued by this Center and is still
// fresh. It returns the unix time until which the replay cache must remember it.
func checkNonce(key, nonce []byte, now time.Time) (int64, bool) {
	body := nonceTimeSize + nonceRandomSize
	if len(nonce) != activation.NonceSize || !hmac.Equal(nonce[body:], nonceMAC(key, nonce[:body])) {
		return 0, false
	}
	issued := int64(binary.BigEndian.Uint32(nonce[:nonceTimeSize]))
	if issued > now.Unix()+2 || now.Unix()-issued > int64(ChallengeLifetime.Seconds()) {
		return 0, false
	}
	return issued + int64(ChallengeLifetime.Seconds()) + 5, true // margin so the replay entry outlives the nonce
}
