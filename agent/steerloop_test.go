package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naoki66/ac-controller/shared/model"
	"github.com/naoki66/ac-controller/shared/proto"
)

// steerFixture wires a resident-style agent with a fake runner, a neighbours
// cache and a controllable clock. The probe replies are set so
// steeringState() returns "none" — otherwise the loop correctly yields and
// every test would pass vacuously.
type steerFixture struct {
	agent  *Agent
	loop   *SteerLoop
	runner *fakeRunner
	now    time.Time
}

func newSteerFixture(t *testing.T) *steerFixture {
	t.Helper()
	root := t.TempDir()
	runner := &fakeRunner{
		failOn: map[string]error{
			"/etc/init.d/nrsyncd enabled": errors.New("not installed"),
			"/etc/init.d/usteer enabled":  errors.New("not installed"),
			"/etc/init.d/dawn enabled":    errors.New("not installed"),
		},
		replies: map[string]string{
			"ubus call network.wireless status": `{"radio1":{"up":true,"config":{"band":"5g"},"interfaces":[{"ifname":"phy0.1-ap0","config":{"ssid":"SSID-A"}}]}}`,
			"iw dev phy0.1-ap0 station dump": "Station aa:bb:cc:dd:ee:ff\n" +
				" signal: -85\n" +
				" connected time: 120 seconds\n",
		},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &Agent{
		Config: Config{Root: root},
		Log:    log,
		state:  State{AppliedRev: 9},
		Runner: runner,
		Applier: &Applier{
			StateDir: root,
			Runner:   runner,
			Log:      log,
		},
		Clients: &ClientCollector{Runner: runner, Log: log},
	}
	loop := NewSteerLoop()
	loop.Agent = a
	f := &steerFixture{agent: a, loop: loop, runner: runner, now: time.Unix(1793698800, 0)}
	loop.Now = func() time.Time { return f.now }
	// The guard must share the fixture's clock, or its pacing would be measured
	// against wall time while the loop runs on the injected one.
	a.btm = newBTMGuard()
	a.btm.now = loop.Now
	return f
}

func (f *steerFixture) advance(d time.Duration) { f.now = f.now.Add(d) }

// seedCandidates writes the on-disk neighbours cache that candidatesFor reads:
// a sibling BSS of the same SSID.
func (f *steerFixture) seedCandidates() {
	body := []byte(`{"phy0.1-ap0":[["aa:bb:cc:dd:ee:01","SSID-A","00aabbccddee01"]]}`)
	if err := os.WriteFile(filepath.Join(f.agent.Applier.StateDir, neighborsCacheFile), body, 0o644); err != nil {
		panic(err)
	}
}

// seedCandidateMap writes an explicit cache, used by binding tests that need
// candidates on more than one SSID.
func (f *steerFixture) seedCandidateMap(body string) {
	if err := os.WriteFile(filepath.Join(f.agent.Applier.StateDir, neighborsCacheFile), []byte(body), 0o644); err != nil {
		panic(err)
	}
}

func callsJoined(f *steerFixture) string { return strings.Join(f.runner.Calls(), "\n") }

func defaultPolicy() *model.SteerPolicy {
	return &model.SteerPolicy{
		BtmSNR:         -75,
		BtmConsecutive: 3,
		ForceAfterMS:   0,
		BanMS:          0,
		MinConnectedMS: 30000,
		CooldownMS:     120000,
	}
}

// Three consecutive weak samples fire exactly one BTM carrying the AC's
// neighbour candidates; the cooldown then suppresses repeats.
func TestSteerLoopSendsBTMAfterConsecutiveWeakSamples(t *testing.T) {
	f := newSteerFixture(t)
	f.seedCandidates()
	p := defaultPolicy()
	f.loop.SetPolicy(p)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		f.loop.tick(ctx)
	}
	if got := strings.Count(callsJoined(f), "bss_transition_request"); got != 0 {
		t.Fatalf("BTM fired before the streak completed:\n%s", callsJoined(f))
	}

	f.loop.tick(ctx)
	calls := callsJoined(f)
	if got := strings.Count(calls, "bss_transition_request"); got != 1 {
		t.Fatalf("got %d BTM calls after 3 weak samples, want 1:\n%s", got, calls)
	}
	if !strings.Contains(calls, "00aabbccddee01") {
		t.Errorf("BTM does not carry the AC's neighbour report:\n%s", calls)
	}
	if !strings.Contains(calls, `"disassociation_imminent":false`) {
		t.Errorf("BTM uses the wrong ubus key:\n%s", calls)
	}
	if active, counts := f.loop.snapshot(); !active {
		t.Errorf("policy should report active (counts %+v)", counts)
	}

	// Cooldown: further weak ticks must not repeat the suggestion.
	f.advance(30 * time.Second)
	f.loop.tick(ctx)
	if got := strings.Count(callsJoined(f), "bss_transition_request"); got != 1 {
		t.Errorf("cooldown did not suppress repeats (%d BTM calls):\n%s", got, callsJoined(f))
	}
}

// nrsyncd only distributes 11k neighbour lists; it does not steer clients, so
// the policy loop must keep running when it is the enabled daemon.
func TestSteerLoopRunsAlongsideNeighborOnlyDaemon(t *testing.T) {
	f := newSteerFixture(t)
	delete(f.runner.failOn, "/etc/init.d/nrsyncd enabled")
	f.seedCandidates()
	f.loop.SetPolicy(defaultPolicy())
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		f.loop.tick(ctx)
	}
	if !strings.Contains(callsJoined(f), "bss_transition_request") {
		t.Fatalf("BTM did not fire alongside nrsyncd:\n%s", callsJoined(f))
	}
}

// Force path: the client ignored the BTM and is still associated after
// ForceAfterMS, so it is deauthenticated with a ban. Before the deadline
// nothing may happen.
func TestSteerLoopForcesAfterIgnoredBTM(t *testing.T) {
	f := newSteerFixture(t)
	f.seedCandidates()
	p := defaultPolicy()
	p.ForceAfterMS = 10 * 1000
	p.BanMS = 30 * 1000
	f.loop.SetPolicy(p)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		f.loop.tick(ctx)
	}
	if !strings.Contains(callsJoined(f), "bss_transition_request") {
		t.Fatalf("BTM never fired:\n%s", callsJoined(f))
	}

	f.advance(5 * time.Second)
	f.loop.tick(ctx)
	if strings.Contains(callsJoined(f), "del_client") {
		t.Fatalf("forced before the deadline:\n%s", callsJoined(f))
	}

	f.advance(6 * time.Second)
	f.loop.tick(ctx)
	calls := callsJoined(f)
	if !strings.Contains(calls, "del_client") {
		t.Fatalf("no force kick after the deadline:\n%s", calls)
	}
	if !strings.Contains(calls, `"ban_time":30000`) {
		t.Errorf("force kick carries no ban_time:\n%s", calls)
	}
	if _, counts := f.loop.snapshot(); counts.Forced != 1 || counts.BTM != 1 {
		t.Errorf("counts = %+v, want BTM=1 Forced=1", counts)
	}
}

// ForceAfterMS=0 must never deauthenticate, however weak the client stays.
func TestSteerLoopWithoutForceNeverKicks(t *testing.T) {
	f := newSteerFixture(t)
	f.seedCandidates()
	f.loop.SetPolicy(defaultPolicy())
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		f.loop.tick(ctx)
		f.advance(15 * time.Second)
	}
	if calls := callsJoined(f); strings.Contains(calls, "del_client") {
		t.Errorf("force path ran despite ForceAfterMS=0:\n%s", calls)
	}
}

// A client whose signal is fine is never touched.
func TestSteerLoopStrongClientUntouched(t *testing.T) {
	f := newSteerFixture(t)
	f.seedCandidates()
	f.runner.replies["iw dev phy0.1-ap0 station dump"] =
		"Station aa:bb:cc:dd:ee:ff\n signal: -55\n connected time: 600 seconds\n"
	f.loop.SetPolicy(defaultPolicy())
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		f.loop.tick(ctx)
	}
	if calls := callsJoined(f); strings.Contains(calls, "bss_transition_request") {
		t.Errorf("strong client was steered:\n%s", calls)
	}
}

// A client that just associated is protected, whatever its signal.
func TestSteerLoopRespectsMinConnected(t *testing.T) {
	f := newSteerFixture(t)
	f.seedCandidates()
	f.runner.replies["iw dev phy0.1-ap0 station dump"] =
		"Station aa:bb:cc:dd:ee:ff\n signal: -85\n connected time: 5 seconds\n"
	f.loop.SetPolicy(defaultPolicy())
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		f.loop.tick(ctx)
	}
	if calls := callsJoined(f); strings.Contains(calls, "bss_transition_request") {
		t.Errorf("fresh association was steered:\n%s", calls)
	}
}

// A station entry survives a roam until mac80211's inactivity timeout; the
// loop must not keep BTM-ing a client that already sits on the peer AP.
func TestSteerLoopIgnoresStaleStation(t *testing.T) {
	f := newSteerFixture(t)
	f.seedCandidates()
	f.runner.replies["iw dev phy0.1-ap0 station dump"] =
		"Station aa:bb:cc:dd:ee:ff\n signal: -85\n inactive time: 120000 ms\n connected time: 300 seconds\n"
	f.loop.SetPolicy(defaultPolicy())
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		f.loop.tick(ctx)
	}
	if calls := callsJoined(f); strings.Contains(calls, "bss_transition_request") {
		t.Errorf("stale station was steered:\n%s", calls)
	}
}

// Nowhere to go: a weak client with an empty candidate pool is left alone.
// Kicking it would just disconnect it.
func TestSteerLoopNoCandidatesNoBTM(t *testing.T) {
	f := newSteerFixture(t)
	f.loop.SetPolicy(defaultPolicy())
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		f.loop.tick(ctx)
	}
	if calls := callsJoined(f); strings.Contains(calls, "bss_transition_request") {
		t.Errorf("BTM sent without candidates:\n%s", calls)
	}
}

// A daemon that actually steers clients wins: the loop stands down entirely.
func TestSteerLoopYieldsToSteeringDaemon(t *testing.T) {
	for _, daemon := range []string{"usteer", "dawn"} {
		t.Run(daemon, func(t *testing.T) {
			f := newSteerFixture(t)
			f.seedCandidates()
			delete(f.runner.failOn, "/etc/init.d/"+daemon+" enabled")
			f.loop.SetPolicy(defaultPolicy())
			ctx := context.Background()

			for i := 0; i < 5; i++ {
				f.loop.tick(ctx)
			}
			if calls := callsJoined(f); strings.Contains(calls, "bss_transition_request") {
				t.Errorf("loop did not yield to %s:\n%s", daemon, calls)
			}
		})
	}
}

// Parsing: defaults are filled in, invalid values rejected.
func TestParseSteerPolicy(t *testing.T) {
	p, err := parseSteerPolicy([]byte(`{"btm_snr":-75}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.BtmConsecutive != 3 || p.MinConnectedMS != 30000 || p.CooldownMS != 120000 {
		t.Errorf("defaults not applied: %+v", p)
	}

	if _, err := parseSteerPolicy([]byte(`{"btm_snr":0}`)); err == nil {
		t.Error("btm_snr=0 accepted; must be a negative dBm value")
	}
	if _, err := parseSteerPolicy([]byte(`{not json`)); err == nil {
		t.Error("garbage accepted")
	}
}

// The policy ingested from a package is normalized before use.
func TestSteerPolicyIngestNormalization(t *testing.T) {
	p, err := parseSteerPolicy([]byte(`{"btm_snr":-80,"btm_consecutive":2,"force_after_ms":5000}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.BanMS != 30000 {
		t.Errorf("BanMS = %d, want the 30s default when forcing is enabled", p.BanMS)
	}
}

const boundClientMAC = "aa:bb:cc:dd:ee:ff"
const selfAPMAC = "8c:de:52:11:22:33"

// A client already on the bound SSID (and AP) is a success: the operator's
// intent wins over the weak-signal policy, so it must not be steered or
// force-kicked however bad its signal is.
func TestSteerBindingSatisfiedSuppressesWeakSignal(t *testing.T) {
	f := newSteerFixture(t)
	f.seedCandidates()
	f.agent.Identity.MAC = selfAPMAC
	f.loop.SetPolicy(defaultPolicy())
	f.loop.SetBindings([]proto.ClientBinding{{
		MAC:   boundClientMAC,
		SSID:  "SSID-A",
		APMAC: selfAPMAC,
	}})
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		f.loop.tick(ctx)
	}
	calls := callsJoined(f)
	if strings.Contains(calls, "bss_transition_request") || strings.Contains(calls, "del_client") {
		t.Errorf("bound client on the right SSID was steered:\n%s", calls)
	}
	if active, _ := f.loop.snapshot(); !active {
		t.Error("bindings alone should report the loop active")
	}
}

// A bound client on the wrong SSID gets a non-imminent BTM naming only BSSes
// that serve the target SSID; the 5-minute cooldown then spaces repeats.
func TestSteerBindingSendsBTMToTargetSSID(t *testing.T) {
	f := newSteerFixture(t)
	f.seedCandidateMap(`{"phy0.1-ap0":[["aa:bb:cc:dd:ee:01","SSID-A","00aabbccddee01"]],` +
		`"phy1.0-ap0":[["aa:bb:cc:dd:ee:02","GUEST","00aabbccddee02"]]}`)
	f.loop.SetBindings([]proto.ClientBinding{{MAC: boundClientMAC, SSID: "GUEST"}})
	ctx := context.Background()

	f.loop.tick(ctx)
	calls := callsJoined(f)
	if !strings.Contains(calls, "bss_transition_request") {
		t.Fatalf("no BTM for a client on the wrong SSID:\n%s", calls)
	}
	if !strings.Contains(calls, "00aabbccddee02") {
		t.Errorf("BTM does not carry the target SSID's neighbour report:\n%s", calls)
	}
	if strings.Contains(calls, "00aabbccddee01") {
		t.Errorf("BTM carries a neighbour on the wrong SSID:\n%s", calls)
	}
	if !strings.Contains(calls, `"disassociation_imminent":false`) {
		t.Errorf("magnet BTM must be advisory, not imminent:\n%s", calls)
	}

	f.advance(time.Minute)
	f.loop.tick(ctx)
	if got := strings.Count(callsJoined(f), "bss_transition_request"); got != 1 {
		t.Errorf("cooldown did not suppress a repeat (%d BTM calls):\n%s", got, callsJoined(f))
	}

	f.advance(4*time.Minute + time.Second)
	f.loop.tick(ctx)
	if got := strings.Count(callsJoined(f), "bss_transition_request"); got != 2 {
		t.Errorf("got %d BTM calls after cooldown, want 2", got)
	}
	if _, counts := f.loop.snapshot(); counts.BTM != 2 {
		t.Errorf("counts = %+v, want BTM=2", counts)
	}
}

// A named AP that is not this unit means the binding is unmet even on the
// right SSID, so the client is still steered toward a BSS that satisfies it.
func TestSteerBindingRequiresNamedAP(t *testing.T) {
	f := newSteerFixture(t)
	f.seedCandidates()
	f.agent.Identity.MAC = selfAPMAC
	f.loop.SetBindings([]proto.ClientBinding{{
		MAC:   boundClientMAC,
		SSID:  "SSID-A",
		APMAC: "8c:de:52:99:99:99",
	}})
	ctx := context.Background()

	f.loop.tick(ctx)
	if calls := callsJoined(f); !strings.Contains(calls, "bss_transition_request") {
		t.Errorf("binding on another AP was treated as satisfied:\n%s", calls)
	}
}

// Nowhere to go: no candidate serves the bound SSID, so the client is left
// alone rather than disconnected.
func TestSteerBindingWithoutCandidatesLeavesClient(t *testing.T) {
	f := newSteerFixture(t)
	f.seedCandidates() // SSID-A only
	f.loop.SetBindings([]proto.ClientBinding{{MAC: boundClientMAC, SSID: "GUEST"}})
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		f.loop.tick(ctx)
		f.advance(10 * time.Second)
	}
	if calls := callsJoined(f); strings.Contains(calls, "bss_transition_request") {
		t.Errorf("BTM sent without a matching candidate:\n%s", calls)
	}
	f.loop.SetBindings([]proto.ClientBinding{})
	if active, _ := f.loop.snapshot(); active {
		t.Error("loop still active after bindings were cleared")
	}
}

// The cadence is chosen by what the last pass saw. A weak client is the reason
// the fast interval exists, so the very first weak sample must select it —
// waiting for the consecutive threshold would delay detection by design.
func TestTickSelectsFastCadenceForWeakClient(t *testing.T) {
	f := newSteerFixture(t)
	f.loop.SetPolicy(defaultPolicy())
	f.seedCandidates()

	if !f.loop.tick(context.Background()) {
		t.Fatal("a weak client must promote the loop to the fast cadence")
	}
}

// A site where every client is strong pays only the slow sweep.
func TestTickSelectsIdleCadenceWhenAllClientsAreStrong(t *testing.T) {
	f := newSteerFixture(t)
	f.loop.SetPolicy(defaultPolicy())
	f.runner.mu.Lock()
	f.runner.replies["iw dev phy0.1-ap0 station dump"] = "Station aa:bb:cc:dd:ee:ff\n" +
		" signal: -55\n" +
		" connected time: 120 seconds\n"
	f.runner.mu.Unlock()

	if f.loop.tick(context.Background()) {
		t.Fatal("a strong client must not keep the loop on the fast cadence")
	}
}

// Nothing associated at all is the common case on an idle AP.
func TestTickSelectsIdleCadenceWithNoClients(t *testing.T) {
	f := newSteerFixture(t)
	f.loop.SetPolicy(defaultPolicy())
	f.runner.mu.Lock()
	f.runner.replies["iw dev phy0.1-ap0 station dump"] = ""
	f.runner.mu.Unlock()

	if f.loop.tick(context.Background()) {
		t.Fatal("an empty BSS must not keep the loop on the fast cadence")
	}
}

// A kick that has been sent but not yet resolved must be watched at the fast
// cadence: that window is when the client either moves or has to be forced.
func TestTickSelectsFastCadenceWhileKickIsPending(t *testing.T) {
	f := newSteerFixture(t)
	p := defaultPolicy()
	p.ForceAfterMS = 20000
	p.CooldownMS = 60000
	f.loop.SetPolicy(p)
	f.seedCandidates()

	// First pass: three weak samples, then the BTM goes out.
	for i := 0; i < 3; i++ {
		f.loop.tick(context.Background())
	}
	if !strings.Contains(callsJoined(f), "bss_transition_request") {
		t.Fatal("no BTM was sent, so there is no pending kick to test")
	}

	f.runner.mu.Lock()
	f.runner.replies["iw dev phy0.1-ap0 station dump"] = "Station aa:bb:cc:dd:ee:ff\n" +
		" signal: -55\n" +
		" connected time: 300 seconds\n"
	f.runner.mu.Unlock()

	if !f.loop.tick(context.Background()) {
		t.Fatal("a pending kick must keep the fast cadence even when the signal looks fine")
	}
}

// The policy path must stop advising a client that ignores every suggestion.
// Observed on 10.0.30.250: one client sat at -81 dBm for hours while a BTM
// went out every cooldown, because "sent" and "worked" were the same event.
func TestSteerLoopStopsAdvisingAClientThatNeverMoves(t *testing.T) {
	f := newSteerFixture(t)
	p := defaultPolicy()
	f.loop.SetPolicy(p)

	f.loop.mu.Lock()
	f.loop.merged = map[string][][]string{
		"phy0.1-ap0": {{"aa:bb:cc:dd:ee:01", "SSID-A", "00aabbccddee01"}},
	}
	f.loop.mu.Unlock()

	ifc := managedInterface{ifname: "phy0.1-ap0", ssid: "SSID-A"}
	c := proto.ClientStat{MAC: "aa:bb:cc:dd:ee:0f", Ifname: "phy0.1-ap0", SSID: "SSID-A", Signal: -85}

	base := f.now
	cooldown := time.Duration(p.CooldownMS) * time.Millisecond
	elapsed := time.Duration(0)

	// One extra round: the fourth must be withheld.
	for attempt := 0; attempt <= btmMaxUnanswered; attempt++ {
		f.now = base.Add(elapsed)
		// Connected time grows with the clock, so the association moment stays
		// fixed — the client is on the same association throughout.
		c.ConnectedSeconds = 600 + int(elapsed/time.Second)
		for i := 0; i < p.BtmConsecutive; i++ {
			f.loop.processClient(context.Background(), p, f.now, ifc, c)
		}
		elapsed += cooldown + time.Second
	}

	sent := strings.Count(callsJoined(f), "bss_transition_request")
	if sent != btmMaxUnanswered {
		t.Fatalf("BTM requests = %d, want %d (one per chance, then silence)", sent, btmMaxUnanswered)
	}
	if got := f.agent.btm.counts(); got == 0 {
		t.Fatal("the withheld suggestion was not counted")
	}
}

// A client that roams must be eligible immediately: the advice worked.
func TestSteerLoopAdvisesAgainAfterTheClientRoams(t *testing.T) {
	f := newSteerFixture(t)
	p := defaultPolicy()
	f.loop.SetPolicy(p)

	f.loop.mu.Lock()
	f.loop.merged = map[string][][]string{
		"phy0.1-ap0": {{"aa:bb:cc:dd:ee:01", "SSID-A", "00aabbccddee01"}},
		"phy0.1-ap1": {{"aa:bb:cc:dd:ee:01", "SSID-A", "00aabbccddee01"}},
	}
	f.loop.mu.Unlock()

	base := f.now
	cooldown := time.Duration(p.CooldownMS) * time.Millisecond
	elapsed := time.Duration(0)
	c := proto.ClientStat{MAC: "aa:bb:cc:dd:ee:0f", Ifname: "phy0.1-ap0", SSID: "SSID-A", Signal: -85}
	ifc := managedInterface{ifname: "phy0.1-ap0", ssid: "SSID-A"}

	for attempt := 0; attempt < btmMaxUnanswered; attempt++ {
		f.now = base.Add(elapsed)
		c.ConnectedSeconds = 600 + int(elapsed/time.Second)
		for i := 0; i < p.BtmConsecutive; i++ {
			f.loop.processClient(context.Background(), p, f.now, ifc, c)
		}
		elapsed += cooldown + time.Second
	}

	// The client moved to the sibling BSS on the same AP and is weak there.
	f.now = base.Add(elapsed)
	ifc = managedInterface{ifname: "phy0.1-ap1", ssid: "SSID-A"}
	c.Ifname = "phy0.1-ap1"
	c.ConnectedSeconds = 60 // fresh association, past the min-connected guard
	for i := 0; i < p.BtmConsecutive; i++ {
		f.loop.processClient(context.Background(), p, f.now, ifc, c)
	}

	sent := strings.Count(callsJoined(f), "bss_transition_request")
	if sent != btmMaxUnanswered+1 {
		t.Fatalf("BTM requests = %d, want %d (the roam earns a new suggestion)", sent, btmMaxUnanswered+1)
	}
}
