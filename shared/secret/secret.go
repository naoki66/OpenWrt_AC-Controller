// Package secret derives the per-purpose credentials the controller and the
// agents share from a single site secret. DeviceKey is used for authenticated
// controller traffic, and PeerKey authenticates peer roaming messages.
package secret

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"hash"
)

// Domain labels separate the derivation contexts. They are not secret; they
// exist so the two keys can never collide.
const (
	domainAuth = "ac-controller/ap-auth/v1"
	domainPeer = "ac-controller/ap-peer/v1"
)

// hmacSHA256 returns a fresh HMAC-SHA256 hasher keyed by key.
func hmacSHA256(key []byte) hash.Hash {
	return hmac.New(sha256.New, key)
}

// derive runs HKDF-SHA256 with an empty salt and the given info label. With a
// single purpose there is no extract-then-expand distinction worth modelling:
// HMAC(key, info || 0x01) is equivalent to the first expand block and is what
// both callers share.
func derive(secret string, info string) string {
	mac := hmacSHA256([]byte(secret))
	mac.Write([]byte(info))
	mac.Write([]byte{0x01})
	return hex.EncodeToString(mac.Sum(nil))
}

// AuthToken derives the legacy bearer token. Production AP traffic uses
// challenge-response authentication instead.
func AuthToken(secret string) string {
	return derive(secret, domainAuth)
}

// PeerKey is the HMAC-SHA256 key APs use to sign and verify peer-state
// messages. Distinct from AuthToken so a compromised peer bus cannot leak the
// controller credential.
func PeerKey(secret string) string {
	return derive(secret, domainPeer)
}
