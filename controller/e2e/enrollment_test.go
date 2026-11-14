package e2e

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naoki66/ac-controller/agent"
	"github.com/naoki66/ac-controller/controller/internal/keys"
	"github.com/naoki66/ac-controller/controller/internal/store"
	"github.com/naoki66/ac-controller/shared/secret"
)

func TestFirmwarePinEnrollment(t *testing.T) {
	t.Setenv("AC_SECRET", "")
	ac := newACWithSecret(t, "")
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg, runner, root := newAgentConfig(t, ac)

	wrong, err := keys.FromSeed(bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	wrongCfg := cfg
	wrongCfg.TrustedControllerKey = wrong.PublicHex()
	untrusted, err := agent.New(wrongCfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := untrusted.Step(ctx); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("wrong firmware pin: %v", err)
	}
	if _, err := ac.store.GetDevice(ctx, apMAC); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("wrong pin created a pending device: %v", err)
	}

	pin := filepath.Join(root, "etc", "ac-agent", "controller.pub")
	if err := os.MkdirAll(filepath.Dir(pin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pin, []byte(ac.key.PublicHex()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Step(ctx); err != nil {
		t.Fatalf("register without a shared secret: %v", err)
	}
	if d := ac.device(t, apMAC); d.State != store.StatePending || d.ConfigRev != 0 {
		t.Fatalf("device before approval = %s, rev %d", d.State, d.ConfigRev)
	}
	if m := mutatingCalls(runner.calls); len(m) != 0 {
		t.Fatalf("mutations before approval: %v", m)
	}
	otherCfg, _, _ := newAgentConfig(t, ac)
	otherCfg.TrustedControllerKey = ac.key.PublicHex()
	other, err := agent.New(otherCfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Step(ctx); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("another device key claimed an existing MAC: %v", err)
	}

	resp := ac.post("/api/v1/devices/"+apMAC+"/adopt", `{"template_id":"default"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve: HTTP %d", resp.StatusCode)
	}
	// A restarted agent must retain its identity and be able to fetch and
	// verify the approved package without a new approval.
	a, err = agent.New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Step(ctx); err != nil {
		t.Fatalf("apply approved configuration: %v", err)
	}
	if a.State().AppliedRev != 1 || a.State().PubKey != ac.key.PublicHex() {
		t.Fatalf("agent applied revision %d using %q", a.State().AppliedRev, a.State().PubKey)
	}

	resp = ac.post("/api/v1/devices/"+apMAC+"/release", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release: HTTP %d", resp.StatusCode)
	}
	if err := a.Step(ctx); err != nil {
		t.Fatalf("heartbeat after release: %v", err)
	}
	if d := ac.device(t, apMAC); d.State != store.StatePending {
		t.Fatalf("device after release = %s", d.State)
	}
	raw, err := os.ReadFile(filepath.Join(root, "etc", "ac", "enrollment.key"))
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	private, err := ecdh.X25519().NewPrivateKey(keyBytes)
	if err != nil {
		t.Fatal(err)
	}
	controllerKey, err := secret.EnrollmentKey(ac.key.Private)
	if err != nil {
		t.Fatal(err)
	}
	deviceKey, err := secret.EnrollmentDeviceKey(private, controllerKey.PublicKey(), apMAC,
		hex.EncodeToString(controllerKey.PublicKey().Bytes()), hex.EncodeToString(private.PublicKey().Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	resp, err = ac.server.Client().Get(ac.server.URL + "/api/v1/ap/challenge")
	if err != nil {
		t.Fatal(err)
	}
	challengeBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("challenge: HTTP %d, %v", resp.StatusCode, err)
	}
	challenge := string(challengeBody)
	req, err := http.NewRequest(http.MethodGet, ac.server.URL+"/api/v1/devices/"+apMAC+"/package?rev=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(secret.DeviceMACHeader, apMAC)
	req.Header.Set(secret.DeviceChallengeHeader, challenge)
	req.Header.Set(secret.DeviceProofHeader, secret.DeviceProof(deviceKey, apMAC, challenge, req.Method, req.URL.RequestURI(), nil))
	resp, err = ac.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("released AP fetched an older configuration: HTTP %d", resp.StatusCode)
	}
}

func TestSwitchControllerAndReadopt(t *testing.T) {
	t.Setenv("AC_SECRET", "")
	oldAC := newACWithSecret(t, "")
	newAC := newACWithIdentity(t, "", 37, "new-site")
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg, runner, root := newAgentConfig(t, oldAC)
	start := func() *agent.Agent {
		t.Helper()
		a, err := agent.New(cfg, log)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Step(ctx); err != nil {
			t.Fatal(err)
		}
		return a
	}
	start()
	if d := oldAC.device(t, apMAC); d.State != store.StatePending {
		t.Fatalf("unconfigured AP should await approval: %s", d.State)
	}
	oldAC.adopt(t, apMAC)
	old := start()
	if old.State().AppliedRev != 1 {
		t.Fatal("old AC did not apply revision 1")
	}
	if old.State().ManagementKey == "" {
		t.Fatal("adoption did not persist a management credential")
	}
	if d := oldAC.device(t, apMAC); d.AgentVersion == "" {
		t.Fatal("agent did not report its build version")
	}
	secondCfg, _, _ := newAgentConfig(t, oldAC)
	secondCfg.Identity.MAC = "02:00:00:00:00:03"
	second, err := agent.New(secondCfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Step(ctx); err != nil {
		t.Fatal(err)
	}
	oldAC.adopt(t, secondCfg.Identity.MAC)
	if err := second.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if old.State().ManagementKey != second.State().ManagementKey {
		t.Fatal("the same AC issued different deployment keys")
	}
	if old.State().ManagementTrafficKey == "" || old.State().ManagementTrafficKey == second.State().ManagementTrafficKey {
		t.Fatal("two APs can authenticate with the same traffic key")
	}
	if err := oldAC.store.EnsureManagementKey(ctx); err != nil {
		t.Fatal(err)
	}
	if key, err := oldAC.store.GetSetting(ctx, store.SettingManagementKey); err != nil || key != old.State().ManagementKey {
		t.Fatalf("reinitializing the AC changed its existing deployment key: %v", err)
	}
	cfg.ACAddr = newAC.server.URL
	other, err := agent.New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Step(ctx); err == nil {
		t.Fatal("bound AP accepted a different controller before reset")
	}
	if _, err := newAC.store.GetDevice(ctx, apMAC); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bound AP registered on a different AC: %v", err)
	}
	read := func(parts ...string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(append([]string{root}, parts...)...))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	wireless := read("etc", "config", "wireless")
	network := read("etc", "config", "network")
	key := read("etc", "ac", "enrollment.key")
	runner.calls = nil

	stateFile := filepath.Join(root, "etc", "ac", "state.json")
	pending := old.State()
	pending.PendingConfirm = &agent.PendingConfirm{Rev: 1}
	if err := agent.SaveState(stateFile, pending); err != nil {
		t.Fatal(err)
	}
	if err := agent.ResetManagement(cfg); err == nil {
		t.Fatal("switched while a network change still needed confirmation")
	}
	if err := agent.SaveState(stateFile, old.State()); err != nil {
		t.Fatal(err)
	}
	if err := agent.ResetManagement(cfg); err != nil {
		t.Fatal(err)
	}
	a := start()
	if d := newAC.device(t, apMAC); d.State != store.StatePending {
		t.Fatalf("new controller state = %s, want pending", d.State)
	}
	if a.State().AppliedRev != 0 || a.State().PubKey != "" || a.State().ManagementKey != "" {
		t.Fatal("old revision or trust survived reset")
	}
	if m := mutatingCalls(runner.calls); len(m) != 0 {
		t.Fatalf("mutations before readoption: %v", m)
	}
	if !bytes.Equal(wireless, read("etc", "config", "wireless")) ||
		!bytes.Equal(network, read("etc", "config", "network")) ||
		!bytes.Equal(key, read("etc", "ac", "enrollment.key")) {
		t.Fatal("switch changed the current configuration or AP identity")
	}
	newAC.adopt(t, apMAC)
	if err := a.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if a.State().AppliedRev != 1 || newAC.device(t, apMAC).AppliedRev != 1 {
		t.Fatal("new AC's revision 1 was not applied and acknowledged")
	}
	if a.State().ManagementKey == "" || a.State().ManagementKey == old.State().ManagementKey {
		t.Fatal("new AC did not issue a distinct management credential")
	}
	// The override persists across restarts but must not clear the new site's
	// state each time.
	if restarted := start(); restarted.State().AppliedRev != 1 {
		t.Fatal("restart cleared the new AC's applied revision")
	}
	if err := agent.ResetManagement(cfg); err != nil {
		t.Fatal(err)
	}
	a = start()
	if d := newAC.device(t, apMAC); d.State != store.StatePending || a.State().ManagementKey != "" {
		t.Fatal("the previous approval reclaimed the AP automatically after reset")
	}
	if a.State().ResetGeneration != old.State().ResetGeneration+2 {
		t.Fatal("reset generation did not survive the switch and agent restart")
	}
	newAC.adopt(t, apMAC)
	if err := a.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if d := newAC.device(t, apMAC); d.State != store.StateAdopted || a.State().AppliedRev != 2 {
		t.Fatal("readoption did not apply the new revision to the existing AP record")
	}
}
