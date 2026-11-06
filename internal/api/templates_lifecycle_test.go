package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/naoki66/ac-controller/internal/model"
	"github.com/naoki66/ac-controller/internal/store"
)

// A template that a device or a group still points at must not disappear:
// nothing would repair those objects until the next package generation failed.
func TestTemplateDeleteProtection(t *testing.T) {
	f := newFixture(t)
	seedCorpTemplate(t, f)

	// Referenced by a device.
	f.adopt()
	refs := decode[struct {
		Refs map[string]store.TemplateRef `json:"refs"`
	}](t, f.do(http.MethodGet, "/api/v1/templates", nil))
	if refs.Refs["default"].Devices != 1 {
		t.Errorf("default refs = %+v, want one device", refs.Refs["default"])
	}
	resp := f.do(http.MethodDelete, "/api/v1/templates/default", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("delete referenced template = %d, want 409", resp.StatusCode)
	}

	// Referenced by a group.
	f.do(http.MethodPost, "/api/v1/groups",
		map[string]string{"name": "一层", "template_id": "corp"}).Body.Close()
	resp = f.do(http.MethodDelete, "/api/v1/templates/corp", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("delete group-referenced template = %d, want 409", resp.StatusCode)
	}

	// Once nothing points at it, deletion succeeds.
	for _, group := range decode[struct {
		Groups []store.APGroup `json:"groups"`
	}](t, f.do(http.MethodGet, "/api/v1/groups", nil)).Groups {
		f.do(http.MethodDelete, "/api/v1/groups/"+group.ID, nil).Body.Close()
	}
	resp = f.do(http.MethodDelete, "/api/v1/templates/corp", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete unreferenced template = %d, want 204", resp.StatusCode)
	}
}

// The last template is the adoption fallback; even with no references it must
// stay so the default path keeps working.
func TestTemplateDeleteKeepsLast(t *testing.T) {
	f := newFixture(t)
	// Release leaves the device row but detaches it from the template.
	f.adopt()
	f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/release", map[string]string{}).Body.Close()

	resp := f.do(http.MethodDelete, "/api/v1/templates/default", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("delete last template = %d, want 409", resp.StatusCode)
	}
}

func TestCloneTemplate(t *testing.T) {
	f := newFixture(t)
	seedCorpTemplate(t, f)

	resp := f.do(http.MethodPost, "/api/v1/templates/corp/clone", map[string]string{})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("clone status = %d", resp.StatusCode)
	}
	clone := decode[model.Template](t, resp)
	if !strings.HasPrefix(clone.ID, "corp-copy-") {
		t.Errorf("clone id = %q, want corp-copy-*", clone.ID)
	}
	if clone.Name != "Corp 副本" {
		t.Errorf("clone name = %q", clone.Name)
	}
	if len(clone.SSIDs) != 1 || clone.SSIDs[0].SSID != "Corp" {
		t.Errorf("clone did not copy the SSID list: %+v", clone.SSIDs)
	}

	// Editing the clone must not touch the source.
	clone.SSIDs[0].SSID = "Clone"
	body := struct {
		SSIDs []model.SSID `json:"ssids"`
	}{SSIDs: clone.SSIDs}
	if resp := f.do(http.MethodPut, "/api/v1/templates/"+clone.ID+"/ssids", body); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("update clone status = %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	src := decode[model.Template](t, f.do(http.MethodGet, "/api/v1/templates/corp", nil))
	if src.SSIDs[0].SSID != "Corp" {
		t.Errorf("source template was modified by clone edit: %+v", src.SSIDs)
	}

	// Explicit ids are honoured, but may not collide or equal the source.
	resp = f.do(http.MethodPost, "/api/v1/templates/corp/clone", map[string]string{"id": "corp"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("clone onto source id = %d, want 400", resp.StatusCode)
	}
	resp = f.do(http.MethodPost, "/api/v1/templates/corp/clone", map[string]string{"id": "default"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("clone onto existing id = %d, want 409", resp.StatusCode)
	}
	resp = f.do(http.MethodPost, "/api/v1/templates/nope/clone", map[string]string{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("clone missing source = %d, want 404", resp.StatusCode)
	}
}
