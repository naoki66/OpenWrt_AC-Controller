package secret

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
)

const EnrollmentPublicHeader = "X-AC-Enrollment-Public"

// EnrollmentKey is the controller's stable X25519 key, separate from its
// Ed25519 signing key. Keeping it tied to the signing seed makes it survive
// controller restarts and restores without distributing another credential.
func EnrollmentKey(signingKey ed25519.PrivateKey) (*ecdh.PrivateKey, error) {
	if len(signingKey) != ed25519.PrivateKeySize {
		return nil, errors.New("missing controller signing key")
	}
	raw, err := hex.DecodeString(derive(string(signingKey.Seed()), "ac-controller/enrollment-x25519/v1"))
	if err != nil {
		return nil, err
	}
	return ecdh.X25519().NewPrivateKey(raw)
}

func ParseEnrollmentPublic(value string) (*ecdh.PublicKey, error) {
	if len(value) != 64 {
		return nil, errors.New("invalid enrollment public key length")
	}
	raw, err := hex.DecodeString(value)
	if err != nil {
		return nil, err
	}
	return ecdh.X25519().NewPublicKey(raw)
}

func EnrollmentStatement(acID, public string) []byte {
	return []byte("ac-controller/enrollment-key/v1\x00" + acID + "\x00" + public)
}

// EnrollmentDeviceKey is the symmetric device credential used by the existing
// challenge, heartbeat and package protocol. The MAC is only a label.
func EnrollmentDeviceKey(private *ecdh.PrivateKey, peer *ecdh.PublicKey, mac, controller, device string) (string, error) {
	shared, err := private.ECDH(peer)
	if err != nil {
		return "", err
	}
	return derive(string(shared), "ac-controller/enrolled-device/v1\x00"+mac+"\x00"+controller+"\x00"+device), nil
}

// ManagementTrafficKey combines the one deployment key with the private
// enrollment exchange. Knowing the site key does not authenticate another AP.
func ManagementTrafficKey(deploymentKey, exchangeKey, enrollmentID string) string {
	if deploymentKey == "" || exchangeKey == "" || enrollmentID == "" {
		return ""
	}
	return derive(deploymentKey, "ac-controller/management-traffic/v1\x00"+exchangeKey+"\x00"+enrollmentID)
}
