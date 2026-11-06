package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/naoki66/ac-controller/internal/proto"
	"github.com/naoki66/ac-controller/internal/store"
)

// hello sends a minimal registration from the seeded device and returns the
// controller's reply, which is where queued actions are delivered.
func (f *fixture) helloWith(actions []proto.ActionResult) proto.HelloReply {
	f.t.Helper()
	body := proto.Hello{
		MAC:        testMAC,
		Model:      "XR1710G",
		Firmware:   "20261026",
		Hostname:   "ap250",
		AppliedRev: 1,
		Actions:    actions,
	}
	resp := f.do(http.MethodPost, "/api/v1/ap/hello", body)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		f.t.Fatalf("hello status = %d", resp.StatusCode)
	}
	return decode[proto.HelloReply](f.t, resp)
}

func (f *fixture) adopt() {
	f.t.Helper()
	resp := f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/adopt", map[string]string{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("adopt status = %d", resp.StatusCode)
	}
}

// An action must survive the round trip: queued -> delivered in a hello reply
// -> reported back -> completed and no longer delivered.
func TestDeviceActionLifecycle(t *testing.T) {
	f := newFixture(t)
	f.adopt()

	resp := f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/actions",
		map[string]string{"type": proto.ActionWifiReload})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create action status = %d, want 202", resp.StatusCode)
	}
	created := decode[store.Action](t, resp)
	if created.ID == "" || created.State != store.ActionPending {
		t.Fatalf("created action = %+v", created)
	}

	reply := f.helloWith(nil)
	if len(reply.Actions) != 1 || reply.Actions[0].ID != created.ID {
		t.Fatalf("delivered actions = %+v, want id %s", reply.Actions, created.ID)
	}
	if reply.Actions[0].Type != proto.ActionWifiReload {
		t.Errorf("delivered type = %q", reply.Actions[0].Type)
	}

	// Reporting the result completes it, and the same reply must not hand the
	// command out again.
	reply = f.helloWith([]proto.ActionResult{{
		ID: created.ID, Type: proto.ActionWifiReload, State: proto.ActionResultDone,
	}})
	if len(reply.Actions) != 0 {
		t.Errorf("actions after result = %+v, want none", reply.Actions)
	}

	list := decode[struct {
		Actions []store.Action `json:"actions"`
	}](t, f.do(http.MethodGet, "/api/v1/devices/"+testMAC+"/actions", nil))
	if len(list.Actions) != 1 || list.Actions[0].State != store.ActionDone {
		t.Errorf("listed actions = %+v, want one done", list.Actions)
	}
}

// Repeated results are legitimate — a hello can be retried — and must not
// resurrect the action.
func TestDeviceActionDuplicateResult(t *testing.T) {
	f := newFixture(t)
	f.adopt()
	created := decode[store.Action](t, f.do(http.MethodPost,
		"/api/v1/devices/"+testMAC+"/actions", map[string]string{"type": proto.ActionReboot}))
	f.helloWith(nil)

	for i := 0; i < 2; i++ {
		f.helloWith([]proto.ActionResult{{
			ID: created.ID, Type: proto.ActionReboot, State: proto.ActionResultFailed,
			Message: "sysupgrade busy",
		}})
	}

	list := decode[struct {
		Actions []store.Action `json:"actions"`
	}](t, f.do(http.MethodGet, "/api/v1/devices/"+testMAC+"/actions", nil))
	if len(list.Actions) != 1 || list.Actions[0].State != store.ActionFailed {
		t.Errorf("listed actions = %+v, want one failed", list.Actions)
	}
}

func TestDeviceActionValidation(t *testing.T) {
	f := newFixture(t)

	// Not adopted yet: commands are only meaningful for managed APs.
	resp := f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/actions",
		map[string]string{"type": proto.ActionReboot})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("pending device status = %d, want 409", resp.StatusCode)
	}

	resp = f.do(http.MethodPost, "/api/v1/devices/aa:bb:cc:dd:ee:ff/actions",
		map[string]string{"type": proto.ActionReboot})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown device status = %d, want 404", resp.StatusCode)
	}

	resp = f.do(http.MethodPost, "/api/v1/devices/not-a-mac/actions",
		map[string]string{"type": proto.ActionReboot})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed MAC status = %d, want 400", resp.StatusCode)
	}

	f.adopt()
	resp = f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/actions",
		map[string]string{"type": "shell"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown type status = %d, want 400", resp.StatusCode)
	}

	resp = f.do(http.MethodGet, "/api/v1/devices/"+testMAC+"/actions?limit=500", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad limit status = %d, want 400", resp.StatusCode)
	}
}

// A successful factory reset must also end the controller's adoption: the AP
// just restored its pre-controller config, so re-sending the package would
// undo the reset on the very next heartbeat.
func TestFactoryResetResultReleasesDevice(t *testing.T) {
	f := newFixture(t)
	f.adopt()

	created := decode[store.Action](t, f.do(http.MethodPost,
		"/api/v1/devices/"+testMAC+"/actions", map[string]string{"type": proto.ActionFactoryReset}))
	if created.Type != proto.ActionFactoryReset {
		t.Fatalf("created action = %+v", created)
	}
	f.helloWith(nil)

	reply := f.helloWith([]proto.ActionResult{{
		ID: created.ID, Type: proto.ActionFactoryReset, State: proto.ActionResultDone,
	}})
	if len(reply.Actions) != 0 {
		t.Errorf("actions after factory reset = %+v, want none", reply.Actions)
	}
	if reply.State != store.StatePending || reply.DesiredRev != 0 {
		t.Errorf("reply state = %q rev = %d, want pending/0", reply.State, reply.DesiredRev)
	}

	d, err := f.store.GetDevice(context.Background(), testMAC)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if d.State != store.StatePending || d.TemplateID != "" {
		t.Errorf("device after factory reset = %+v, want pending without template", d)
	}
}
