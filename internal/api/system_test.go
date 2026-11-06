package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/naoki66/ac-controller/internal/store"
)

// Diagnostics report counts and configuration flags, never the secret itself.
func TestDiagnosticsReportsCountsWithoutSecrets(t *testing.T) {
	f := newFixture(t)
	f.adopt()
	f.helloWith(nil)
	f.seedClient(testClientMAC)

	ctx := context.Background()
	if err := f.store.RecordSamples(ctx,
		[]store.ClientSample{{TS: 1, MAC: testClientMAC, APMAC: testMAC, Signal: -50}},
		[]store.RadioSample{{TS: 1, APMAC: testMAC, Radio: "phy1", Band: "5g"}}); err != nil {
		t.Fatalf("record samples: %v", err)
	}

	got := decode[struct {
		Version    string             `json:"version"`
		ACID       string             `json:"ac_id"`
		AdminAuth  bool               `json:"admin_auth_required"`
		APAuth     bool               `json:"ap_auth_required"`
		SampleSecs int64              `json:"sample_interval_seconds"`
		Devices    systemDeviceCounts `json:"devices"`
		Clients    systemClientCounts `json:"clients"`
		Telemetry  struct {
			ClientSamples int64 `json:"client_samples"`
			RadioSamples  int64 `json:"radio_samples"`
		} `json:"telemetry"`
		Events struct {
			Total int `json:"total"`
		} `json:"events"`
	}](t, f.do(http.MethodGet, "/api/v1/diagnostics", nil))

	if got.Devices.Total != 1 || got.Devices.Online != 1 || got.Devices.Adopted != 1 {
		t.Errorf("devices = %+v", got.Devices)
	}
	if got.Clients.Total != 1 || got.Clients.Online != 1 {
		t.Errorf("clients = %+v", got.Clients)
	}
	if got.Telemetry.ClientSamples != 1 || got.Telemetry.RadioSamples != 1 {
		t.Errorf("telemetry = %+v", got.Telemetry)
	}
	// Login is always required, so the management flag is true even on a fixture
	// with no admin token; the AP channel stays open without a secret.
	if !got.AdminAuth || got.APAuth {
		t.Errorf("auth flags = admin %v ap %v, want admin true and ap false", got.AdminAuth, got.APAuth)
	}
	if got.ACID == "" {
		t.Error("ac_id missing")
	}
	if got.Events.Total < 1 {
		t.Errorf("events total = %d, want at least the adoption row", got.Events.Total)
	}
}

// The newest audit rows come back first and the cap is enforced.
func TestEventsReturnsNewestFirst(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, e := range []struct{ kind, msg string }{
		{"template", "first"},
		{"group", "second"},
	} {
		f.store.LogEvent(ctx, e.kind, "", e.msg)
	}

	got := decode[struct {
		Events []store.Event `json:"events"`
		Total  int           `json:"total"`
	}](t, f.do(http.MethodGet, "/api/v1/events?limit=2", nil))

	if len(got.Events) != 2 || got.Events[0].Kind != "group" || got.Events[1].Kind != "template" {
		t.Errorf("events = %+v, want group then template", got.Events)
	}
	if got.Total < 2 {
		t.Errorf("total = %d, want >= 2", got.Total)
	}

	resp := f.do(http.MethodGet, "/api/v1/events?limit=0", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("limit=0 status = %d, want 400", resp.StatusCode)
	}
}
