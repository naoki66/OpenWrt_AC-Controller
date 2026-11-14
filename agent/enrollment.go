package agent

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/naoki66/ac-controller/shared/proto"
	"github.com/naoki66/ac-controller/shared/secret"
)

// FirmwareControllerKey can be injected into a site-specific agent build.
var FirmwareControllerKey string

func (c Config) controllerTrustKey() string {
	if c.overrideTrustKey != "" {
		return c.overrideTrustKey
	}
	if FirmwareControllerKey != "" {
		return strings.TrimSpace(FirmwareControllerKey)
	}
	if c.TrustedControllerKey != "" {
		return strings.TrimSpace(c.TrustedControllerKey)
	}
	raw, _ := os.ReadFile(filepath.Join(c.Root, "etc", "ac-agent", "controller.pub"))
	return strings.TrimSpace(string(raw))
}

func (a *Agent) pollUnboundControllers(ctx context.Context) error {
	if a.state.EnrollmentID == "" {
		id, err := newEnrollmentID()
		if err != nil {
			return err
		}
		a.state.EnrollmentID = id
		if err := a.saveState(); err != nil {
			a.state.EnrollmentID = ""
			return err
		}
	}
	var candidates []string
	if a.pinnedAC {
		candidates = append(candidates, a.Config.ACAddr)
	}
	if !a.pinnedAC || a.state.ManagementReset {
		key, err := a.enrollmentKey()
		if err != nil {
			return err
		}
		announce := discoveryAnnouncement(a.Identity, hex.EncodeToString(key.PublicKey().Bytes()))
		seen := map[string]bool{withScheme(a.Config.ACAddr): true}
		_, _, err = discoverControllers(ctx, a.Config, announce, a.Log, func(addr string) {
			if endpoint := withScheme(addr); !seen[endpoint] && len(candidates) < 8 {
				seen[endpoint] = true
				candidates = append(candidates, addr)
			}
		})
		if len(candidates) == 0 && err != nil {
			return err
		}
	}
	var lastErr error
	pending := ""
	var chosen *managementBinding
	var chosenAddr string
	var chosenClient *Client
	for _, addr := range candidates {
		a.client = a.newControlClient(addr)
		attempt, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := a.enroll(attempt, func(binding managementBinding) error {
			if chosen == nil {
				chosen, chosenAddr, chosenClient = &binding, addr, a.client
			}
			return nil
		})
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		pending = addr
	}
	// Finish registering with reachable ACs while still unbound. A claim
	// from one AC must not prevent the previous AC from seeing the reset.
	if chosen != nil {
		a.client = chosenClient
		if err := a.acceptManagementBinding(*chosen); err != nil {
			return err
		}
		a.acAddr = chosenAddr
		if a.state.ManagementReset && withScheme(chosenAddr) != withScheme(a.Config.ACAddr) {
			a.pinnedAC = false
		}
		a.Config.overrideTrustKey = a.state.PubKey
		return nil
	}
	a.client = nil
	if pending != "" {
		a.acAddr = pending
		return nil
	}
	if a.pinnedAC {
		a.acAddr = a.Config.ACAddr
		a.client = a.newControlClient(a.acAddr)
		return lastErr
	}
	a.acAddr = ""
	return lastErr
}

func (a *Agent) enrollmentKey() (*ecdh.PrivateKey, error) {
	path := filepath.Join(a.Config.stateDir(), "enrollment.key")
	raw, err := os.ReadFile(path)
	if err == nil {
		key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil {
			return nil, fmt.Errorf("decode AP enrollment key: %w", err)
		}
		return ecdh.X25519().NewPrivateKey(key)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := writeAll(map[string]string{path: hex.EncodeToString(key.Bytes()) + "\n"}); err != nil {
		return nil, fmt.Errorf("save AP enrollment key: %w", err)
	}
	return key, nil
}

type managementBinding struct {
	claim      proto.ManagementClaim
	trafficKey string
	trusted    ed25519.PublicKey
}

func (a *Agent) acceptManagementBinding(b managementBinding) error {
	before := a.state
	a.state.ACID, a.state.PubKey, a.state.ManagementKey = b.claim.ACID, b.claim.SigningKey, b.claim.Credential
	a.state.ManagementTrafficKey = b.trafficKey
	a.state.ManagementAddr = a.client.BaseURL
	if err := a.saveState(); err != nil {
		a.state = before
		return err
	}
	a.trustedController = b.trusted
	a.client.deviceKey, a.client.enrollmentACID = b.trafficKey, b.claim.ACID
	return nil
}

func (a *Agent) enroll(ctx context.Context, onClaim func(managementBinding) error) error {
	c := a.client
	url := c.BaseURL + "/api/v1/ap/enroll"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("get controller enrollment key: %w", err)
	}
	var info proto.EnrollmentInfo
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2049))
	resp.Body.Close()
	if readErr != nil || resp.StatusCode != http.StatusOK || len(body) > 2048 || json.Unmarshal(body, &info) != nil {
		return fmt.Errorf("invalid controller enrollment response (HTTP %d)", resp.StatusCode)
	}
	sig, err := hex.DecodeString(info.Signature)
	if err != nil {
		return errors.New("invalid controller enrollment signature")
	}
	trusted := a.trustedController
	if len(trusted) == 0 {
		trusted, err = hex.DecodeString(info.SigningKey)
	}
	if err != nil || len(trusted) != ed25519.PublicKeySize || info.ACID == "" || !ed25519.Verify(trusted, secret.EnrollmentStatement(info.ACID, info.Public), sig) {
		return errors.New("controller enrollment signature does not match the firmware trust key")
	}
	peer, err := secret.ParseEnrollmentPublic(info.Public)
	if err != nil {
		return err
	}
	private, err := a.enrollmentKey()
	if err != nil {
		return err
	}
	public := hex.EncodeToString(private.PublicKey().Bytes())
	key, err := secret.EnrollmentDeviceKey(private, peer, c.deviceMAC, info.Public, public)
	if err != nil {
		return err
	}
	registration := discoveryAnnouncement(a.Identity, public)
	registration.EnrollmentID = a.state.EnrollmentID
	registration.ResetGeneration = a.state.ResetGeneration
	body, err = json.Marshal(registration)
	if err != nil {
		return err
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(secret.EnrollmentPublicHeader, public)
	enrolling := *c
	enrolling.deviceKey = key
	challenge, err := enrolling.authorize(ctx, req, body)
	if err != nil {
		return err
	}
	resp, err = c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("register AP identity: %w", err)
	}
	defer resp.Body.Close()
	if a.state.EnrollmentID != "" {
		if resp.StatusCode == http.StatusNoContent {
			return nil // Still waiting for an administrator to adopt.
		}
		if resp.StatusCode == http.StatusOK {
			sealed, err := io.ReadAll(io.LimitReader(resp.Body, 2049))
			if err != nil || len(sealed) > 2048 || resp.Header.Get("Content-Type") != secret.DevicePackageType {
				return errors.New("invalid controller claim response")
			}
			payload, err := secret.OpenDevicePackage(key, challenge, sealed)
			if err != nil {
				return err
			}
			var claim proto.ManagementClaim
			if json.Unmarshal(payload, &claim) != nil || claim.ACID != info.ACID || claim.SigningKey != info.SigningKey {
				return errors.New("invalid controller claim identity")
			}
			raw, err := hex.DecodeString(claim.Credential)
			if err != nil || len(raw) != 32 {
				return errors.New("invalid management credential")
			}
			return onClaim(managementBinding{
				claim: claim, trusted: trusted,
				trafficKey: secret.ManagementTrafficKey(claim.Credential, key, a.state.EnrollmentID),
			})
		}
	}
	if resp.StatusCode != http.StatusNoContent {
		if resp.StatusCode == http.StatusConflict {
			return errors.New("this MAC is already registered to another AP identity on the controller")
		}
		return fmt.Errorf("register AP identity: HTTP %d", resp.StatusCode)
	}
	c.deviceKey = key
	c.enrollmentACID = info.ACID
	return nil
}
