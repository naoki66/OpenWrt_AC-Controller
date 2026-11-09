package provision

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/naoki66/ac-controller/controller/internal/keys"
	"github.com/naoki66/ac-controller/controller/internal/store"
	"github.com/naoki66/ac-controller/shared/bundle"
	"github.com/naoki66/ac-controller/shared/model"
	"github.com/naoki66/ac-controller/shared/render"
)

// The central reason this feature exists: the controller can build every AP's
// 11k neighbour list from what the APs already reported, so no per-AP daemon is
// needed.
//
// The test also pins down a real ordering constraint. A freshly adopted AP has
// not run the agent yet, so the controller does not know which interfaces or
// SSIDs exist on it and cannot address a list to them. The first package
// therefore carries no neighbour file; the list appears from the second
// revision on, after the AP has reported itself. That is why the operational
// order is "adopt everything, then re-push once".
func TestPackageCarriesPeerNeighborLists(t *testing.T) {
	svc := newService(t)

	// ap1 must be adopted before its BSSes count as advertisement sources.
	if _, err := svc.Adopt(context.Background(), ap1, "corp"); err != nil {
		t.Fatalf("adopt ap1: %v", err)
	}
	if err := svc.Store.ReplaceNeighborsForAP(context.Background(), ap1, []store.BSSNeighbor{
		{APMAC: ap1, Ifname: "phy0.1-ap0", BSSID: "00:00:00:00:0a:01", SSID: "SSID-A", Report: "aa01"},
	}); err != nil {
		t.Fatalf("seed ap1 neighbours: %v", err)
	}

	// First adoption of ap2: nothing is known about its own interfaces yet.
	first, err := svc.Adopt(context.Background(), ap2, "corp")
	if err != nil {
		t.Fatalf("adopt ap2: %v", err)
	}
	if body, ok := extract(t, svc, first.MAC, first.Rev)[render.FileNeighbors]; ok {
		t.Fatalf("first package already carries neighbours: %s", body)
	}

	// ap2 comes up and reports its BSS. This is what the agent sends in hello.
	if err := svc.Store.ReplaceNeighborsForAP(context.Background(), ap2, []store.BSSNeighbor{
		{APMAC: ap2, Ifname: "phy0.1-ap0", BSSID: "00:00:00:00:0b:01", SSID: "SSID-A", Report: "bb01"},
	}); err != nil {
		t.Fatalf("seed ap2 neighbours: %v", err)
	}

	// Re-push: now both sides are known and the lists can be built.
	res, err := svc.Adopt(context.Background(), ap2, "corp")
	if err != nil {
		t.Fatalf("re-adopt ap2: %v", err)
	}

	files := extract(t, svc, res.MAC, res.Rev)
	body, ok := files[render.FileNeighbors]
	if !ok {
		t.Fatalf("package has no neighbours file; files = %v", names(files))
	}

	var lists map[string][][]string
	if err := json.Unmarshal(body, &lists); err != nil {
		t.Fatalf("neighbours is not JSON: %v (%s)", err, body)
	}
	got := lists["phy0.1-ap0"]
	if len(got) != 1 {
		t.Fatalf("ap2 list = %v, want exactly the ap1 entry", got)
	}
	if got[0][0] != "00:00:00:00:0a:01" || got[0][2] != "aa01" {
		t.Errorf("ap2 list entry = %v, want ap1's bssid and verbatim report", got[0])
	}
	// hostapd appends the executing BSS itself; shipping it would duplicate it.
	for _, e := range got {
		if e[0] == "00:00:00:00:0b:01" {
			t.Error("package advertises the AP's own BSSID")
		}
	}
}

// The first AP on a network has no peers. The file must be absent rather than
// empty, so the agent never writes to hostapd for nothing.
func TestPackageOmitsNeighborsWithoutPeers(t *testing.T) {
	svc := newService(t)

	if err := svc.Store.ReplaceNeighborsForAP(context.Background(), ap1, []store.BSSNeighbor{
		{APMAC: ap1, Ifname: "phy0.1-ap0", BSSID: "00:00:00:00:0a:01", SSID: "SSID-A", Report: "aa01"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := svc.Adopt(context.Background(), ap1, "corp")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	files := extract(t, svc, res.MAC, res.Rev)
	if _, ok := files[render.FileNeighbors]; ok {
		t.Errorf("single AP package carries a neighbours file: %s", files[render.FileNeighbors])
	}
}

// A pending device must not appear in anyone's neighbour reports: it has not been
// trusted yet, and its BSSIDs may belong to a device an operator will reject.
func TestPendingDeviceIsNotAdvertised(t *testing.T) {
	svc := newService(t)

	if err := svc.Store.ReplaceNeighborsForAP(context.Background(), ap2, []store.BSSNeighbor{
		{APMAC: ap2, Ifname: "phy0.1-ap0", BSSID: "00:00:00:00:0b:01", SSID: "SSID-A", Report: "bb01"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := svc.Adopt(context.Background(), ap1, "corp")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	files := extract(t, svc, res.MAC, res.Rev)
	if body, ok := files[render.FileNeighbors]; ok {
		t.Errorf("pending device leaked into the list: %s", body)
	}
}

const (
	ap1 = "8c:de:52:00:00:01"
	ap2 = "8c:de:52:00:00:02"
)

func newService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "ac.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	key, err := keys.LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("keys: %v", err)
	}

	tpl := model.Template{
		ID:   "corp",
		Name: "Corp",
		Bridge: model.Bridge{
			Name:       "br-lan",
			VLANs:      []model.BridgeVLAN{{VLAN: 20, Ports: []string{"lan1:t"}}},
			Interfaces: []model.Interface{{Name: "ac_vlan20", Device: "br-lan.20", Proto: "none"}},
		},
		Radios: []model.Radio{{Index: 1, Band: model.Band5G, Channel: "149", HTMode: "EHT80", TxPower: 20}},
		SSIDs: []model.SSID{{
			SSID: "SSID-A", Bands: []string{model.Band5G}, Network: "ac_vlan20",
			Security: model.SecWPA3, PSK: "corp-passphrase",
		}},
	}
	if err := st.UpsertTemplate(context.Background(), tpl); err != nil {
		t.Fatalf("seed template: %v", err)
	}

	for _, mac := range []string{ap1, ap2} {
		if err := st.UpsertDiscovered(context.Background(), store.Device{
			MAC: mac, Model: "XR1710G", Firmware: "test", Hostname: "ap",
		}); err != nil {
			t.Fatalf("seed device %s: %v", mac, err)
		}
	}

	return &Service{Store: st, Key: key, ACID: "test-ac"}
}

// extract pulls a stored revision back out and verifies its signature, so the
// test sees exactly what an AP would receive.
func extract(t *testing.T, svc *Service, mac string, rev int) map[string][]byte {
	t.Helper()
	_, blob, err := svc.Package(context.Background(), mac, rev)
	if err != nil {
		t.Fatalf("fetch package: %v", err)
	}
	_, files, err := bundle.Extract(blob, svc.Key.Public)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return files
}

func names(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
