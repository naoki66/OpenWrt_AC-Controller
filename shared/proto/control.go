package proto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

const (
	ControlNonceHeader      = "X-AC-Nonce"
	ControlReplyProofHeader = "X-AC-Reply-Proof"
	MaxHelloRequest         = 1 << 20
	MaxHelloResponse        = 1 << 20
	controlNonceSize        = 32
)

func NewControlNonce() (string, error) {
	var nonce [controlNonceSize]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(nonce[:]), nil
}

func ValidControlNonce(s string) bool {
	if len(s) != 2*controlNonceSize {
		return false
	}
	var nonce [controlNonceSize]byte
	_, err := hex.Decode(nonce[:], []byte(s))
	return err == nil
}

// HelloReplyProof authenticates the controller response, including its bundle
// signing key, before the AP uses any fields in it. Its domain is separate
// from device request proofs, and the AP supplies a fresh nonce every time.
func HelloReplyProof(key, challenge, nonce string, request, response []byte) string {
	reqHash := sha256.Sum256(request)
	resHash := sha256.Sum256(response)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte("ac-controller/site-hello-response/v1\x00"))
	mac.Write([]byte(challenge))
	mac.Write([]byte{0})
	mac.Write([]byte(nonce))
	mac.Write(reqHash[:])
	mac.Write(resHash[:])
	return hex.EncodeToString(mac.Sum(nil))
}

func VerifyHelloReply(key, proof, challenge, nonce string, request, response []byte) error {
	if key == "" || challenge == "" || !ValidControlNonce(nonce) ||
		!hmac.Equal([]byte(proof), []byte(HelloReplyProof(key, challenge, nonce, request, response))) {
		return errors.New("controller response authentication failed (check the shared secret)")
	}
	return nil
}
