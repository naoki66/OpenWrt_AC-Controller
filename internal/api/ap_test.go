package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/naoki66/ac-controller/internal/keys"
	"github.com/naoki66/ac-controller/internal/proto"
	"github.com/naoki66/ac-controller/internal/store"
)

type apFixture struct {
	t      *testing.T
	store  *store.Store
	server *httptest.Server
	// cookie is the session used by the read-only management calls do makes.
	cookie *http.Cookie
}

func newAPFixture(t *testing.T, secret string) *apFixture {
	f := newAPFixtureNoLogin(t, secret)
	f.login(defaultAdminUsername, defaultAdminPassword)
	return f
}

func newAPFixtureNoLogin(t *testing.T, secret string) *apFixture {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "ac.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := EnsureAdminAccount(ctx, st); err != nil {
		t.Fatalf("seed admin account: %v", err)
	}

	key, err := keys.FromSeed(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	srv := &Server{
		Store:        st,
		ACID:         "feedfacefeedfacefeedfacefeedface",
		Key:          key,
		SharedSecret: secret,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		st.Close()
	})
	return &apFixture{t: t, store: st, server: ts}
}

func (f *apFixture) login(username, password string) {
	f.t.Helper()
	raw, err := json.Marshal(credentialsRequest{Username: username, Password: password})
	if err != nil {
		f.t.Fatalf("marshal login: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/api/v1/auth/login", bytes.NewReader(raw))
	if err != nil {
		f.t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.server.Client().Do(req)
	if err != nil {
		f.t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("login = %d, want 200", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			f.cookie = c
		}
	}
}

func (f *apFixture) hello(secret string, h proto.Hello) (*http.Response, proto.HelloReply) {
	f.t.Helper()

	raw, err := json.Marshal(h)
	if err != nil {
		f.t.Fatalf("marshal hello: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/api/v1/ap/hello", bytes.NewReader(raw))
	if err != nil {
		f.t.Fatalf("request: %v", err)
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := f.server.Client().Do(req)
	if err != nil {
		f.t.Fatalf("hello: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return resp, proto.HelloReply{}
	}
	var reply proto.HelloReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		f.t.Fatalf("decode reply: %v", err)
	}
	resp.Body.Close()
	return resp, reply
}

// do issues a plain request against the controller, for the read-only
// management endpoints the AP fixture is reused for.
func (f *apFixture) do(method, path string) *http.Response {
	f.t.Helper()
	req, err := http.NewRequest(method, f.server.URL+path, nil)
	if err != nil {
		f.t.Fatalf("request: %v", err)
	}
	if f.cookie != nil {
		req.AddCookie(f.cookie)
	}
	resp, err := f.server.Client().Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func TestHelloRegistersDevice(t *testing.T) {
	f := newAPFixture(t, "")

	resp, reply := f.hello("", proto.Hello{
		MAC:      "8C:DE:52:11:22:33",
		Model:    "XR1710G",
		Firmware: "20261026",
		Hostname: "ap250",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if reply.State != store.StatePending {
		t.Errorf("state = %q, want pending", reply.State)
	}
	if reply.DesiredRev != 0 {
		t.Errorf("desired_rev = %d, want 0 for a pending device", reply.DesiredRev)
	}
	if reply.PubKey == "" {
		t.Error("pubkey missing from reply")
	}

	d, err := f.store.GetDevice(context.Background(), "8c:de:52:11:22:33")
	if err != nil {
		t.Fatalf("device not registered: %v", err)
	}
	if d.Hostname != "ap250" || d.Model != "XR1710G" {
		t.Errorf("unexpected device: %+v", d)
	}
}

// Uptime is a heartbeat-carried snapshot. Zero means the agent could not read
// it, so an older non-zero value must not be blanked out by a newer hello.
func TestHelloRecordsUptime(t *testing.T) {
	f := newAPFixture(t, "")
	const mac = "8c:de:52:11:22:33"

	f.hello("", proto.Hello{MAC: "8C:DE:52:11:22:33", Uptime: 3600})
	d, err := f.store.GetDevice(context.Background(), mac)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if d.Uptime != 3600 {
		t.Errorf("uptime = %d, want 3600", d.Uptime)
	}

	f.hello("", proto.Hello{MAC: "8C:DE:52:11:22:33", Uptime: 0})
	d, err = f.store.GetDevice(context.Background(), mac)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if d.Uptime != 3600 {
		t.Errorf("uptime after silent hello = %d, want 3600", d.Uptime)
	}
}

func TestHelloReportsDesiredRevisionWhenAdopted(t *testing.T) {
	f := newAPFixture(t, "")
	ctx := context.Background()

	if _, reply := f.hello("", proto.Hello{MAC: testMAC, Model: "XR1710G"}); reply.State != store.StatePending {
		t.Fatalf("expected pending, got %q", reply.State)
	}
	if err := f.store.MarkAdopted(ctx, testMAC, "default", 3); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	_, reply := f.hello("", proto.Hello{MAC: testMAC, AppliedRev: 2})
	if reply.State != store.StateAdopted {
		t.Errorf("state = %q", reply.State)
	}
	if reply.DesiredRev != 3 {
		t.Errorf("desired_rev = %d, want 3", reply.DesiredRev)
	}
	if reply.PackagePath != "/api/v1/devices/8c:de:52:11:22:33/package?rev=3" {
		t.Errorf("package_path = %q", reply.PackagePath)
	}
}

func TestHelloRecordsApplyReport(t *testing.T) {
	f := newAPFixture(t, "")
	ctx := context.Background()
	f.hello("", proto.Hello{MAC: testMAC})

	f.hello("", proto.Hello{
		MAC:         testMAC,
		AppliedRev:  2,
		SyncState:   proto.SyncRollback,
		Steering:    "nrsyncd",
		SyncMessage: "bss ac_ssid_corp_5g did not come up",
	})

	d, err := f.store.GetDevice(ctx, testMAC)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if d.AppliedRev != 2 {
		t.Errorf("applied_rev = %d, want 2", d.AppliedRev)
	}
	if d.SyncState != proto.SyncRollback {
		t.Errorf("sync_state = %q", d.SyncState)
	}
	if d.SyncMessage == "" {
		t.Error("sync_message not stored")
	}
	if d.SyncAt == 0 {
		t.Error("sync_at not stored")
	}
}

// applied_rev follows what the AP reports, including a lower revision after a
// rollback. Keeping the maximum would leave the panel showing a revision the
// device no longer runs (observed on 10.0.30.251: agent rolled back to 7,
// controller still displayed 9).
func TestAppliedRevisionFollowsAgentRollback(t *testing.T) {
	f := newAPFixture(t, "")
	ctx := context.Background()

	f.hello("", proto.Hello{MAC: testMAC, AppliedRev: 5, SyncState: proto.SyncApplied})
	f.hello("", proto.Hello{MAC: testMAC, AppliedRev: 2, SyncState: proto.SyncApplied})

	d, err := f.store.GetDevice(ctx, testMAC)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if d.AppliedRev != 2 {
		t.Errorf("applied_rev = %d, want 2 (the revision the AP actually runs)", d.AppliedRev)
	}
}

func TestHelloRequiresBearerWhenSecretConfigured(t *testing.T) {
	f := newAPFixture(t, "s3cret-token")

	resp, _ := f.hello("", proto.Hello{MAC: testMAC})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status without token = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	resp, _ = f.hello("wrong-token", proto.Hello{MAC: testMAC})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status with wrong token = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	resp, reply := f.hello("s3cret-token", proto.Hello{MAC: testMAC})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status with correct token = %d", resp.StatusCode)
	}
	if !reply.SecretRequired {
		t.Error("secret_required = false, want true")
	}
}

func TestHelloRejectsMalformedMAC(t *testing.T) {
	f := newAPFixture(t, "")
	resp, _ := f.hello("", proto.Hello{MAC: "nope"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestHelloToleratesUnknownFields locks in forward compatibility.
//
// The hello endpoint is the one place two different builds meet: an agent that
// gains a telemetry field must keep working against a controller that has never
// heard of it. Rejecting the body (which strict decoding would do) returns 400
// and stops the AP reporting anything at all — far worse than ignoring a field.
func TestHelloToleratesUnknownFields(t *testing.T) {
	f := newAPFixture(t, "")

	body := []byte(`{"mac":"` + testMAC + `","surprise":true}`)
	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/api/v1/ap/hello", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a newer agent must not be rejected)", resp.StatusCode)
	}
	d, err := f.store.GetDevice(context.Background(), testMAC)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if d.Hostname != "unknown" {
		t.Errorf("hostname = %q: the known fields were not read", d.Hostname)
	}
}

func TestHelloIngestsClients(t *testing.T) {
	f := newAPFixture(t, "")

	_, _ = f.hello("", proto.Hello{
		MAC: testMAC,
		Clients: []proto.ClientStat{
			{MAC: "AA:BB:CC:DD:EE:01", SSID: "SSID-A_5G", Band: "5g", Signal: -47,
				RxRateMbps: 1441, TxRateMbps: 1201, ConnectedSeconds: 3600, IP: "10.0.10.5"},
			{MAC: "aa:bb:cc:dd:ee:02", SSID: "SSID-A_5G", Band: "5g", Signal: -71},
		},
	})

	clients, err := f.store.ListClients(context.Background())
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	if len(clients) != 2 {
		t.Fatalf("clients = %d, want 2", len(clients))
	}
	// MAC is normalised, and the reporting AP is recorded, not the client MAC.
	// Look up by MAC: the list is ordered by signal, which is not what this
	// assertion is about.
	var first *store.Client
	for i := range clients {
		if clients[i].MAC == "aa:bb:cc:dd:ee:01" {
			first = &clients[i]
		}
	}
	if first == nil {
		t.Fatalf("upper-case MAC was not normalised: %+v", clients)
	}
	if first.APMAC != testMAC || first.Signal != -47 || first.IP != "10.0.10.5" {
		t.Errorf("unexpected client: %+v", first)
	}

	// A later report without that station must mark it offline, not delete it.
	_, _ = f.hello("", proto.Hello{
		MAC:     testMAC,
		Clients: []proto.ClientStat{{MAC: "aa:bb:cc:dd:ee:02", Signal: -60}},
	})
	clients, err = f.store.ListClients(context.Background())
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	if len(clients) != 2 {
		t.Fatalf("client history lost: %d rows", len(clients))
	}
	var online int
	for _, c := range clients {
		if c.Online {
			online++
		}
	}
	if online != 1 {
		t.Errorf("online = %d, want 1", online)
	}
}

// An agent that does not collect clients sends no list at all; that must not
// wipe the table.
func TestHelloWithoutClientsLeavesTableAlone(t *testing.T) {
	f := newAPFixture(t, "")

	_, _ = f.hello("", proto.Hello{
		MAC:     testMAC,
		Clients: []proto.ClientStat{{MAC: "aa:bb:cc:dd:ee:09", Signal: -50}},
	})
	_, _ = f.hello("", proto.Hello{MAC: testMAC})

	clients, err := f.store.ListClients(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(clients) != 1 || !clients[0].Online {
		t.Errorf("table was disturbed: %+v", clients)
	}
}
