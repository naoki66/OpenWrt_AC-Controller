package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/naoki66/ac-controller/internal/model"
	"github.com/naoki66/ac-controller/internal/store"
)

func corpTemplate() model.Template {
	return model.Template{
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
}

func seedCorpTemplate(t *testing.T, f *fixture) {
	t.Helper()
	resp := f.do(http.MethodPost, "/api/v1/templates", corpTemplate())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create template status = %d", resp.StatusCode)
	}
}

func TestGroupCRUDProtectsMembers(t *testing.T) {
	f := newFixture(t)
	seedCorpTemplate(t, f)

	// An unknown template reference is rejected before anything is stored.
	resp := f.do(http.MethodPost, "/api/v1/groups", map[string]string{"name": "x", "template_id": "nope"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown template status = %d, want 400", resp.StatusCode)
	}

	resp = f.do(http.MethodPost, "/api/v1/groups", map[string]string{"name": "一层", "template_id": ""})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create group status = %d", resp.StatusCode)
	}
	g := decode[store.APGroup](t, resp)
	if g.ID == "" || g.Name != "一层" {
		t.Fatalf("group = %+v", g)
	}

	// Rename and bind the template in one update.
	resp = f.do(http.MethodPut, "/api/v1/groups/"+g.ID, map[string]string{"name": "一层办公区", "template_id": "corp"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update group status = %d", resp.StatusCode)
	}
	upd := decode[store.APGroup](t, resp)
	if upd.Name != "一层办公区" || upd.TemplateID != "corp" {
		t.Errorf("updated group = %+v", upd)
	}

	// Bind the pending device: membership is recorded but its template waits
	// until adoption.
	resp = f.do(http.MethodPut, "/api/v1/devices/"+testMAC+"/group", map[string]string{"group_id": g.ID})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bind status = %d", resp.StatusCode)
	}
	d := decode[store.Device](t, resp)
	if d.GroupID != g.ID {
		t.Errorf("group_id = %q, want %q", d.GroupID, g.ID)
	}

	// A non-empty group cannot be deleted: detaching silently would leave the
	// devices on a template nobody chose.
	resp = f.do(http.MethodDelete, "/api/v1/groups/"+g.ID, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("delete with members status = %d, want 409", resp.StatusCode)
	}

	resp = f.do(http.MethodPut, "/api/v1/devices/"+testMAC+"/group", map[string]string{"group_id": ""})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("unbind status = %d, want 200", resp.StatusCode)
	}
	resp = f.do(http.MethodDelete, "/api/v1/groups/"+g.ID, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete empty group status = %d, want 204", resp.StatusCode)
	}
}

// A pending device joining a group inherits that group's template at adoption
// instead of silently falling back to the placeholder default.
func TestAdoptUsesGroupTemplate(t *testing.T) {
	f := newFixture(t)
	seedCorpTemplate(t, f)

	g := decode[store.APGroup](t, f.do(http.MethodPost, "/api/v1/groups",
		map[string]string{"name": "一层", "template_id": "corp"}))
	if resp := f.do(http.MethodPut, "/api/v1/devices/"+testMAC+"/group",
		map[string]string{"group_id": g.ID}); resp.StatusCode != http.StatusOK {
		t.Fatalf("bind status = %d", resp.StatusCode)
	}

	res := decode[struct {
		Rev      int    `json:"rev"`
		Template string `json:"template_id"`
	}](t, f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/adopt", map[string]string{}))
	if res.Template != "corp" {
		t.Errorf("adopted template = %q, want corp (group binding)", res.Template)
	}
	d, err := f.store.GetDevice(context.Background(), testMAC)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if d.TemplateID != "corp" || d.ConfigRev != res.Rev {
		t.Errorf("device after adopt = %+v", d)
	}
}

// Binding an already adopted device to a group with a different template must
// push the new revision immediately; recording only intent would leave the AP
// on the old configuration while the UI claimed success.
func TestBindAdoptedDeviceAppliesGroupTemplate(t *testing.T) {
	f := newFixture(t)
	seedCorpTemplate(t, f)
	f.adopt()

	g := decode[store.APGroup](t, f.do(http.MethodPost, "/api/v1/groups",
		map[string]string{"name": "一层", "template_id": "corp"}))

	d := decode[store.Device](t, f.do(http.MethodPut,
		"/api/v1/devices/"+testMAC+"/group", map[string]string{"group_id": g.ID}))
	if d.GroupID != g.ID {
		t.Errorf("group_id = %q, want %q", d.GroupID, g.ID)
	}
	if d.TemplateID != "corp" {
		t.Errorf("template_id = %q, want corp", d.TemplateID)
	}
	if d.ConfigRev != 2 {
		t.Errorf("config_rev = %d, want 2", d.ConfigRev)
	}
	if d.State != store.StateAdopted {
		t.Errorf("state = %q, want adopted", d.State)
	}
}

func TestBindUnknownGroupIsRejected(t *testing.T) {
	f := newFixture(t)
	resp := f.do(http.MethodPut, "/api/v1/devices/"+testMAC+"/group", map[string]string{"group_id": "nope"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
