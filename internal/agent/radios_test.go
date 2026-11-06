package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/naoki66/ac-controller/internal/proto"
)

// Captured from `ubus call network.wireless status` on an XR1710G, shaped like
// the transition from the old ac_radio* naming to the platform's radio0..radio2:
// the managed radios now carry the platform name, and one stale ac_radio0
// section is still present but disabled until the next apply removes it.
const radioStatusFixture = `{
  "ac_radio0": {"up": false, "disabled": true, "config": {"band": "2g", "channel": "auto",
             "htmode": "HE20", "txpower": 28, "radio": 0}, "interfaces": []},
  "radio0": {"up": true, "disabled": false,
             "config": {"band": "2g", "channel": "auto", "htmode": "HE20", "country": "US", "txpower": 28, "radio": 0},
             "interfaces": [{"section": "ac_ssid_home_2g", "ifname": "phy0.0-ap0",
                             "config": {"ssid": "SSID-A"}}]},
  "radio1": {"up": true, "disabled": false,
             "config": {"band": "5g", "channel": 36, "htmode": "EHT160", "txpower": 30, "radio": 1},
             "interfaces": [{"section": "ac_ssid_home_5g", "ifname": "phy0.1-ap0",
                             "config": {"ssid": "SSID-A_5G"}},
                            {"section": "ac_ssid_oplt_5g", "ifname": "phy0.1-ap1",
                             "config": {"ssid": "SSID-B_5G"}}]},
  "radio2": {"up": true, "disabled": false,
             "config": {"band": "6g", "channel": "37", "htmode": "EHT320", "txpower": 30, "radio": 2},
             "interfaces": [{"section": "ac_ssid_lt2k_6g", "ifname": "phy0.2-ap0",
                             "config": {"ssid": "SSID-A_6G"}}]}
}`

const hostapd2g = `{"status":"ENABLED","freq":2427,"channel":4,"phy":"phy0.0",
  "airtime":{"time":6255032,"time_busy":874168,"utilization":45}}`
const hostapd5g = `{"status":"ENABLED","freq":5180,"channel":36,"phy":"phy0.1",
  "airtime":{"utilization":12}}`
const hostapd6g = `{"status":"ENABLED","freq":6115,"channel":37,"phy":"phy0.2",
  "airtime":{"utilization":3}}`

func newRadioCollector(t *testing.T, outputs map[string]string) (*RadioCollector, *mapRunner) {
	t.Helper()
	r := &mapRunner{outputs: outputs}
	return &RadioCollector{Runner: r, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, r
}

// The channel in the sample must be the one hostapd is actually using, not the
// "auto" that UCI still holds. This is the whole reason the collector exists.
func TestCollectRadiosPrefersLiveChannel(t *testing.T) {
	c, _ := newRadioCollector(t, map[string]string{
		"status": radioStatusFixture,
		"ubus call hostapd.phy0.0-ap0 get_status": hostapd2g,
		"ubus call hostapd.phy0.1-ap0 get_status": hostapd5g,
		"ubus call hostapd.phy0.2-ap0 get_status": hostapd6g,
	})

	clients := []proto.ClientStat{
		{MAC: "aa:bb:cc:dd:ee:01", Ifname: "phy0.1-ap0"},
		{MAC: "aa:bb:cc:dd:ee:02", Ifname: "phy0.1-ap1"},
		{MAC: "aa:bb:cc:dd:ee:03", Ifname: "phy0.0-ap0"},
	}
	got := c.Collect(context.Background(), clients)
	if len(got) != 4 {
		t.Fatalf("radios = %d, want 4 (%+v)", len(got), got)
	}

	byName := map[string]proto.RadioStat{}
	for _, r := range got {
		byName[r.Radio] = r
	}

	two := byName["radio0"]
	if two.Channel != 4 {
		t.Errorf("2.4G channel = %d, want 4 (live channel, not UCI 'auto')", two.Channel)
	}
	if two.FreqMHz != 2427 || two.Phy != "phy0.0" {
		t.Errorf("2.4G freq/phy = %d/%q, want 2427/phy0.0", two.FreqMHz, two.Phy)
	}
	if two.Utilization != 45 {
		t.Errorf("2.4G utilization = %d, want 45", two.Utilization)
	}
	if two.WidthMHz != 20 || two.HTMode != "HE20" {
		t.Errorf("2.4G width/htmode = %d/%q, want 20/HE20", two.WidthMHz, two.HTMode)
	}
	if two.Country != "US" {
		t.Errorf("2.4G country = %q, want US (from UCI option country)", two.Country)
	}
	if !two.Up {
		t.Error("radio0 should be up")
	}
	if two.Clients != 1 {
		t.Errorf("2.4G clients = %d, want 1", two.Clients)
	}

	five := byName["radio1"]
	if five.Channel != 36 || five.WidthMHz != 160 || five.Utilization != 12 {
		t.Errorf("5G sample = %+v", five)
	}
	// Both BSSes on the radio count towards it, not just the one probed.
	if five.Clients != 2 {
		t.Errorf("5G clients = %d, want 2 (both BSSes)", five.Clients)
	}

	six := byName["radio2"]
	if six.Channel != 37 || six.WidthMHz != 320 {
		t.Errorf("6G sample = %+v", six)
	}

	// A disabled radio still appears: the controller needs to know it exists.
	// This is the stale ac_radio0 section a device keeps until an apply with
	// the platform naming removes it.
	off := byName["ac_radio0"]
	if off.Up {
		t.Error("ac_radio0 is disabled and must not be reported up")
	}
	if off.Channel != 0 {
		t.Errorf("disabled radio channel = %d, want 0 (unconfigured)", off.Channel)
	}
}

// hostapd may not answer (older build, radio mid-setup). The configured channel
// is then the best available answer, and utilisation must say "unknown" rather
// than silently reading as 0% busy.
func TestCollectRadiosFallsBackToConfiguredChannel(t *testing.T) {
	c, runner := newRadioCollector(t, map[string]string{"status": radioStatusFixture})

	got := c.Collect(context.Background(), nil)
	byName := map[string]proto.RadioStat{}
	for _, r := range got {
		byName[r.Radio] = r
	}
	five := byName["radio1"]
	if five.Channel != 36 {
		t.Errorf("channel = %d, want 36 from the configured value", five.Channel)
	}
	if five.Utilization != -1 {
		t.Errorf("utilization = %d, want -1 (unknown, not 0)", five.Utilization)
	}
	if five.Clients != 0 {
		t.Errorf("clients = %d, want 0 with no station list", five.Clients)
	}
	// A radio on channel "auto" must still be probed: hostapd is the only thing
	// that knows which channel ACS picked.
	probed := false
	for _, call := range runner.calls {
		if call == "ubus call hostapd.phy0.0-ap0 get_status" {
			probed = true
		}
	}
	if !probed {
		t.Errorf("radio0 (channel auto) was never probed: %v", runner.calls)
	}
	if two := byName["radio0"]; two.Channel != 0 {
		t.Errorf("auto channel with no hostapd answer = %d, want 0", two.Channel)
	}
}

// Radios must come back in a stable order: two consecutive samples are compared
// by position in several places and an unstable order would look like churn.
func TestCollectRadiosIsSorted(t *testing.T) {
	c, _ := newRadioCollector(t, map[string]string{"status": radioStatusFixture})
	got := c.Collect(context.Background(), nil)
	for i := 1; i < len(got); i++ {
		if got[i-1].Radio > got[i].Radio {
			t.Fatalf("radios not sorted: %v", got)
		}
	}
}

func TestHtmodeWidth(t *testing.T) {
	cases := map[string]int{
		"HE20":    20,
		"HE40":    40,
		"EHT80":   80,
		"EHT160":  160,
		"EHT320":  320,
		"HT20":    20,
		"NOHT":    0,
		"":        0,
		"VHT80":   80,
		"HE80_80": 80, // the second width is not what a chart wants
	}
	for in, want := range cases {
		if got := htmodeWidth(in); got != want {
			t.Errorf("htmodeWidth(%q) = %d, want %d", in, got, want)
		}
	}
}

// netifd dumps a UCI option as a JSON string on one build and a number on
// another; both shapes have to survive or the channel silently disappears.
func TestWirelessStatusAcceptsStringOrNumber(t *testing.T) {
	c, _ := newRadioCollector(t, map[string]string{
		"status": `{"r": {"up": true, "config": {"band": "5g", "channel": 149,
		           "htmode": "EHT80", "txpower": "23"},
		           "interfaces": [{"ifname": "phy0-ap0"}]}}`,
		"ubus call hostapd.phy0-ap0 get_status": `{"channel": 149, "freq": 5745, "phy": "phy0"}`,
	})
	got := c.Collect(context.Background(), nil)
	if len(got) != 1 {
		t.Fatalf("radios = %d, want 1", len(got))
	}
	if got[0].Channel != 149 {
		t.Errorf("channel = %d, want 149", got[0].Channel)
	}
	if got[0].TxPower != 23 {
		t.Errorf("txpower = %d, want 23 (string \"23\")", got[0].TxPower)
	}
	// No airtime object at all must read as unknown, not as idle.
	if got[0].Utilization != -1 {
		t.Errorf("utilization = %d, want -1", got[0].Utilization)
	}
}

// A heartbeat without telemetry enabled must not fabricate an empty list: nil
// tells the controller "this agent does not report radios".
func TestAgentWithoutRadioCollectorReportsNil(t *testing.T) {
	a := &Agent{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if got := a.collectRadios(context.Background(), nil); got != nil {
		t.Errorf("collectRadios = %v, want nil", got)
	}
}

// The sample is cached between cycles: three ubus calls per heartbeat would be
// a measurable cost on the AP for data the controller throws away anyway.
func TestRadioSampleIsCachedBetweenCycles(t *testing.T) {
	a := &Agent{
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Radios: &RadioCollector{Runner: &mapRunner{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
	}
	a.cycles = 1
	first := a.collectRadios(context.Background(), nil)
	if first != nil {
		t.Fatalf("first sample = %v, want nil (no ubus output)", first)
	}
	a.cycles = 2
	if got := a.collectRadios(context.Background(), nil); got != nil {
		t.Errorf("cycle 2 returned a fresh sample: %v", got)
	}
}

// A radio sample has no consumer while the controller is gone: it cannot be
// reported and peer announcements carry only the BSS set. The offline cadence
// must therefore be longer — with exactly one fresh sample at each transition,
// so the first online report never carries an offline-era sample.
func TestRadioSampleSlowsDownWhileControlIsLost(t *testing.T) {
	r := &mapRunner{outputs: map[string]string{"status": radioStatusFixture}}
	a := &Agent{
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Radios: &RadioCollector{Runner: r, Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
	}
	sampled := func() int {
		before := len(r.calls)
		a.collectRadios(context.Background(), nil)
		return len(r.calls) - before
	}

	a.cycles = 1
	if sampled() == 0 {
		t.Fatal("cycle 1 did not sample")
	}
	a.cycles = 2
	if sampled() != 0 {
		t.Error("cycle 2 sampled again; the sample is cached between cycles")
	}

	// The transition samples even though cycle 4 is not a multiple of the
	// offline period.
	a.helloFailures = rediscoverAfterFailures
	a.cycles = 4
	if sampled() == 0 {
		t.Error("no sample when control was lost")
	}
	for c := 5; c < radioSampleCyclesOffline; c++ {
		a.cycles = c
		if sampled() != 0 {
			t.Errorf("cycle %d sampled before the offline period elapsed", c)
		}
	}
	a.cycles = radioSampleCyclesOffline
	if sampled() == 0 {
		t.Error("the offline period did not sample")
	}

	// Recovery samples immediately: the controller is about to receive it.
	a.helloFailures = 0
	a.cycles = radioSampleCyclesOffline + 1
	if sampled() == 0 {
		t.Error("no sample when control returned")
	}
}

// --- the ubusd socket path -------------------------------------------------

// fakeUbusReplies serves the objects a radio sample asks for, so the socket path
// can be compared against the CLI path on identical data.
func fakeUbusReplies(t *testing.T, byPath map[string]string) ubusTransport {
	t.Helper()
	ids := make(map[string]uint32, len(byPath))
	tables := make(map[uint32]map[string]any, len(byPath))
	var next uint32 = 100
	for path, raw := range byPath {
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		next++
		ids[path] = next
		tables[next] = m
	}
	return &fakeTransport{
		lookupFn: func(_ context.Context, obj string) (uint32, error) {
			id, ok := ids[obj]
			if !ok {
				return 0, statusError("lookup "+obj, ubusStatusNotFound)
			}
			return id, nil
		},
		invokeFn: func(_ context.Context, id uint32, method string) (map[string]any, error) {
			m, ok := tables[id]
			if !ok {
				return nil, statusError("invoke "+method, ubusStatusNotFound)
			}
			return m, nil
		},
	}
}

func radioFixtures() map[string]string {
	return map[string]string{
		"network.wireless":   radioStatusFixture,
		"hostapd.phy0.0-ap0": hostapd2g,
		"hostapd.phy0.1-ap0": hostapd5g,
		"hostapd.phy0.2-ap0": hostapd6g,
	}
}

// TestRadioCollectorSocketMatchesCLI is the claim behind moving the calls off
// fork+exec: the socket path produces the same sample, field for field. Any
// difference here — a lost type, a renamed field, a container level too deep —
// would show up as a changed number on the dashboard, which is exactly what this
// swap must not do.
func TestRadioCollectorSocketMatchesCLI(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cli := &RadioCollector{
		Runner: &mapRunner{outputs: map[string]string{
			"status": radioStatusFixture,
			"ubus call hostapd.phy0.0-ap0 get_status": hostapd2g,
			"ubus call hostapd.phy0.1-ap0 get_status": hostapd5g,
			"ubus call hostapd.phy0.2-ap0 get_status": hostapd6g,
		}},
		Log: log,
	}
	socketRunner := &mapRunner{}
	socket := &RadioCollector{
		Runner: socketRunner,
		Log:    log,
		Ubus:   testUbusClient(fakeUbusReplies(t, radioFixtures())),
	}

	clients := []proto.ClientStat{
		{MAC: "aa:bb:cc:dd:ee:01", Ifname: "phy0.1-ap0"},
		{MAC: "aa:bb:cc:dd:ee:02", Ifname: "phy0.1-ap1"},
	}
	want := cli.Collect(context.Background(), clients)
	if len(want) != 4 {
		t.Fatalf("CLI sample has %d radios, expected the fixture's 4", len(want))
	}
	got := socket.Collect(context.Background(), clients)

	if len(socketRunner.calls) != 0 {
		t.Fatalf("socket path still forked: %v", socketRunner.calls)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("socket sample differs from the CLI sample:/n got %+v\nwant %+v", got, want)
	}
}

// TestRadioCollectorFallsBackToCLI keeps the escape hatch honest: when the socket
// is missing or ubusd refuses, the sample must still arrive at the old cost.
func TestRadioCollectorFallsBackToCLI(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	broken := &fakeTransport{
		lookupFn: func(context.Context, string) (uint32, error) {
			return 0, errors.New("dial unix /var/run/ubus/ubus.sock: connect: no such file or directory")
		},
	}
	runner := &mapRunner{outputs: map[string]string{
		"status": radioStatusFixture,
		"ubus call hostapd.phy0.0-ap0 get_status": hostapd2g,
		"ubus call hostapd.phy0.1-ap0 get_status": hostapd5g,
		"ubus call hostapd.phy0.2-ap0 get_status": hostapd6g,
	}}
	c := &RadioCollector{Runner: runner, Log: log, Ubus: testUbusClient(broken)}

	got := c.Collect(context.Background(), nil)
	if len(got) != 4 {
		t.Fatalf("fallback sample has %d radios, want 4", len(got))
	}
	if len(runner.calls) == 0 {
		t.Fatal("the CLI was never used, so this did not test the fallback")
	}
}
