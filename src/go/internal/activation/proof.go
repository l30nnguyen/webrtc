// Package activation holds the SID-to-device bindings Center creates on first
// use, and the proof a device must give to create or reuse one.
package activation

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

const (
	// NonceSize is the size of a Center challenge: 8B unix time + 16B MAC.
	NonceSize = 24
	// ProofContext prefixes every signed message so a signature made for
	// activation can never be mistaken for another protocol's signature.
	ProofContext = "center-activate-v1"
)

// DeviceID is the identifier of a device: unpadded Base64URL of the SHA-256 of
// its Ed25519 public key (43 characters). It can not be claimed without the key.
func DeviceID(publicKey ed25519.PublicKey) string {
	sum := sha256.Sum256(publicKey)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Message builds the bytes a device signs to answer a challenge:
//
//	"center-activate-v1" || nonce(24) || SHA256(SID text)(32) || main origin
//
// mainOrigin is the exact WSS origin the device will connect to, so a proof can
// not be reused to obtain a ticket for a different MainServer.
func Message(nonce []byte, sidText, mainOrigin string) ([]byte, error) {
	if len(nonce) != NonceSize {
		return nil, errors.New("invalid challenge nonce size")
	}
	digest := sha256.Sum256([]byte(sidText))
	message := make([]byte, 0, len(ProofContext)+NonceSize+len(digest)+len(mainOrigin))
	message = append(message, ProofContext...)
	message = append(message, nonce...)
	message = append(message, digest[:]...)
	message = append(message, mainOrigin...)
	return message, nil
}

// Sign produces the device's proof. Devices implement the same thing natively;
// this exists for tests and tooling.
func Sign(privateKey ed25519.PrivateKey, nonce []byte, sidText, mainOrigin string) ([]byte, error) {
	message, err := Message(nonce, sidText, mainOrigin)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(privateKey, message), nil
}
