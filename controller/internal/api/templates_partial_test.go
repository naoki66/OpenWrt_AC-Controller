package api

import (
	"net/http"
	"testing"

	"github.com/naoki66/ac-controller/controller/internal/provision"
	"github.com/naoki66/ac-controller/shared/model"
)

// The SSID page and the ports page edit the same shared template from two
// directions. These tests pin the partial-update contract: an SSID save must
// never rewrite the layer-2 fabric, and a rename must never need the SSIDs.

func TestTemplateSSIDsPartialUpdate(t *testing.T) {
	f := newFixture(t)

	// Change ONLY the SSID list; bridge and radios must survive untouched.
	newSSIDs := []model.SSID{{
		SSID:     "Replaced",
		Bands:    []string{model.Band2G},
		Network:  "ac_vlan10",
		Security: model.SecWPA2,
		PSK:      "12345678",
	}}
	resp := f.do(http.MethodPut, "/api/v1/templates/default/ssids", map[string]any{"ssids": newSSIDs})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put ssids status = %d", resp.StatusCode)
	}
	saved := decode[model.Template](t, resp)
	if len(saved.SSIDs) != 1 || saved.SSIDs[0].SSID != "Replaced" {
		t.Fatalf("saved ssids = %+v", saved.SSIDs)
	}
	if saved.Bridge.Name != "br-lan" || len(saved.Bridge.VLANs) == 0 {
		t.Errorf("bridge was clobbered: %+v", saved.Bridge)
	}
	if len(saved.Radios) == 0 {
		t.Errorf("radios were clobbered")
	}

	// Read back through the store: what the API returned is what is stored.
	stored, err := f.store.GetTemplate(t.Context(), "default")
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	if len(stored.SSIDs) != 1 || stored.SSIDs[0].SSID != "Replaced" {
		t.Fatalf("stored ssids = %+v", stored.SSIDs)
	}
	if stored.Bridge.Name != provision.DefaultTemplate().Bridge.Name {
		t.Errorf("stored bridge name = %q", stored.Bridge.Name)
	}
}

func TestTemplateSSIDsRejectsInvalid(t *testing.T) {
	f := newFixture(t)

	// An enabled SSID without a network is invalid, so the stored template
	// must stay unchanged.
	resp := f.do(http.MethodPut, "/api/v1/templates/default/ssids",
		map[string]any{"ssids": []model.SSID{{
			SSID: "Broken", Bands: []string{model.Band2G}, Security: model.SecOpen,
		}}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid ssids status = %d, want 400", resp.StatusCode)
	}

	// Unknown fields are a client bug; reject rather than silently ignore.
	resp = f.do(http.MethodPut, "/api/v1/templates/default/ssids",
		map[string]any{"ssids": []model.SSID{}, "bridge": map[string]any{}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d, want 400", resp.StatusCode)
	}

	// Unknown template.
	resp = f.do(http.MethodPut, "/api/v1/templates/nope/ssids",
		map[string]any{"ssids": []model.SSID{}})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing template status = %d, want 404", resp.StatusCode)
	}
}

func TestTemplateRename(t *testing.T) {
	f := newFixture(t)

	resp := f.do(http.MethodPut, "/api/v1/templates/default/name",
		map[string]string{"name": "  站点主模板  "})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rename status = %d", resp.StatusCode)
	}
	renamed := decode[model.Template](t, resp)
	if renamed.Name != "站点主模板" {
		t.Errorf("name = %q, want trimmed 站点主模板", renamed.Name)
	}
	if renamed.ID != "default" {
		t.Errorf("id changed to %q", renamed.ID)
	}
	if len(renamed.SSIDs) != len(provision.DefaultTemplate().SSIDs) {
		t.Errorf("ssids changed by rename: %d", len(renamed.SSIDs))
	}

	// Empty name fails validation and leaves the stored name intact.
	resp = f.do(http.MethodPut, "/api/v1/templates/default/name", map[string]string{"name": "   "})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty rename status = %d, want 400", resp.StatusCode)
	}
	stored, err := f.store.GetTemplate(t.Context(), "default")
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	if stored.Name != "站点主模板" {
		t.Errorf("stored name = %q, want unchanged", stored.Name)
	}
}
