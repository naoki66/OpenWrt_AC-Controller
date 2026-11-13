package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/naoki66/ac-controller/agent"
	"github.com/naoki66/ac-controller/controller/internal/api"
	"github.com/naoki66/ac-controller/controller/internal/keys"
	"github.com/naoki66/ac-controller/controller/internal/provision"
	"github.com/naoki66/ac-controller/controller/internal/store"
	"github.com/naoki66/ac-controller/shared/model"
	"github.com/naoki66/ac-controller/shared/secret"
)

const apMAC = "8c:de:52:11:22:33"

// scriptedRunner answers ubus with a healthy radio set and records commands.
type scriptedRunner struct {
	mu    sync.Mutex
	calls []string
	root  string
}

func (r *scriptedRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	r.mu.Unlock()

	if name == "ubus" && len(args) == 3 && args[2] == "status" {
		// Report every managed BSS as up. The section names are not known to the
		// runner, so it echoes back whatever the test expects via the wildcard
		// below: parse names out of the last written config instead.
		return r.status(), nil
	}
	return "", nil
}

// status builds ubus output from the wireless file currently on disk.
func (r *scriptedRunner) status() string {
	r.mu.Lock()
	root := r.root
	r.mu.Unlock()

	body, err := os.ReadFile(filepath.Join(root, "etc", "config", "wireless"))
	if err != nil {
		return "{}"
	}
	var ifaces []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "config wifi-iface") {
			continue
		}
		name := strings.Trim(strings.TrimPrefix(line, "config wifi-iface "), "'")
		if !strings.HasPrefix(name, "ac_") {
			continue
		}
		ifaces = append(ifaces, `{"section":"`+name+`","up":true,"config":{"ssid":"x","mode":"ap"}}`)
	}
	return `{"radio0":{"interfaces":[` + strings.Join(ifaces, ",") + `]}}`
}

// mutatingCalls filters out read-only probes (ubus status, iw, ip neigh,
// init.d enabled, the 11k self-report read) so a test can assert that a cycle
// changed nothing on the device without caring that telemetry was collected.
//
// rrm_nr_get_own and rrm_nr_list are reads; rrm_nr_set is not, and must stay
// visible to these assertions.
func mutatingCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		switch {
		case strings.HasPrefix(c, "ubus call network.wireless status"),
			strings.HasPrefix(c, "iw dev "),
			strings.HasPrefix(c, "ip neigh "),
			strings.HasPrefix(c, "ubus list"),
			strings.Contains(c, " rrm_nr_get_own"),
			strings.Contains(c, " rrm_nr_list"),
			strings.HasSuffix(c, " enabled"):
			continue
		}
		out = append(out, c)
	}
	return out
}

type acFixture struct {
	server *httptest.Server
	store  *store.Store
	key    keys.Key
	t      *testing.T
	cookie *http.Cookie
}

const fixtureAPSecret = "e2e-ap-shared-secret"

func newAC(t *testing.T) *acFixture {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "ac.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := api.EnsureAdminAccount(ctx, st, "admin", "123456"); err != nil {
		t.Fatalf("seed admin account: %v", err)
	}
	if err := st.UpsertTemplate(ctx, provision.DefaultTemplate()); err != nil {
		t.Fatalf("seed template: %v", err)
	}

	key, err := keys.FromSeed(bytes.Repeat([]byte{11}, 32))
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	srv := &api.Server{
		Store:           st,
		ACID:            "cafebabecafebabecafebabecafebabe",
		Key:             key,
		Provision:       &provision.Service{Store: st, Key: key, ACID: "cafebabecafebabecafebabecafebabe"},
		SharedSecret:    fixtureAPSecret,
		RequireSignedAP: true,
		Log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		st.Close()
	})

	f := &acFixture{server: ts, store: st, key: key, t: t}
	f.login()
	return f
}

// login obtains the session cookie for the fixture's test account.
func (f *acFixture) login() {
	f.t.Helper()
	resp, err := http.Post(f.server.URL+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"123456"}`))
	if err != nil {
		f.t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("login status %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "ac_session" {
			f.cookie = c
		}
	}
	if f.cookie == nil {
		f.t.Fatal("login returned no session cookie")
	}
}

// post sends a management request carrying the session cookie.
func (f *acFixture) post(path, body string) *http.Response {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.server.URL+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(f.cookie)
	resp, err := f.server.Client().Do(req)
	if err != nil {
		f.t.Fatalf("post %s: %v", path, err)
	}
	return resp
}

// ensureDevice stands in for AP discovery so adopt has something to act on.
func (f *acFixture) ensureDevice(t *testing.T, mac string) {
	t.Helper()
	if err := f.store.UpsertDiscovered(context.Background(), store.Device{
		MAC: mac, Model: "XR1710G", Firmware: "20261026", IP: "10.0.10.250", Hostname: "ap250",
	}); err != nil {
		t.Fatalf("seed device: %v", err)
	}
}

func (f *acFixture) adopt(t *testing.T, mac string) {
	t.Helper()
	f.ensureDevice(t, mac)
	resp := f.post("/api/v1/devices/"+mac+"/adopt", `{"template_id":"default"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("adopt status %d: %s", resp.StatusCode, body)
	}
}

func (f *acFixture) device(t *testing.T, mac string) store.Device {
	t.Helper()
	d, err := f.store.GetDevice(context.Background(), mac)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	return d
}

func newAgentFixture(t *testing.T, ac *acFixture, secret string) (*agent.Agent, *scriptedRunner, string) {
	t.Helper()
	if secret == "" {
		secret = fixtureAPSecret
	}

	root := t.TempDir()
	configDir := filepath.Join(root, "etc", "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	localNetwork := "config interface 'lan'\n\toption device 'br-lan'\n\toption proto 'static'\n\toption ipaddr '10.0.10.250'\n"
	if err := os.WriteFile(filepath.Join(configDir, "network"), []byte(localNetwork), 0o644); err != nil {
		t.Fatalf("seed network: %v", err)
	}

	runner := &scriptedRunner{root: root}
	cfg := agent.Config{
		Root:          root,
		ACAddr:        strings.TrimPrefix(ac.server.URL, "http://"),
		Secret:        secret,
		HealthTimeout: 0,
		Identity: agent.Identity{
			MAC:      apMAC,
			Model:    "XR1710G",
			Firmware: "20261026",
			Hostname: "ap250",
		},
		Runner: runner,
	}
	a, err := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	return a, runner, root
}

// TestAgentAppliesRevisionEndToEnd is the closest thing to a real provisioning
// run that can happen without hardware: a real AC over HTTP, a real signed
// package, and the agent writing into a real directory tree.
func TestAgentAppliesRevisionEndToEnd(t *testing.T) {
	ac := newAC(t)
	a, runner, root := newAgentFixture(t, ac, fixtureAPSecret)
	ctx := context.Background()

	// Cycle 1: registers, learns it is not adopted yet.
	if err := a.Step(ctx); err != nil {
		t.Fatalf("first step: %v", err)
	}
	if d := ac.device(t, apMAC); d.State != store.StatePending {
		t.Fatalf("device state = %q, want pending", d.State)
	}
	if a.State().AppliedRev != 0 {
		t.Errorf("applied rev = %d, want 0", a.State().AppliedRev)
	}
	if m := mutatingCalls(runner.calls); len(m) != 0 {
		t.Errorf("mutating commands ran before adoption: %v", m)
	}

	// Operator adopts the AP.
	ac.adopt(t, apMAC)

	// Cycle 2: pulls rev 1, verifies, applies, reports.
	if err := a.Step(ctx); err != nil {
		t.Fatalf("second step: %v", err)
	}
	if got := a.State().AppliedRev; got != 1 {
		t.Fatalf("applied rev = %d, want 1", got)
	}
	if a.State().SHA256 == "" {
		t.Error("package digest not recorded")
	}
	if a.State().PubKey != ac.key.PublicHex() {
		t.Errorf("pinned key = %q, want %q", a.State().PubKey, ac.key.PublicHex())
	}

	// The controller must see the report.
	d := ac.device(t, apMAC)
	if d.AppliedRev != 1 {
		t.Errorf("controller applied_rev = %d, want 1", d.AppliedRev)
	}
	if d.SyncState != "applied" {
		t.Errorf("controller sync_state = %q, want applied", d.SyncState)
	}

	// The device tree really changed, and the wireless-only default left the
	// network file alone: the local uplink is intact and no VLAN appeared.
	network, err := os.ReadFile(filepath.Join(root, "etc", "config", "network"))
	if err != nil {
		t.Fatalf("read network: %v", err)
	}
	if !strings.Contains(string(network), "10.0.10.250") {
		t.Errorf("local uplink lost\n%s", network)
	}
	if strings.Contains(string(network), "ac_vlan") {
		t.Errorf("wireless-only default touched the network file\n%s", network)
	}

	// One reload of each kind, no more: extra reloads flush the PPE table.
	wifi := 0
	for _, c := range runner.calls {
		if c == "wifi reload" {
			wifi++
		}
	}
	if wifi != 1 {
		t.Errorf("wifi reload ran %d times, want 1 (%v)", wifi, runner.calls)
	}

	// Cycle 3: nothing left to do.
	before := len(runner.calls)
	if err := a.Step(ctx); err != nil {
		t.Fatalf("third step: %v", err)
	}
	if m := mutatingCalls(runner.calls[before:]); len(m) != 0 {
		t.Errorf("idle cycle ran mutating commands: %v", m)
	}
}

// A controller restored from an older database advertises a lower revision.
// The agent must roll back to it; treating "already newer" as done leaves the
// AP on stale config forever (observed on 10.0.30.251: applied 9, desired 7,
// so the rev-7 LAN4-6 BSS never came back).
func TestAgentAppliesLowerRevisionWhenControllerRollsBack(t *testing.T) {
	ac := newAC(t)
	a, _, root := newAgentFixture(t, ac, fixtureAPSecret)
	ctx := context.Background()
	ac.adopt(t, apMAC)

	if err := a.Step(ctx); err != nil {
		t.Fatalf("first step: %v", err)
	}
	if got := a.State().AppliedRev; got != 1 {
		t.Fatalf("applied rev = %d, want 1", got)
	}

	// Make the persisted state look like an AP that had applied a newer
	// revision from a previous controller incarnation.
	statePath := filepath.Join(root, "etc", "ac", "state.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var st map[string]any
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	st["applied_rev"] = 9
	patched, _ := json.Marshal(st)
	if err := os.WriteFile(statePath, patched, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	a2, err := agent.New(agent.Config{
		Root:   root,
		ACAddr: strings.TrimPrefix(ac.server.URL, "http://"),
		Secret: fixtureAPSecret,
		Identity: agent.Identity{
			MAC: apMAC, Model: "XR1710G", Firmware: "20261026", Hostname: "ap250",
		},
		Runner: &scriptedRunner{root: root},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	if err := a2.Step(ctx); err != nil {
		t.Fatalf("rollback step: %v", err)
	}
	if got := a2.State().AppliedRev; got != 1 {
		t.Fatalf("agent kept stale rev %d, want the controller's rev 1", got)
	}
	if d := ac.device(t, apMAC); d.AppliedRev != 1 {
		t.Errorf("controller applied_rev = %d, want 1", d.AppliedRev)
	}
}

func TestAgentRejectsWrongSharedSecret(t *testing.T) {
	ac := newAC(t)
	a, _, _ := newAgentFixture(t, ac, "different-site-secret")

	err := a.Step(context.Background())
	if err == nil {
		t.Fatal("agent authenticated with the wrong shared secret")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should surface the 401, got %v", err)
	}
}

func TestWebSharedSecretRotationReconnectsAP(t *testing.T) {
	ac := newAC(t)
	oldAP, _, _ := newAgentFixture(t, ac, fixtureAPSecret)
	ctx := context.Background()
	if err := oldAP.Step(ctx); err != nil {
		t.Fatalf("connect with initial shared secret: %v", err)
	}

	const replacement = "new-web-shared-secret"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, ac.server.URL+"/api/v1/settings/shared-secret",
		strings.NewReader(`{"secret":"`+replacement+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(ac.cookie)
	resp, err := ac.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save shared secret from web settings: HTTP %d", resp.StatusCode)
	}
	if err := oldAP.Step(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("old shared secret should be rejected after rotation, got %v", err)
	}
	updatedAP, _, _ := newAgentFixture(t, ac, replacement)
	if err := updatedAP.Step(ctx); err != nil {
		t.Fatalf("connect with replacement shared secret: %v", err)
	}
}

func TestDevicePackageScopeReplayAndIntegrity(t *testing.T) {
	ac := newAC(t)
	deviceKey := secret.DeviceKey(fixtureAPSecret, apMAC)
	const other = "02:00:00:00:00:02"
	ac.adopt(t, apMAC)
	ac.adopt(t, other)

	request := func(mac string) (*http.Request, string) {
		t.Helper()
		resp, err := ac.server.Client().Get(ac.server.URL + "/api/v1/ap/challenge")
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("challenge: %d, %v", resp.StatusCode, err)
		}
		challenge := string(raw)
		req, err := http.NewRequest("GET", ac.server.URL+"/api/v1/devices/"+mac+"/package", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(secret.DeviceMACHeader, apMAC)
		req.Header.Set(secret.DeviceChallengeHeader, challenge)
		req.Header.Set(secret.DeviceProofHeader,
			secret.DeviceProof(deviceKey, apMAC, challenge, req.Method, req.URL.RequestURI(), nil))
		return req, challenge
	}
	send := func(req *http.Request, want int) []byte {
		t.Helper()
		resp, err := ac.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != want {
			t.Fatalf("package request: got status %d (%v), want %d", resp.StatusCode, err, want)
		}
		if want == http.StatusOK && resp.Header.Get("Content-Type") != secret.DevicePackageType {
			t.Fatal("device received a package without encryption")
		}
		return body
	}
	req, _ := request(other)
	send(req, http.StatusForbidden)
	req, challenge := request(apMAC)
	ciphertext := send(req, http.StatusOK)
	send(req, http.StatusUnauthorized)
	if _, err := secret.OpenDevicePackage(deviceKey, challenge, ciphertext); err != nil {
		t.Fatalf("decrypt own package: %v", err)
	}
	ciphertext[len(ciphertext)-1] ^= 1
	if _, err := secret.OpenDevicePackage(deviceKey, challenge, ciphertext); err == nil {
		t.Fatal("tampered ciphertext passed authentication")
	}
}

func TestAgentRejectsTamperedPackage(t *testing.T) {
	// A controller that hands out a package it did not sign must be refused
	// before anything is written to disk.
	ac := newAC(t)
	a, runner, root := newAgentFixture(t, ac, fixtureAPSecret)
	ac.adopt(t, apMAC)

	// First cycle pins the key and applies rev 1 successfully.
	if err := a.Step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}

	// Now corrupt the stored package on the controller side and bump the rev so
	// the agent is forced to fetch it again.
	ctx := context.Background()
	if err := ac.store.CreatePackage(ctx, apMAC, 2, "deadbeef", []byte("not a tarball")); err != nil {
		t.Fatalf("inject package: %v", err)
	}
	if err := ac.store.SetConfigRev(ctx, apMAC, 2); err != nil {
		t.Fatalf("bump rev: %v", err)
	}

	before := len(runner.calls)
	if err := a.Step(ctx); err == nil {
		t.Fatal("agent applied a package that failed verification")
	}
	if a.State().AppliedRev != 1 {
		t.Errorf("applied rev moved to %d after a rejected package", a.State().AppliedRev)
	}
	if a.State().PendingSync == "" {
		t.Error("rejection not queued for reporting")
	}
	if m := mutatingCalls(runner.calls[before:]); len(m) != 0 {
		t.Errorf("mutating commands ran for a rejected package: %v", m)
	}

	// The config on disk must be untouched by the rejected revision.
	wireless, err := os.ReadFile(filepath.Join(root, "etc", "config", "wireless"))
	if err != nil {
		t.Fatalf("read wireless: %v", err)
	}
	if !strings.Contains(string(wireless), "ac_ssid_ac_test_5g") {
		t.Errorf("previous good config was disturbed\n%s", wireless)
	}
}

// A dry run must never advance the applied revision: the controller would
// otherwise believe a rehearsal was a real deployment.
func TestAgentDryRunDoesNotAdvanceRevision(t *testing.T) {
	ac := newAC(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc", "config"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	a, err := agent.New(agent.Config{
		Root:   root,
		ACAddr: strings.TrimPrefix(ac.server.URL, "http://"),
		Secret: fixtureAPSecret,
		DryRun: true,
		Identity: agent.Identity{
			MAC: apMAC, Model: "XR1710G", Hostname: "ap250",
		},
		Runner: &scriptedRunner{root: root},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	ac.adopt(t, apMAC)
	if err := a.Step(context.Background()); err != nil {
		t.Fatalf("dry-run step: %v", err)
	}
	if got := a.State().AppliedRev; got != 0 {
		t.Errorf("dry run advanced applied rev to %d", got)
	}
	if _, err := os.Stat(filepath.Join(root, "etc", "config", "wireless")); !os.IsNotExist(err) {
		t.Errorf("dry run wrote into /etc/config (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(root, "etc", "ac", "staging", "rev1", "wireless")); err != nil {
		t.Errorf("dry run did not stage output: %v", err)
	}
}

// The rendered template must survive the whole pipeline intact: a 6 GHz BSS in
// the template has to appear in the file on disk.
func TestAgentAppliesAllBands(t *testing.T) {
	ac := newAC(t)
	ctx := context.Background()

	tpl := model.Template{
		ID:   "tri-band",
		Name: "三频",
		Bridge: model.Bridge{
			Name:       "br-lan",
			VLANs:      []model.BridgeVLAN{{VLAN: 42, Ports: []string{"wan:t"}}},
			Interfaces: []model.Interface{{Name: "ac_vlan42", Device: "br-lan.42", Proto: "none"}},
		},
		Radios: []model.Radio{
			{Index: 0, Band: model.Band2G, Channel: "1", HTMode: "HE20"},
			{Index: 1, Band: model.Band5G, Channel: "36", HTMode: "EHT80"},
			{Index: 2, Band: model.Band6G, Channel: "37", HTMode: "EHT320"},
		},
		SSIDs: []model.SSID{{
			SSID: "Tri", Bands: []string{model.Band2G, model.Band5G, model.Band6G}, Network: "ac_vlan42",
			Security: model.SecWPA3, PSK: "tri-passphrase",
		}},
	}
	if err := ac.store.UpsertTemplate(ctx, tpl); err != nil {
		t.Fatalf("template: %v", err)
	}

	a, _, root := newAgentFixture(t, ac, fixtureAPSecret)
	if err := a.Step(ctx); err != nil {
		t.Fatalf("register: %v", err)
	}
	resp := ac.post("/api/v1/devices/"+apMAC+"/adopt", `{"template_id":"tri-band"}`)
	resp.Body.Close()

	if err := a.Step(ctx); err != nil {
		t.Fatalf("apply: %v", err)
	}

	wireless, err := os.ReadFile(filepath.Join(root, "etc", "config", "wireless"))
	if err != nil {
		t.Fatalf("read wireless: %v", err)
	}
	for _, want := range []string{
		"config wifi-device 'radio2'",
		"config wifi-iface 'ac_ssid_tri_6g'",
		"option network 'ac_vlan42'",
	} {
		if !strings.Contains(string(wireless), want) {
			t.Errorf("missing %q\n%s", want, wireless)
		}
	}
	network, _ := os.ReadFile(filepath.Join(root, "etc", "config", "network"))
	if !strings.Contains(string(network), "option vlan '42'") {
		t.Errorf("bridge-vlan missing\n%s", network)
	}

	d := ac.device(t, apMAC)
	if d.TemplateID != "tri-band" || d.AppliedRev != 1 {
		t.Errorf("controller view: %+v", d)
	}
}
