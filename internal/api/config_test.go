package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/naoki66/ac-controller/internal/model"
)

// A snapshot taken from one controller must restore the same intent on another,
// including group membership and network overrides.
func TestConfigExportImportRoundTrip(t *testing.T) {
	src := newFixture(t)
	src.adopt()

	ctx := context.Background()
	templates, err := src.store.ListTemplates(ctx)
	if err != nil || len(templates) == 0 {
		t.Fatalf("list templates: %v", err)
	}
	g, err := src.store.CreateGroup(ctx, "一层", templates[0].ID)
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := src.store.SetDeviceGroup(ctx, testMAC, g.ID); err != nil {
		t.Fatalf("set group: %v", err)
	}
	if err := src.store.SetDeviceNetwork(ctx, testMAC, `{"igmp_snooping":true}`); err != nil {
		t.Fatalf("set network: %v", err)
	}

	snap := decode[configSnapshot](t, src.do(http.MethodGet, "/api/v1/config/export", nil))
	if snap.Format != acConfigFormat || snap.Version != acConfigVersion {
		t.Fatalf("snapshot header = %q v%d", snap.Format, snap.Version)
	}
	if len(snap.Templates) != len(templates) {
		t.Errorf("templates = %d, want %d", len(snap.Templates), len(templates))
	}
	if len(snap.Groups) != 1 || snap.Groups[0].ID != g.ID || snap.Groups[0].Name != "一层" {
		t.Errorf("groups = %+v", snap.Groups)
	}
	if len(snap.DeviceOverrides) != 1 {
		t.Fatalf("device overrides = %+v", snap.DeviceOverrides)
	}
	ov := snap.DeviceOverrides[0]
	if ov.MAC != testMAC || ov.GroupID != g.ID || ov.Network == nil || ov.Network.IGMPSnooping == nil || !*ov.Network.IGMPSnooping {
		t.Errorf("override = %+v", ov)
	}

	dst := newFixture(t)
	res := decode[configImportResult](t, dst.do(http.MethodPost, "/api/v1/config/import", snap))
	if res.Templates != len(snap.Templates) || res.Groups != 1 || res.Devices != 1 {
		t.Errorf("import result = %+v", res)
	}
	if len(res.SkippedDevices) != 0 {
		t.Errorf("skipped = %v, want none", res.SkippedDevices)
	}

	gotGroup, err := dst.store.GetGroup(ctx, g.ID)
	if err != nil || gotGroup.Name != "一层" || gotGroup.TemplateID != templates[0].ID {
		t.Errorf("restored group = %+v, err %v", gotGroup, err)
	}
	gotDevice, err := dst.store.GetDevice(ctx, testMAC)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if gotDevice.GroupID != g.ID || gotDevice.NetworkParams == "" {
		t.Errorf("restored device = group %q params %q", gotDevice.GroupID, gotDevice.NetworkParams)
	}
}

// Restoring before the APs have reported in must not fail: the override waits
// with the device and the summary says so.
func TestConfigImportSkipsUnknownDevice(t *testing.T) {
	f := newFixture(t)
	snap := configSnapshot{
		Format:  acConfigFormat,
		Version: acConfigVersion,
		DeviceOverrides: []configDeviceOverride{{
			MAC: "aa:bb:cc:dd:ee:99", GroupID: "",
		}},
	}
	res := decode[configImportResult](t, f.do(http.MethodPost, "/api/v1/config/import", snap))
	if res.Devices != 0 || len(res.SkippedDevices) != 1 || res.SkippedDevices[0] != "aa:bb:cc:dd:ee:99" {
		t.Fatalf("import result = %+v", res)
	}
}

// Validation happens before any write: a bad document leaves the controller
// untouched and returns every problem it found, not just the first one.
func TestConfigImportRejectsInvalidSnapshots(t *testing.T) {
	f := newFixture(t)

	badFormat := configSnapshot{Format: "other", Version: acConfigVersion}
	resp := f.do(http.MethodPost, "/api/v1/config/import", badFormat)
	if resp.StatusCode != http.StatusBadRequest {
		resp.Body.Close()
		t.Fatalf("bad format status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Every radio disabled: the template passes structural checks but cannot
	// beacon anything, so it must be rejected at import time.
	tpl := model.Template{
		ID:   "tpl-disabled",
		Name: "全禁用",
		Radios: []model.Radio{{
			Index: 1, Band: model.Band5G, Channel: "36", HTMode: "EHT160", TxPower: 20, Disabled: true,
		}},
		SSIDs: []model.SSID{{
			SSID: "X", Bands: []string{model.Band5G}, Network: "lan",
			Security: model.SecWPA3, PSK: "passphrase-x",
		}},
	}
	snap := configSnapshot{
		Format:    acConfigFormat,
		Version:   acConfigVersion,
		Templates: []model.Template{tpl},
		Groups:    []configGroup{{ID: "g1", Name: "g", TemplateID: "does-not-exist"}},
	}
	resp = f.do(http.MethodPost, "/api/v1/config/import", snap)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid snapshot status = %d, want 400", resp.StatusCode)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	for _, want := range []string{"at least one radio", "does-not-exist"} {
		if !strings.Contains(body.Error, want) {
			t.Errorf("error %q does not mention %q", body.Error, want)
		}
	}
}
