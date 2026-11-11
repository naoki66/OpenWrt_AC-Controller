package proto

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

const (
	ControlNonceHeader     = "X-AC-Nonce"
	ControlSignatureHeader = "X-AC-Signature"
	MaxHelloRequest        = 1 << 20
	MaxHelloResponse       = 1 << 20
	controlNonceSize       = 32
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

// controlMessage binds the exact response to the exact request and its fresh
// challenge. The domain separates these signatures from configuration bundles.
func controlMessage(nonce string, request, response []byte) []byte {
	reqHash := sha256.Sum256(request)
	resHash := sha256.Sum256(response)
	msg := []byte("ac-controller/hello-response/v1\x00")
	msg = append(msg, nonce...)
	msg = append(msg, reqHash[:]...)
	return append(msg, resHash[:]...)
}

func SignHelloReply(key ed25519.PrivateKey, nonce string, request, response []byte) (string, error) {
	if len(key) != ed25519.PrivateKeySize || !ValidControlNonce(nonce) {
		return "", errors.New("invalid controller signing key or nonce")
	}
	return hex.EncodeToString(ed25519.Sign(key, controlMessage(nonce, request, response))), nil
}

func VerifyHelloReply(key ed25519.PublicKey, signature, nonce string, request, response []byte) error {
	if len(key) != ed25519.PublicKeySize || !ValidControlNonce(nonce) || len(signature) != 2*ed25519.SignatureSize {
		return errors.New("missing or invalid controller response signature")
	}
	sig, err := hex.DecodeString(signature)
	if err != nil || !ed25519.Verify(key, controlMessage(nonce, request, response), sig) {
		return errors.New("controller response signature did not match the provisioned public key")
	}
	return nil
}
