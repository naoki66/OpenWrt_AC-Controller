package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
)

const (
	DeviceMACHeader       = "X-AC-Device"
	DeviceChallengeHeader = "X-AC-Device-Challenge"
	DeviceProofHeader     = "X-AC-Device-Proof"
	DevicePackageType     = "application/vnd.ac-controller.encrypted-package"
)

// DeviceKey derives a per-MAC control credential from the site's shared secret.
// Both sides use the canonical MAC; the derived key is never sent or stored.
func DeviceKey(shared, mac string) string {
	if len(shared) < 12 || mac == "" {
		return ""
	}
	return derive(shared, "ac-controller/site-device/v1\x00"+mac)
}

func DeviceProof(key, device, challenge, method, target string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(derive(key, "ac-controller/device-request/v1")))
	for _, field := range []string{device, challenge, method, target} {
		io.WriteString(mac, field)
		mac.Write([]byte{0})
	}
	sum := sha256.Sum256(body)
	mac.Write(sum[:])
	return hex.EncodeToString(mac.Sum(nil))
}

func devicePackageCipher(key string) (cipher.AEAD, error) {
	raw, err := hex.DecodeString(derive(key, "ac-controller/device-package/v1"))
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

func SealDevicePackage(key, challenge string, payload []byte) ([]byte, error) {
	gcm, err := devicePackageCipher(key)
	if err != nil {
		return nil, err
	}
	return gcm.Seal(nil, nil, payload, []byte(challenge)), nil
}

func OpenDevicePackage(key, challenge string, ciphertext []byte) ([]byte, error) {
	gcm, err := devicePackageCipher(key)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nil, ciphertext, []byte(challenge))
}
