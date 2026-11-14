package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/naoki66/ac-controller/shared/proto"
)

type controllerOverride struct {
	ID   string `json:"id"`
	Key  string `json:"public_key"`
	Addr string `json:"address"`
}

func (c Config) controllerOverridePath() string {
	return filepath.Join(c.stateDir(), "controller.json")
}

func validateReplacement(key, address string) error {
	raw, err := hex.DecodeString(key)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return errors.New("enter the new controller's 64-character Ed25519 public key")
	}
	if address != "" {
		u, err := url.Parse(withScheme(address))
		if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" ||
			u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return errors.New("enter a controller address as host:port, or leave it blank for discovery")
		}
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return errors.New("enter the controller's port (usually 8090)")
		}
	}
	return nil
}

// ResetManagement changes the controller trusted on the next agent start.
// Callers must stop the running agent first and hold its state lock.
func ResetManagement(cfg Config) error {
	cfg = Load(cfg)
	st, err := LoadState(cfg.statePath())
	if err != nil {
		return err
	}
	if st.PendingApply != "" || st.PendingConfirm != nil {
		return errors.New("wait for the pending network change to finish or roll back before switching controllers")
	}
	if st.FirmwareState == proto.FirmwareUpgrading {
		return errors.New("wait for the firmware upgrade to finish before switching controllers")
	}
	if st.ResetGeneration < 0 || st.ResetGeneration == math.MaxInt64 {
		return errors.New("invalid saved management reset generation")
	}
	id, err := newEnrollmentID()
	if err != nil {
		return err
	}
	return SaveState(cfg.statePath(), State{
		EnrollmentID: id, ResetGeneration: st.ResetGeneration + 1, ManagementReset: true,
	})
}

func newEnrollmentID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

func loadControllerOverride(cfg *Config, st *State) error {
	raw, err := os.ReadFile(cfg.controllerOverridePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var override controllerOverride
	if err := json.Unmarshal(raw, &override); err != nil || override.ID == "" {
		return errors.New("invalid local controller override")
	}
	if err := validateReplacement(override.Key, override.Addr); err != nil {
		return fmt.Errorf("invalid local controller override: %w", err)
	}
	cfg.overrideTrustKey = override.Key
	cfg.ACAddr = override.Addr
	// The old site's shared secret must not keep sending authenticated peer
	// announcements after the AP has been moved.
	cfg.Secret = ""
	if st.ControllerOverrideID != override.ID {
		// Controller revisions and queued results cannot cross this boundary.
		// Leave UCI, the AP's identity key and its backups alone.
		*st = State{ControllerOverrideID: override.ID, ResetGeneration: st.ResetGeneration}
	}
	return nil
}
