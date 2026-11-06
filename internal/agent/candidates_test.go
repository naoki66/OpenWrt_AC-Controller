package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Bodies below were captured from 10.0.30.250 (hostapd rrm_nr_get_own). They
// pin the offsets the parser depends on: everything after the 13-byte fixed
// part is optional subelements the agent must not choke on.
const (
	nr2gHome = "00000000005bef48000051040e0603000400"
	nr5gHome = "00000000005cef58200081240e0603033200"
	nr6gHome = "00000000005def40200089250e0603001f00"
)

func TestParseNeighborReportReadsOperatingPoint(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		bssid   string
		opclass uint8
		channel uint8
	}{
		{"2.4 GHz", nr2gHome, "00:00:00:00:00:5b", 81, 4},
		{"5 GHz", nr5gHome, "00:00:00:00:00:5c", 129, 36},
		{"6 GHz", nr6gHome, "00:00:00:00:00:5d", 137, 37},
	}
	for _, c := range cases {
		info, ok := parseNeighborReport(c.body)
		if !ok {
			t.Fatalf("%s: body rejected", c.name)
		}
		if info.BSSID != c.bssid || info.OpClass != c.opclass || info.Channel != c.channel {
			t.Errorf("%s: got %s class=%d ch=%d, want %s class=%d ch=%d",
				c.name, info.BSSID, info.OpClass, info.Channel, c.bssid, c.opclass, c.channel)
		}
	}
	if _, ok := parseNeighborReport("00aabb"); ok {
		t.Error("a truncated body was accepted")
	}
	if _, ok := parseNeighborReport("zzzz"); ok {
		t.Error("non-hex was accepted")
	}
}

func TestBandOfOpClass(t *testing.T) {
	cases := map[uint8]string{
		81: "2g", 82: "2g", 84: "2g",
		115: "5g", 121: "5g", 129: "5g", 130: "5g",
		131: "6g", 137: "6g",
		0: "", 100: "",
	}
	for op, want := range cases {
		if got := bandOfOpClass(op); got != want {
			t.Errorf("op class %d: got %q, want %q", op, got, want)
		}
	}
}

// The client's own BSS must never appear in its own candidate list, and a
// candidate on the same class and channel is not a roam — it is the same
// airtime with a reassociation attached.
func TestRankCandidatesDropsSelfAndSameChannel(t *testing.T) {
	self := neighborInfo{BSSID: "00:00:00:00:00:5c", OpClass: 129, Channel: 36}
	cands := [][]string{
		{"00:00:00:00:00:5c", "SSID-A_5G", nr5gHome},                               // self
		{"02:00:00:00:00:5c", "SSID-B_5G", "02000000005cef58200081240e0603033200"}, // same class+channel
		{"0a:00:00:00:00:5c", "SSID-D_5G", "0a000000005cef58200081280e0603033200"}, // 5 GHz ch 40
		{"00:00:00:00:005b", "SSID-A", nr2gHome},                                   // malformed bssid, kept
	}
	got := rankCandidates(cands, self)
	var ids []string
	for _, e := range got {
		ids = append(ids, strings.ToLower(e[0]))
	}
	joined := strings.Join(ids, ",")
	if strings.Contains(joined, "00:00:00:00:00:5c") {
		t.Fatalf("self BSS survived filtering: %s", joined)
	}
	if strings.Contains(joined, "02:00:00:00:00:5c") {
		t.Fatalf("same-channel candidate survived filtering: %s", joined)
	}
	if len(got) != 2 {
		t.Fatalf("got %d candidates (%s), want 2", len(got), joined)
	}
}

// Same band first, then the higher-capacity band, then channel. A 5 GHz client
// should be offered 6 GHz ahead of 2.4 GHz even though 2.4 GHz uses the lower
// channel number.
func TestRankCandidatesOrdersByBandThenChannel(t *testing.T) {
	self := neighborInfo{BSSID: "00:00:00:00:00:5c", OpClass: 129, Channel: 36}
	cands := [][]string{
		{"00:00:00:00:00:5b", "SSID-A", nr2gHome},                                  // 2.4 GHz
		{"00:00:00:00:00:5d", "SSID-A_6G", nr6gHome},                               // 6 GHz
		{"0a:00:00:00:00:5c", "SSID-D_5G", "0a000000005cef58200081280e0603033200"}, // 5 GHz ch 40
		{"0e:00:00:00:00:5c", "SSID-E_5G", "0e000000005cef58200081240e0603033200"}, // 5 GHz ch 36 → same channel as self
	}
	got := rankCandidates(cands, self)
	if len(got) != 3 {
		t.Fatalf("got %d candidates, want 3", len(got))
	}
	want := []string{"0a:00:00:00:00:5c", "00:00:00:00:00:5d", "00:00:00:00:00:5b"}
	for i, w := range want {
		if !strings.EqualFold(got[i][0], w) {
			t.Fatalf("position %d is %s, want %s (full order %v)", i, got[i][0], w, got)
		}
	}
}

// Without a self report (netifd fallback, or the first hello before the 11k
// cache arrives) nothing may be dropped except by unparsable content: an
// unknown operating point is not evidence that a candidate is bad.
func TestRankCandidatesKeepsEverythingWithoutSelfInfo(t *testing.T) {
	cands := [][]string{
		{"00:00:00:00:00:5b", "SSID-A", nr2gHome},
		{"00:00:00:00:00:5c", "SSID-A_5G", nr5gHome},
	}
	if got := rankCandidates(cands, neighborInfo{}); len(got) != 2 {
		t.Fatalf("got %d candidates without self info, want 2", len(got))
	}
}

func TestStationSupportsBTM(t *testing.T) {
	cases := []struct {
		name string
		caps stationCaps
		want bool
	}{
		// Captured from the live site: eleven of thirteen stations look like
		// this, including the one that ignored every suggestion.
		{"no rrm, no mbo", stationCaps{RRM: []int{0, 0, 0, 0, 0}}, false},
		{"neighbor report only", stationCaps{RRM: []int{1, 0, 0, 0, 0}}, true},
		{"active beacon", stationCaps{RRM: []int{66, 0, 0, 0, 0}}, true},
		{"mbo only", stationCaps{MBO: true}, true},
		{"empty bitmap", stationCaps{}, false},
	}
	for _, c := range cases {
		if got := stationSupportsBTM(c.caps); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// A station that cannot parse a neighbour report element is never sent a BTM:
// the three attempts the guard would otherwise spend are pure waste.
func TestSteerLoopSkipsClientThatCannotReadBTM(t *testing.T) {
	f := newSteerFixture(t)
	f.seedCandidates()
	f.runner.replies["ubus call hostapd.phy0.1-ap0 get_clients"] =
		`{"clients":{"aa:bb:cc:dd:ee:ff":{"rrm":[0,0,0,0,0],"mbo":false}}}`
	f.loop.SetPolicy(defaultPolicy())
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		f.loop.tick(ctx)
		f.advance(4 * time.Second)
	}
	if calls := callsJoined(f); strings.Contains(calls, "bss_transition_request") {
		t.Fatalf("BTM sent to a client that cannot parse it:\n%s", calls)
	}
	if n := f.agent.btm.unsupportedCount(); n != 1 {
		t.Fatalf("unsupported counter is %d, want 1", n)
	}
	if quiet := f.agent.btm.counts(); quiet == 0 {
		t.Fatal("guard was not silenced for the incapable client")
	}
}

// A client that does declare 11k keeps the old behaviour.
func TestSteerLoopStillSteersCapableClient(t *testing.T) {
	f := newSteerFixture(t)
	f.seedCandidates()
	f.runner.replies["ubus call hostapd.phy0.1-ap0 get_clients"] =
		`{"clients":{"aa:bb:cc:dd:ee:ff":{"rrm":[113,80,80,0,4],"mbo":false}}}`
	f.loop.SetPolicy(defaultPolicy())
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		f.loop.tick(ctx)
		f.advance(4 * time.Second)
	}
	if calls := callsJoined(f); !strings.Contains(calls, "bss_transition_request") {
		t.Fatalf("capable client was not steered:\n%s", calls)
	}
}

// hostapd being unreachable must not silence the loop: unknown is not "no".
func TestSteerLoopProceedsWhenCapabilityUnknown(t *testing.T) {
	f := newSteerFixture(t)
	f.seedCandidates()
	f.runner.failOn["ubus call hostapd.phy0.1-ap0 get_clients"] = errTestUbusDown
	f.loop.SetPolicy(defaultPolicy())
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		f.loop.tick(ctx)
		f.advance(4 * time.Second)
	}
	if calls := callsJoined(f); !strings.Contains(calls, "bss_transition_request") {
		t.Fatalf("a failed capability lookup suppressed steering:\n%s", calls)
	}
}

var errTestUbusDown = errors.New("ubus unavailable")
