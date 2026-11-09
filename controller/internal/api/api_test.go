package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naoki66/ac-controller/controller/internal/keys"
	"github.com/naoki66/ac-controller/controller/internal/provision"
	"github.com/naoki66/ac-controller/controller/internal/store"
	"github.com/naoki66/ac-controller/shared/bundle"
	"github.com/naoki66/ac-controller/shared/model"
)

const testMAC = "8c:de:52:11:22:33"

type fixture struct {
	t      *testing.T
	store  *store.Store
	server *httptest.Server
	key    keys.Key
	srv    *Server
	// cookie is the session issued by login. newFixture logs in as the seeded
	// default account so ordinary behaviour tests exercise what an operator sees.
	cookie *http.Cookie
}

// newFixture returns an authenticated operator fixture. Login is always
// required now, so tests that are about devices/templates/telemetry rather than
// credentials log in as the seeded default account.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixtureWithAuth(t, "", "")
	resp := f.login(defaultAdminUsername, defaultAdminPassword)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("default login = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	return f
}

// newFixtureWithAuth builds a controller that demands credentials. adminToken
// guards the management API, sharedSecret guards the AP channel; either empty
// keeps that half open (the login account is always seeded, but this fixture
// does not log in — that is what auth tests measure).
func newFixtureWithAuth(t *testing.T, adminToken, sharedSecret string) *fixture {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "ac.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := EnsureAdminAccount(ctx, st, defaultAdminUsername, defaultAdminPassword); err != nil {
		t.Fatalf("seed admin account: %v", err)
	}
	if err := st.UpsertTemplate(ctx, provision.DefaultTemplate()); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	if err := st.UpsertDiscovered(ctx, store.Device{
		MAC:      testMAC,
		Model:    "XR1710G",
		Firmware: "20261026",
		IP:       "10.0.10.250",
		Hostname: "ap250",
	}); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	key, err := keys.FromSeed(bytes.Repeat([]byte{3}, ed25519.SeedSize))
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{
		Store:        st,
		ACID:         "feedfacefeedfacefeedfacefeedface",
		Key:          key,
		Provision:    &provision.Service{Store: st, Key: key, ACID: "feedfacefeedfacefeedfacefeedface"},
		SharedSecret: sharedSecret,
		AdminToken:   adminToken,
		Log:          log,
	}
	ts := httptest.NewServer(srv.Handler())

	t.Cleanup(func() {
		ts.Close()
		st.Close()
	})
	return &fixture{t: t, store: st, server: ts, key: key, srv: srv}
}

func (f *fixture) do(method, path string, body any) *http.Response {
	f.t.Helper()
	return f.send("", true, method, path, body)
}

// doAuth sends the request with an Authorization: Bearer header, so tests can
// exercise the admin token and the AP shared secret through the same path a
// real caller uses. It deliberately ignores the fixture's session cookie: an
// empty token means "no credential at all".
func (f *fixture) doAuth(token, method, path string, body any) *http.Response {
	f.t.Helper()
	return f.send(token, false, method, path, body)
}

func (f *fixture) send(token string, withCookie bool, method, path string, body any) *http.Response {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			f.t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, f.server.URL+path, rdr)
	if err != nil {
		f.t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if withCookie && f.cookie != nil {
		req.AddCookie(f.cookie)
	}
	resp, err := f.server.Client().Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// login posts the credentials and stores the session cookie for later requests.
func (f *fixture) login(username, password string) *http.Response {
	f.t.Helper()
	resp := f.send("", false, http.MethodPost, "/api/v1/auth/login",
		credentialsRequest{Username: username, Password: password})
	if resp.StatusCode == http.StatusOK {
		for _, c := range resp.Cookies() {
			if c.Name == sessionCookieName {
				f.cookie = c
			}
		}
	}
	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return v
}

// TestAdoptGeneratesVerifiablePackage walks the full P2 path: adopt, download,
// then verify the archive exactly as the AP would.
func TestAdoptGeneratesVerifiablePackage(t *testing.T) {
	f := newFixture(t)

	resp := f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/adopt", map[string]string{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("adopt status = %d", resp.StatusCode)
	}
	adopted := decode[provision.Result](t, resp)
	if adopted.Rev != 1 {
		t.Errorf("rev = %d, want 1", adopted.Rev)
	}
	if adopted.Template != "default" {
		t.Errorf("template = %q, want default", adopted.Template)
	}
	if len(adopted.SHA256) != 64 {
		t.Errorf("sha256 = %q", adopted.SHA256)
	}

	pkgResp := f.do(http.MethodGet, "/api/v1/devices/"+testMAC+"/package", nil)
	if pkgResp.StatusCode != http.StatusOK {
		t.Fatalf("package status = %d", pkgResp.StatusCode)
	}
	if got := pkgResp.Header.Get("Content-Type"); got != "application/gzip" {
		t.Errorf("content-type = %q", got)
	}
	if got := pkgResp.Header.Get("X-Ac-Rev"); got != "1" {
		t.Errorf("X-Ac-Rev = %q, want 1", got)
	}
	blob, err := io.ReadAll(pkgResp.Body)
	pkgResp.Body.Close()
	if err != nil {
		t.Fatalf("read package: %v", err)
	}

	manifest, files, err := bundle.Extract(blob, f.key.Public)
	if err != nil {
		t.Fatalf("verify package: %v", err)
	}
	if manifest.Rev != 1 || manifest.DeviceMAC != testMAC {
		t.Errorf("unexpected manifest: %+v", manifest)
	}
	if manifest.ACID != "feedfacefeedfacefeedfacefeedface" {
		t.Errorf("ac_id = %q", manifest.ACID)
	}
	if len(manifest.ModelWhitelist) != 1 || manifest.ModelWhitelist[0] != "XR1710G" {
		t.Errorf("model whitelist = %v", manifest.ModelWhitelist)
	}

	wireless := string(files["wireless"])
	for _, want := range []string{
		"config wifi-device 'radio0'",
		"config wifi-device 'radio2'",
		"config wifi-iface 'ac_ssid_ac_test_6g'",
		"option network 'lan'",
		"option mobility_domain 'a1b2'",
	} {
		if !strings.Contains(wireless, want) {
			t.Errorf("wireless missing %q\n%s", want, wireless)
		}
	}
	// The default template is wireless-only: no network file in the package,
	// so the AP keeps its own layer-2 configuration after adoption.
	if network := string(files["network"]); network != "" {
		t.Errorf("wireless-only default rendered a network file:\n%s", network)
	}

	// Device state must reflect the adoption.
	d, err := f.store.GetDevice(context.Background(), testMAC)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if d.State != store.StateAdopted || d.TemplateID != "default" || d.ConfigRev != 1 {
		t.Errorf("device after adopt: %+v", d)
	}
}

func TestRepushIncrementsRevision(t *testing.T) {
	f := newFixture(t)

	if resp := f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/adopt", map[string]string{}); resp.StatusCode != http.StatusOK {
		t.Fatalf("adopt status = %d", resp.StatusCode)
	}
	resp := f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/repush", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("repush status = %d", resp.StatusCode)
	}
	second := decode[provision.Result](t, resp)
	if second.Rev != 2 {
		t.Fatalf("rev = %d, want 2", second.Rev)
	}

	// Both revisions stay downloadable and distinct.
	first := f.do(http.MethodGet, "/api/v1/devices/"+testMAC+"/package?rev=1", nil)
	firstBody, _ := io.ReadAll(first.Body)
	first.Body.Close()
	latest := f.do(http.MethodGet, "/api/v1/devices/"+testMAC+"/package", nil)
	latestBody, _ := io.ReadAll(latest.Body)
	latest.Body.Close()

	if bytes.Equal(firstBody, latestBody) {
		t.Error("rev 1 and rev 2 downloadable bytes are identical")
	}
	if m, err := bundle.Verify(firstBody, f.key.Public); err != nil || m.Rev != 1 {
		t.Errorf("rev 1 package: %v %+v", err, m)
	}
	if m, err := bundle.Verify(latestBody, f.key.Public); err != nil || m.Rev != 2 {
		t.Errorf("latest package: %v %+v", err, m)
	}

	list := decode[struct {
		Packages []store.Package `json:"packages"`
	}](t, f.do(http.MethodGet, "/api/v1/devices/"+testMAC+"/packages", nil))
	if len(list.Packages) != 2 {
		t.Errorf("package count = %d, want 2", len(list.Packages))
	}
}

func TestRepushRequiresAdoption(t *testing.T) {
	f := newFixture(t)
	resp := f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/repush", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestReleaseResetsToPending(t *testing.T) {
	f := newFixture(t)
	f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/adopt", map[string]string{}).Body.Close()

	resp := f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/release", nil)
	d := decode[store.Device](t, resp)
	if d.State != store.StatePending {
		t.Errorf("state = %q, want pending", d.State)
	}
	if d.TemplateID != "" {
		t.Errorf("template_id = %q, want empty", d.TemplateID)
	}
	// The revision counter must survive so a re-adopt never reuses a rev number.
	if d.ConfigRev != 1 {
		t.Errorf("config_rev = %d, want 1", d.ConfigRev)
	}
}

func TestAdoptUnknownDevice(t *testing.T) {
	f := newFixture(t)
	resp := f.do(http.MethodPost, "/api/v1/devices/aa:bb:cc:dd:ee:ff/adopt", map[string]string{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestAdoptUnknownTemplate(t *testing.T) {
	f := newFixture(t)
	resp := f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/adopt", map[string]string{"template_id": "nope"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMalformedMACIsRejected(t *testing.T) {
	f := newFixture(t)
	resp := f.do(http.MethodGet, "/api/v1/devices/not-a-mac", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestTemplateCRUD(t *testing.T) {
	f := newFixture(t)

	tpl := model.Template{
		ID:   "corp",
		Name: "Corp",
		Bridge: model.Bridge{
			Name:       "br-lan",
			VLANs:      []model.BridgeVLAN{{VLAN: 20, Ports: []string{"wan:t"}}},
			Interfaces: []model.Interface{{Name: "ac_vlan20", Device: "br-lan.20", Proto: "none"}},
		},
		Radios: []model.Radio{{Index: 1, Band: model.Band5G, Channel: "149", HTMode: "EHT80", TxPower: 20}},
		SSIDs: []model.SSID{{
			SSID: "Corp", Bands: []string{model.Band5G}, Network: "ac_vlan20",
			Security: model.SecWPA3, PSK: "corp-passphrase",
		}},
	}
	resp := f.do(http.MethodPost, "/api/v1/templates", tpl)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	got := decode[model.Template](t, f.do(http.MethodGet, "/api/v1/templates/corp", nil))
	if got.Name != "Corp" || len(got.SSIDs) != 1 {
		t.Errorf("template round-trip mismatch: %+v", got)
	}

	list := decode[struct {
		Templates []model.Template `json:"templates"`
	}](t, f.do(http.MethodGet, "/api/v1/templates", nil))
	if len(list.Templates) != 2 {
		t.Errorf("template count = %d, want 2", len(list.Templates))
	}
}

func TestInvalidTemplateIsRejected(t *testing.T) {
	f := newFixture(t)

	// 6 GHz SSID without a 6 GHz radio.
	tpl := model.Template{
		ID:     "bad",
		Name:   "Bad",
		Radios: []model.Radio{{Index: 1, Band: model.Band5G, Channel: "36", HTMode: "EHT80"}},
		SSIDs: []model.SSID{{
			SSID: "X", Bands: []string{model.Band6G}, Network: "ac_vlan10",
			Security: model.SecWPA3, PSK: "passphrase-x",
		}},
	}
	resp := f.do(http.MethodPost, "/api/v1/templates", tpl)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPubKeyEndpoint(t *testing.T) {
	f := newFixture(t)

	body := decode[struct {
		Algorithm string `json:"algorithm"`
		PublicKey string `json:"public_key"`
	}](t, f.do(http.MethodGet, "/api/v1/ac/pubkey", nil))
	if body.Algorithm != "ed25519" {
		t.Errorf("algorithm = %q", body.Algorithm)
	}
	if body.PublicKey != f.key.PublicHex() {
		t.Errorf("public_key = %q, want %q", body.PublicKey, f.key.PublicHex())
	}

	resp := f.do(http.MethodGet, "/api/v1/ac/pubkey?format=text", nil)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.TrimSpace(string(raw)) != f.key.PublicHex() {
		t.Errorf("text format = %q", raw)
	}
}

func TestPackageNotFoundBeforeAdopt(t *testing.T) {
	f := newFixture(t)
	resp := f.do(http.MethodGet, "/api/v1/devices/"+testMAC+"/package", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// Two adoptions of the same device must not reuse a revision number.
func TestReAdoptContinuesRevisionSequence(t *testing.T) {
	f := newFixture(t)
	f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/adopt", map[string]string{}).Body.Close()
	f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/release", nil).Body.Close()

	res := decode[provision.Result](t, f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/adopt", map[string]string{}))
	if res.Rev != 2 {
		t.Errorf("rev after re-adopt = %d, want 2", res.Rev)
	}
}

// The trimmed profile must actually remove the routes, not just advertise the
// flags: a UI that hides a page is not a security boundary, and an AP that kept
// asking for an image would otherwise get one.
func TestTrimmedProfileHidesSubsystems(t *testing.T) {
	f := newFixtureWithAuth(t, "trim-token", "trim-secret")
	f.srv.Features = &FeatureGate{
		Profile:          "openwrt",
		TelemetryHistory: false,
		Firmware:         false,
		PackageHistory:   1,
		EventsMax:        500,
	}
	ts := httptest.NewServer(f.srv.Handler())
	t.Cleanup(ts.Close)

	get := func(path string) *http.Response {
		req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		if err != nil {
			t.Fatalf("request %s: %v", path, err)
		}
		req.Header.Set("Authorization", "Bearer trim-token")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return resp
	}

	resp := get("/api/v1/features")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("features status = %d: %s", resp.StatusCode, body)
	}
	gate := decode[FeatureGate](t, resp)
	if gate.TelemetryHistory || gate.Firmware {
		t.Errorf("features = %+v, want both subsystems off", gate)
	}
	if gate.PackageHistory != 1 || gate.EventsMax != 500 {
		t.Errorf("limits not reported: %+v", gate)
	}

	for _, path := range []string{"/api/v1/firmwares", "/api/v1/telemetry/counts"} {
		resp := get(path)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestFirmwareRefNilWhenDisabled(t *testing.T) {
	f := newFixture(t)
	f.srv.Features = &FeatureGate{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ap/hello", nil)
	dev := store.Device{State: store.StateAdopted, FwTarget: "20261026"}
	if ref := f.srv.firmwareRef(req, dev); ref != nil {
		t.Fatalf("firmware ref = %+v, want nil", ref)
	}
}
