// Package keys manages the controller's Ed25519 signing identity.
//
// The AP verifies every configuration package it applies, so the controller
// must hold a stable private key: regenerating it would invalidate every
// provisioned AP at once. The key therefore lives next to the SQLite database
// and is expected to be included in backups (design §8.2, §11).
package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileName is the private key file inside the data directory.
const FileName = "ac_signing.key"

// Key is a signing identity.
type Key struct {
	Private ed25519.PrivateKey
	Public  ed25519.PublicKey
}

// PublicHex renders the public key for AP enrolment and diagnostics.
func (k Key) PublicHex() string { return hex.EncodeToString(k.Public) }

// LoadOrCreate reads the signing key, generating one on first boot.
func LoadOrCreate(dir string) (Key, error) {
	k, err := Load(dir)
	if err == nil {
		return k, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Key{}, err
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Key{}, fmt.Errorf("generate signing key: %w", err)
	}

	path := filepath.Join(dir, FileName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Key{}, fmt.Errorf("create data dir: %w", err)
	}
	// 0600: the private key is the only thing that makes a package trustworthy.
	if err := os.WriteFile(path, []byte(hex.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		return Key{}, fmt.Errorf("write signing key: %w", err)
	}
	return Key{Private: priv, Public: pub}, nil
}

// Load reads an existing signing key. It returns os.ErrNotExist wrapped when no
// key has been created yet.
func Load(dir string) (Key, error) {
	path := filepath.Join(dir, FileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return Key{}, err // already wraps os.ErrNotExist
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return Key{}, fmt.Errorf("decode signing key %s: %w", path, err)
	}
	if len(seed) != ed25519.SeedSize {
		return Key{}, fmt.Errorf("signing key %s has %d bytes, want %d", path, len(seed), ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return Key{Private: priv, Public: priv.Public().(ed25519.PublicKey)}, nil
}

// FromSeed builds a key from a fixed seed. Tests use it to keep fixtures
// reproducible; production keys come from LoadOrCreate.
func FromSeed(seed []byte) (Key, error) {
	if len(seed) != ed25519.SeedSize {
		return Key{}, fmt.Errorf("seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return Key{Private: priv, Public: priv.Public().(ed25519.PublicKey)}, nil
}
