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
	"time"

	"github.com/naoki66/ac-controller/controller/internal/keys"
	"github.com/naoki66/ac-controller/controller/internal/store"
	"github.com/naoki66/ac-controller/shared/proto"
)

// newTelemetryFixture builds a controller with a known sampling interval so the
// throttle can be exercised deterministically.
func newTelemetryFixture(t *testing.T, interval time.Duration) *apFixture {
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
	key, err := keys.FromSeed(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	srv := &Server{
		Store:          st,
		ACID:           "feedfacefeedfacefeedfacefeedface",
		Key:            key,
		SampleInterval: interval,
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		st.Close()
	})
	f := &apFixture{t: t, store: st, server: ts}
	f.login(defaultAdminUsername, defaultAdminPassword)
	return f
}

func TestParseRange(t *testing.T) {
	cases := map[string]time.Duration{
		"":     6 * time.Hour,
		"30m":  30 * time.Minute,
		"1h":   time.Hour,
		"24h":  24 * time.Hour,
		"7d":   7 * 24 * time.Hour,
		" 6h ": 6 * time.Hour,
	}
	for in, want := range cases {
		got, err := parseRange(in)
		if err != nil {
			t.Errorf("parseRange(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseRange(%q) = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"6", "six", "0h", "-1h", "90d", "6w"} {
		if _, err := parseRange(bad); err == nil {
			t.Errorf("parseRange(%q) should have failed", bad)
		}
	}
}

// A chart must come back with roughly the same number of dots whatever range is
// asked for: a 7 day view returning 10000 points would be unreadable anyway.
func TestBucketForKeepsPointCountBounded(t *testing.T) {
	for _, d := range []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour, 7 * 24 * time.Hour} {
		b := bucketFor(d)
		points := int(d.Seconds()) / b
		if points > telemetryPoints+1 {
			t.Errorf("range %v: bucket %ds gives %d points, want <= %d", d, b, points, telemetryPoints)
		}
		if b < 60 {
			t.Errorf("range %v: bucket %ds is finer than the sample interval", d, b)
		}
	}
}

func TestTelemetryRequiresMACAndValidRange(t *testing.T) {
	f := newTelemetryFixture(t, 0)

	resp := f.do(http.MethodGet, "/api/v1/telemetry/clients")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing mac: status = %d, want 400", resp.StatusCode)
	}

	resp = f.do(http.MethodGet, "/api/v1/telemetry/clients?mac="+testMAC+"&range=soon")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad range: status = %d, want 400", resp.StatusCode)
	}
}

// The throttle is what keeps a 20s heartbeat from writing 4320 rows a day per
// client. Without it the database grows an order of magnitude faster for no
// additional chart resolution.
func TestHelloSamplesAreThrottled(t *testing.T) {
	throttled := newTelemetryFixture(t, time.Hour)

	hello := proto.Hello{
		MAC:     testMAC,
		Clients: []proto.ClientStat{{MAC: "aa:bb:cc:dd:ee:01", Signal: -50, RxRateMbps: 800}},
		Radios:  []proto.RadioStat{{Radio: "ac_radio1", Band: "5g", Up: true, Channel: 36, Utilization: 10}},
	}
	for i := 0; i < 3; i++ {
		resp, _ := throttled.hello("", hello)
		resp.Body.Close()
	}

	clients, radios, err := throttled.store.CountSamples(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if clients != 1 || radios != 1 {
		t.Errorf("throttled: clients=%d radios=%d, want 1/1 (three hellos, one sample)", clients, radios)
	}

	// Same three hellos with no interval configured: every one is stored.
	open := newTelemetryFixture(t, 0)
	for i := 0; i < 3; i++ {
		resp, _ := open.hello("", hello)
		resp.Body.Close()
	}
	clients, radios, err = open.store.CountSamples(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if clients != 3 || radios != 3 {
		t.Errorf("unthrottled: clients=%d radios=%d, want 3/3", clients, radios)
	}
}

// An AP that reports nil for both lists must not write telemetry either, but
// must still be accepted — that is what an older agent looks like.
func TestHelloWithoutTelemetryWritesNothing(t *testing.T) {
	f := newTelemetryFixture(t, 0)
	resp, _ := f.hello("", proto.Hello{MAC: testMAC})
	resp.Body.Close()

	clients, radios, err := f.store.CountSamples(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if clients != 0 || radios != 0 {
		t.Errorf("clients=%d radios=%d, want 0/0", clients, radios)
	}
}

// End to end through the API: three hellos, then read the series back.
func TestTelemetryEndpoints(t *testing.T) {
	f := newTelemetryFixture(t, 0)
	ctx := context.Background()

	resp, _ := f.hello("", proto.Hello{
		MAC: testMAC,
		Clients: []proto.ClientStat{
			{MAC: "aa:bb:cc:dd:ee:01", SSID: "SSID-A_5G", Band: "5g", Signal: -50, RxRateMbps: 1200, TxRateMbps: 900},
		},
		Radios: []proto.RadioStat{
			{Radio: "ac_radio0", Band: "2g", Up: true, Channel: 4, HTMode: "HE20", WidthMHz: 20, Utilization: 45},
			{Radio: "ac_radio1", Band: "5g", Up: true, Channel: 36, HTMode: "EHT160", WidthMHz: 160, Utilization: 12},
		},
	})
	resp.Body.Close()

	clientResp := f.do(http.MethodGet, "/api/v1/telemetry/clients?mac=aa:bb:cc:dd:ee:01&range=1h")
	var cs struct {
		Mac    string              `json:"mac"`
		Bucket int                 `json:"bucket"`
		Points []store.ClientPoint `json:"points"`
	}
	decodeInto(t, clientResp, &cs)
	if len(cs.Points) != 1 {
		t.Fatalf("client points = %+v, want 1", cs.Points)
	}
	if cs.Points[0].Signal != -50 || cs.Points[0].RxRate != 1200 {
		t.Errorf("client point = %+v", cs.Points[0])
	}

	radioResp := f.do(http.MethodGet, "/api/v1/telemetry/radios?range=1h")
	var rs struct {
		Points []store.RadioPoint `json:"points"`
	}
	decodeInto(t, radioResp, &rs)
	if len(rs.Points) != 2 {
		t.Fatalf("radio points = %d, want 2 (%+v)", len(rs.Points), rs.Points)
	}
	if rs.Points[0].Channel != 4 || rs.Points[1].Channel != 36 {
		t.Errorf("radio points = %+v", rs.Points)
	}

	countResp := f.do(http.MethodGet, "/api/v1/telemetry/counts?range=1h")
	var cnt struct {
		Points []store.ClientCountPoint `json:"points"`
	}
	decodeInto(t, countResp, &cnt)
	if len(cnt.Points) != 1 || cnt.Points[0].Clients != 1 {
		t.Errorf("counts = %+v", cnt.Points)
	}

	latest := f.do(http.MethodGet, "/api/v1/radios")
	var lr struct {
		Radios []store.RadioPoint `json:"radios"`
	}
	decodeInto(t, latest, &lr)
	if len(lr.Radios) != 2 {
		t.Fatalf("latest radios = %+v, want 2", lr.Radios)
	}
	if lr.Radios[0].HTMode != "HE20" || lr.Radios[1].HTMode != "EHT160" {
		t.Errorf("latest radios lost htmode: %+v", lr.Radios)
	}

	// The device row must not be disturbed by any of this.
	d, err := f.store.GetDevice(ctx, testMAC)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if d.Hostname != "unknown" {
		t.Errorf("hostname = %q", d.Hostname)
	}
}

// The RF page reads the "now" view, so a radio the AP stops reporting must
// vanish on that very hello — not when retention pruning happens to erase its
// last sample. Renamed UCI sections were the field case.
func TestRadiosConvergeOnHello(t *testing.T) {
	f := newTelemetryFixture(t, time.Hour) // throttled on purpose: state is per hello

	resp, _ := f.hello("", proto.Hello{
		MAC: testMAC,
		Radios: []proto.RadioStat{
			{Radio: "ac_radio0", Band: "2g", Up: true, Channel: 4},
			{Radio: "ac_radio1", Band: "5g", Up: true, Channel: 36},
		},
	})
	resp.Body.Close()

	resp, _ = f.hello("", proto.Hello{
		MAC:    testMAC,
		Radios: []proto.RadioStat{{Radio: "radio0", Band: "2g", Up: true, Channel: 4}},
	})
	resp.Body.Close()

	latest := f.do(http.MethodGet, "/api/v1/radios")
	var lr struct {
		Radios []store.RadioPoint `json:"radios"`
	}
	decodeInto(t, latest, &lr)
	if len(lr.Radios) != 1 || lr.Radios[0].Radio != "radio0" {
		t.Fatalf("latest after rename = %+v, want only radio0", lr.Radios)
	}

	// A hello that carries no radio list at all (older agent) must not clear
	// the view: nil means "not reported", not "no radios".
	resp, _ = f.hello("", proto.Hello{MAC: testMAC})
	resp.Body.Close()
	latest = f.do(http.MethodGet, "/api/v1/radios")
	decodeInto(t, latest, &lr)
	if len(lr.Radios) != 1 || lr.Radios[0].Radio != "radio0" {
		t.Fatalf("nil radios cleared the view: %+v", lr.Radios)
	}
}

// Telemetry is management data: it must sit behind the admin token like the
// rest of the REST surface, not next to the open health endpoint.
func TestTelemetryRequiresAdminToken(t *testing.T) {
	f := newFixtureWithAuth(t, "s3cret", "")
	resp := f.do(http.MethodGet, "/api/v1/telemetry/counts", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	ok := f.doAuth("s3cret", http.MethodGet, "/api/v1/telemetry/counts", nil)
	ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Errorf("with token: status = %d, want 200", ok.StatusCode)
	}
}

func decodeInto(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
