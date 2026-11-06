package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/naoki66/ac-controller/internal/proto"
	"github.com/naoki66/ac-controller/internal/store"
)

const testClientMAC = "aa:bb:cc:dd:ee:ff"

// seedClient registers one associated station as reported by the fixture AP.
func (f *fixture) seedClient(mac string) {
	f.t.Helper()
	if err := f.store.ReplaceClientsForAP(context.Background(), testMAC, []store.Client{{
		MAC:    mac,
		Ifname: "phy1-ap0",
		SSID:   "Home",
		Signal: -55,
	}}); err != nil {
		f.t.Fatalf("seed client: %v", err)
	}
}

// Operator metadata is a partial update: a later request that only touches the
// tracking flag must not erase the alias, and clearing the SSID must also drop
// the AP preference that only makes sense alongside it.
func TestClientMetaPartialUpdateAndList(t *testing.T) {
	f := newFixture(t)
	f.seedClient(testClientMAC)

	meta := decode[store.ClientMeta](t, f.do(http.MethodPut, "/api/v1/clients/"+testClientMAC, map[string]any{
		"alias":       "  Laptop  ",
		"note":        "office desk",
		"tracked":     true,
		"bind_ssid":   "Home",
		"bind_ap_mac": "8C:DE:52:11:22:33",
	}))
	if meta.Alias != "Laptop" || meta.Note != "office desk" || !meta.Tracked {
		t.Fatalf("meta = %+v", meta)
	}
	if meta.BindSSID != "Home" || meta.BindAPMAC != testMAC {
		t.Errorf("binding = %q/%q, want Home/%s", meta.BindSSID, meta.BindAPMAC, testMAC)
	}

	// Only the tracking flag changes; everything else survives.
	meta = decode[store.ClientMeta](t, f.do(http.MethodPut, "/api/v1/clients/"+testClientMAC, map[string]any{
		"tracked": false,
	}))
	if meta.Alias != "Laptop" || meta.Note != "office desk" || meta.Tracked {
		t.Errorf("partial update erased fields: %+v", meta)
	}

	// Clearing the SSID clears the AP preference too.
	meta = decode[store.ClientMeta](t, f.do(http.MethodPut, "/api/v1/clients/"+testClientMAC, map[string]any{
		"bind_ssid": "",
	}))
	if meta.BindSSID != "" || meta.BindAPMAC != "" {
		t.Errorf("binding not cleared: %+v", meta)
	}

	list := decode[struct {
		Clients []store.Client `json:"clients"`
	}](t, f.do(http.MethodGet, "/api/v1/clients", nil))
	if len(list.Clients) != 1 {
		t.Fatalf("clients = %+v", list.Clients)
	}
	c := list.Clients[0]
	if c.Alias != "Laptop" || c.Note != "office desk" || c.Tracked {
		t.Errorf("list did not merge operator metadata: %+v", c)
	}
}

func TestClientMetaRejectsOverlongAlias(t *testing.T) {
	f := newFixture(t)
	resp := f.do(http.MethodPut, "/api/v1/clients/"+testClientMAC, map[string]any{
		"alias": string(make([]byte, 65)),
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// A binding reaches the AP through the hello reply, with the BSSID resolved
// from the neighbour reports the AP itself submitted. Clearing it must send an
// authoritative empty list rather than omitting the field.
func TestHelloDeliversClientBindings(t *testing.T) {
	f := newFixture(t)
	f.adopt()
	ctx := context.Background()

	if err := f.store.ReplaceNeighborsForAP(ctx, testMAC, []store.BSSNeighbor{{
		APMAC:  testMAC,
		Ifname: "phy1-ap0",
		BSSID:  "cc:dd:ee:ff:00:11",
		SSID:   "Home",
	}}); err != nil {
		t.Fatalf("seed neighbors: %v", err)
	}
	if err := f.store.UpsertClientMeta(ctx, store.ClientMeta{
		MAC:       testClientMAC,
		BindSSID:  "Home",
		BindAPMAC: testMAC,
	}); err != nil {
		t.Fatalf("seed meta: %v", err)
	}

	reply := f.helloWith(nil)
	if len(reply.Bindings) != 1 {
		t.Fatalf("bindings = %+v, want one", reply.Bindings)
	}
	b := reply.Bindings[0]
	if b.MAC != testClientMAC || b.SSID != "Home" || b.APMAC != testMAC {
		t.Errorf("binding = %+v", b)
	}
	if b.BSSID != "cc:dd:ee:ff:00:11" {
		t.Errorf("bssid = %q, want the neighbour report's BSSID", b.BSSID)
	}

	meta := decode[store.ClientMeta](t, f.do(http.MethodPut, "/api/v1/clients/"+testClientMAC, map[string]any{
		"bind_ssid": "",
	}))
	if meta.BindSSID != "" {
		t.Fatalf("binding = %q, want empty", meta.BindSSID)
	}

	reply = f.helloWith(nil)
	// Non-nil even when empty: the agent distinguishes "clear" from "the
	// controller does not support bindings".
	if reply.Bindings == nil {
		t.Fatal("bindings omitted from the reply; agent would keep stale intent")
	}
	if len(reply.Bindings) != 0 {
		t.Errorf("bindings = %+v, want none", reply.Bindings)
	}
}

// A binding with no AP preference resolves to any AP serving the SSID.
func TestResolveBindingBSSIDWithoutAP(t *testing.T) {
	neighbors := []store.BSSNeighbor{
		{APMAC: "8c:de:52:00:00:01", Ifname: "phy0-ap0", BSSID: "aa:bb:cc:00:00:01", SSID: "Home"},
		{APMAC: testMAC, Ifname: "phy1-ap0", BSSID: "cc:dd:ee:ff:00:11", SSID: "Home"},
	}
	if got := resolveBindingBSSID(neighbors, proto.ClientBinding{SSID: "Home"}); got != "aa:bb:cc:00:00:01" {
		t.Errorf("unprefixed binding resolved to %q", got)
	}
	if got := resolveBindingBSSID(neighbors, proto.ClientBinding{SSID: "Guest"}); got != "" {
		t.Errorf("unknown SSID resolved to %q, want empty", got)
	}
	if got := resolveBindingBSSID(neighbors, proto.ClientBinding{SSID: "Home", APMAC: "aa:bb:cc:dd:ee:00"}); got != "" {
		t.Errorf("unmatched AP resolved to %q, want empty", got)
	}
}

// Kicking an associated client queues the closed-set action for the AP; an
// offline client is refused because there is nothing to deauthenticate.
func TestKickClient(t *testing.T) {
	f := newFixture(t)
	f.adopt()
	f.seedClient(testClientMAC)

	resp := f.do(http.MethodPost, "/api/v1/clients/"+testClientMAC+"/kick", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	created := decode[store.Action](t, resp)
	if created.Type != proto.ActionKick || created.MAC != testMAC {
		t.Fatalf("action = %+v", created)
	}
	if created.Args["mac"] != testClientMAC || created.Args["ifname"] != "phy1-ap0" {
		t.Errorf("args = %+v", created.Args)
	}

	reply := f.helloWith(nil)
	if len(reply.Actions) != 1 || reply.Actions[0].Type != proto.ActionKick {
		t.Errorf("delivered actions = %+v", reply.Actions)
	}

	// The AP reports no clients: the station is offline and cannot be kicked.
	if err := f.store.ReplaceClientsForAP(context.Background(), testMAC, nil); err != nil {
		t.Fatalf("mark offline: %v", err)
	}
	resp = f.do(http.MethodPost, "/api/v1/clients/"+testClientMAC+"/kick", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("offline kick status = %d, want 409", resp.StatusCode)
	}

	resp = f.do(http.MethodPost, "/api/v1/clients/de:ad:be:ef:00:01/kick", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown client status = %d, want 404", resp.StatusCode)
	}
}

// The generic device action endpoint accepts kick_client only with a valid MAC
// and interface name.
func TestDeviceActionKickValidatesArgs(t *testing.T) {
	f := newFixture(t)
	f.adopt()

	for _, args := range []map[string]string{
		{"mac": "nope", "ifname": "phy1-ap0"},
		{"mac": testClientMAC, "ifname": "phy1 ap0"},
		{},
	} {
		resp := f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/actions", map[string]any{
			"type": proto.ActionKick,
			"args": args,
		})
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("args %v: status = %d, want 400", args, resp.StatusCode)
		}
	}

	resp := f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/actions", map[string]any{
		"type": proto.ActionKick,
		"args": map[string]string{"mac": testClientMAC, "ifname": "phy1-ap0"},
	})
	if resp.StatusCode != http.StatusAccepted {
		resp.Body.Close()
		t.Fatalf("valid kick status = %d, want 202", resp.StatusCode)
	}
	resp.Body.Close()
}
