package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naoki66/ac-controller/internal/proto"
)

const (
	testSelfMAC = "8c:de:52:11:22:33"
	testPeerMAC = "8c:de:52:aa:bb:cc"
)

func peerAnnouncement(t *testing.T, st proto.PeerState) []byte {
	t.Helper()
	st.Type = proto.MsgPeer
	st.V = proto.PeerVersion
	body, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal peer state: %v", err)
	}
	return body
}

// A peer announcement is stored under a normalized MAC, and anything that is
// not a well-formed announcement from a different AP is dropped. A malformed
// peer must never be able to evict or impersonate a real one.
func TestPeerIngestValidatesAndIgnoresSelf(t *testing.T) {
	bus := NewPeerBus(0, time.Minute, strings.ToUpper(testSelfMAC), testLog())

	bus.ingest(peerAnnouncement(t, proto.PeerState{
		MAC: testPeerMAC,
		BSS: []proto.NeighborBSS{{Ifname: "phy0.1-ap0", BSSID: "00:11:22:33:44:55", SSID: "SSID-A", Report: "body"}},
	}))

	snap := bus.Snapshot()
	if len(snap.APs) != 1 || snap.APs[0] != testPeerMAC {
		t.Fatalf("peers = %v, want [%s]", snap.APs, testPeerMAC)
	}
	if len(snap.BSS) != 1 || snap.BSS[0].BSSID != "00:11:22:33:44:55" {
		t.Errorf("BSS = %+v", snap.BSS)
	}

	bad := [][]byte{
		[]byte(`not json`),
		peerAnnouncement(t, proto.PeerState{MAC: strings.ToUpper(testSelfMAC)}),
		peerAnnouncement(t, proto.PeerState{MAC: "zz:zz"}),
		peerAnnouncement(t, proto.PeerState{MAC: testPeerMAC, V: proto.PeerVersion + 1}),
	}
	for _, body := range bad {
		bus.ingest(body)
	}
	if got := bus.Snapshot().APs; len(got) != 1 || got[0] != testPeerMAC {
		t.Errorf("invalid messages changed the peer table: %v", got)
	}
}

// A peer that stopped announcing must fall out of the table, otherwise an AP
// that was decommissioned keeps appearing in every neighbour list.
func TestPeerSnapshotExpiresSilentPeer(t *testing.T) {
	bus := NewPeerBus(0, 30*time.Second, testSelfMAC, testLog())
	bus.mu.Lock()
	bus.peers[testPeerMAC] = peerEntry{
		state: proto.PeerState{MAC: testPeerMAC, BSS: []proto.NeighborBSS{
			{Ifname: "phy0.1-ap0", BSSID: "00:11:22:33:44:55", SSID: "SSID-A", Report: "body"},
		}},
		seen: time.Now().Add(-5 * time.Minute),
	}
	bus.mu.Unlock()

	snap := bus.Snapshot()
	if len(snap.APs) != 0 || len(snap.BSS) != 0 {
		t.Errorf("silent peer survived: %+v", snap)
	}
}

// Empty BSS entries carry no usable report and must not reach a neighbour list.
func TestPeerSnapshotSkipsIncompleteBSS(t *testing.T) {
	bus := NewPeerBus(0, time.Minute, testSelfMAC, testLog())
	bus.ingest(peerAnnouncement(t, proto.PeerState{
		MAC: testPeerMAC,
		BSS: []proto.NeighborBSS{
			{Ifname: "phy0.1-ap0", BSSID: "", SSID: "SSID-A", Report: "body"},
			{Ifname: "phy0.2-ap0", BSSID: "00:11:22:33:44:55", SSID: "SSID-A", Report: ""},
		},
	}))
	if got := bus.Snapshot().BSS; len(got) != 0 {
		t.Errorf("incomplete entries survived: %+v", got)
	}
}

// mergeNeighborLists is what makes AP-to-AP roaming work without the controller:
// it must combine the cached controller list, this AP's own sibling BSSes and
// every peer serving the same SSID, while never writing an interface's own BSSID
// back (hostapd adds it itself).
func TestMergeNeighborLists(t *testing.T) {
	own := []proto.NeighborBSS{
		{Ifname: "phy0.1-ap0", BSSID: "00:11:22:33:44:01", SSID: "SSID-A_5G", Report: "r1"},
		{Ifname: "phy0.2-ap0", BSSID: "00:11:22:33:44:02", SSID: "SSID-A_5G", Report: "r2"},
		{Ifname: "phy0.0-ap0", BSSID: "00:11:22:33:44:00", SSID: "HOME_2G", Report: "r0"},
	}
	controller := map[string][][]string{
		"phy0.1-ap0": {
			{"00:11:22:33:44:01", "SSID-A_5G", "self"},
			{"aa:bb:cc:dd:ee:01", "SSID-A_5G", "cached"},
		},
	}
	peers := []proto.NeighborBSS{
		{Ifname: "phy1.1-ap0", BSSID: "bb:bb:cc:dd:ee:02", SSID: "SSID-A_5G", Report: "p1"},
		{Ifname: "phy1.0-ap0", BSSID: "cc:bb:cc:dd:ee:03", SSID: "HOME_2G", Report: "p2"},
	}

	got := mergeNeighborLists(controller, own, peers)
	list := got["phy0.1-ap0"]
	if len(list) != 3 {
		t.Fatalf("phy0.1-ap0 list = %v, want 3 entries", list)
	}
	// Own BSSID excluded; the sibling BSS, the cached controller entry and the
	// peer that serves the same SSID are all present, sorted by BSSID.
	want := []string{"00:11:22:33:44:02", "aa:bb:cc:dd:ee:01", "bb:bb:cc:dd:ee:02"}
	for i, bssid := range want {
		if strings.ToLower(list[i][0]) != bssid {
			t.Errorf("entry %d = %q, want %q (list %v)", i, list[i][0], bssid, list)
		}
	}
	// A BSS on another SSID must not appear: the client cannot join it.
	for _, entry := range list {
		if entry[0] == "cc:bb:cc:dd:ee:03" {
			t.Errorf("foreign SSID leaked into the list: %v", list)
		}
	}
}

// Without a local report the controller's cache is the best available answer.
func TestMergeNeighborListsFallsBackToController(t *testing.T) {
	controller := map[string][][]string{"phy0.1-ap0": {{"aa:bb:cc:dd:ee:01", "SSID-A", "r"}}}
	if got := mergeNeighborLists(controller, nil, nil); len(got) != 1 || len(got["phy0.1-ap0"]) != 1 {
		t.Errorf("fallback = %v", got)
	}
}

// A BSS whose only candidate is its own BSSID must produce no list at all.
func TestMergeNeighborListsReturnsNilWhenNothingToAdvertise(t *testing.T) {
	own := []proto.NeighborBSS{{Ifname: "phy0.1-ap0", BSSID: "00:11:22:33:44:01", SSID: "SSID-A", Report: "r"}}
	controller := map[string][][]string{"phy0.1-ap0": {{"00:11:22:33:44:01", "SSID-A", "self"}}}
	if got := mergeNeighborLists(controller, own, nil); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestPeerStateFromMapsHello(t *testing.T) {
	h := proto.Hello{
		MAC:      testSelfMAC,
		Model:    "XR1710G",
		Hostname: "ap250",
		Neighbors: []proto.NeighborBSS{
			{Ifname: "phy0.1-ap0", BSSID: "00:11:22:33:44:55", SSID: "SSID-A", Report: "r"},
		},
		Clients: []proto.ClientStat{
			{MAC: "aa:bb:cc:dd:ee:0a", Ifname: "phy0.1-ap0", SSID: "SSID-A", Band: "5g", Signal: -60, ConnectedSeconds: 120},
		},
		Radios: []proto.RadioStat{
			{Band: "5g", Channel: 36, Utilization: 20, Clients: 1},
		},
	}
	st := peerStateFrom(h)
	if st.MAC != testSelfMAC || st.Model != "XR1710G" || len(st.BSS) != 1 || len(st.Clients) != 1 || len(st.Radios) != 1 {
		t.Errorf("peer state = %+v", st)
	}
	if st.Clients[0].Signal != -60 || st.Clients[0].ConnectedSeconds != 120 {
		t.Errorf("client = %+v", st.Clients[0])
	}
	if st.Radios[0].Channel != 36 || st.Radios[0].Utilization != 20 {
		t.Errorf("radio = %+v", st.Radios[0])
	}
}

// neighborBody is the payload the agent writes to hostapd. With the controller
// unreachable the cached list must still be merged with fresh peer reports, so a
// client can be handed a working neighbour list during an AC outage.
func TestNeighborBodyMergesControllerCacheAndPeers(t *testing.T) {
	app := &Applier{StateDir: t.TempDir(), Runner: newNeighborRunner(), Log: testLog()}
	app.cacheNeighbors([]byte(`{"phy0.1-ap0":[["aa:bb:cc:dd:ee:01","SSID-A_5G","00aabbccddee01"]]}`))

	bus := NewPeerBus(0, time.Minute, testSelfMAC, testLog())
	bus.ingest(peerAnnouncement(t, proto.PeerState{
		MAC: testPeerMAC,
		BSS: []proto.NeighborBSS{
			{Ifname: "phy1.1-ap0", BSSID: "bb:bb:cc:dd:ee:02", SSID: "SSID-A_5G", Report: "00bbbbccddee02"},
			{Ifname: "phy1.0-ap0", BSSID: "cc:bb:cc:dd:ee:03", SSID: "HOME_2G", Report: "00ccbbccddee03"},
		},
	}))

	a := &Agent{
		Applier: app,
		peerBus: bus,
		Log:     testLog(),
		lastOwnNeighbors: []proto.NeighborBSS{
			{Ifname: "phy0.1-ap0", BSSID: "8c:de:52:11:22:33", SSID: "SSID-A_5G", Report: "00self"},
		},
	}
	body := a.neighborBody()
	if body == nil {
		t.Fatal("neighborBody returned nil during a controller outage")
	}
	merged, err := decodeNeighbors(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	list := merged["phy0.1-ap0"]
	if len(list) != 2 {
		t.Fatalf("list = %v, want cached entry plus the 5 GHz peer", list)
	}
	joined := string(body)
	if !strings.Contains(joined, "aa:bb:cc:dd:ee:01") || !strings.Contains(joined, "bb:bb:cc:dd:ee:02") {
		t.Errorf("merged payload lost an entry: %s", joined)
	}
	if strings.Contains(joined, "cc:bb:cc:dd:ee:03") {
		t.Errorf("2.4 GHz peer leaked into the 5 GHz list: %s", joined)
	}
}

// Even with no controller cache at all (a factory-fresh AP), live peers serving
// the same SSID are enough to build a working list.
func TestNeighborBodyWorksWithoutControllerCache(t *testing.T) {
	bus := NewPeerBus(0, time.Minute, testSelfMAC, testLog())
	bus.ingest(peerAnnouncement(t, proto.PeerState{
		MAC: testPeerMAC,
		BSS: []proto.NeighborBSS{
			{Ifname: "phy1.1-ap0", BSSID: "bb:bb:cc:dd:ee:02", SSID: "SSID-A_5G", Report: "00bb"},
		},
	}))
	a := &Agent{
		Applier: &Applier{StateDir: t.TempDir(), Runner: newNeighborRunner(), Log: testLog()},
		peerBus: bus,
		Log:     testLog(),
		lastOwnNeighbors: []proto.NeighborBSS{
			{Ifname: "phy0.1-ap0", BSSID: "8c:de:52:11:22:33", SSID: "SSID-A_5G", Report: "00self"},
		},
	}
	body := a.neighborBody()
	if body == nil {
		t.Fatal("neighborBody returned nil with peers and no controller cache")
	}
	if !strings.Contains(string(body), "bb:bb:cc:dd:ee:02") {
		t.Errorf("peer entry missing: %s", body)
	}
}

// A discovered controller that stops answering is forgotten, but only after
// three missed heartbeats; an operator-pinned address is never forgotten.
func TestStepRediscoversVanishedController(t *testing.T) {
	root := t.TempDir()
	cfg := Config{
		Root:     root,
		Runner:   newNeighborRunner(),
		Identity: Identity{MAC: testSelfMAC, Model: "XR1710G", Hostname: "ap250"},
	}
	a, err := New(cfg, testLog())
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	a.acAddr = "127.0.0.1:1"
	a.client = NewClient("http://127.0.0.1:1", "")

	for i := 1; i <= rediscoverAfterFailures; i++ {
		if err := a.Step(context.Background()); err == nil {
			t.Fatalf("step %d reported success against a dead controller", i)
		}
	}
	if a.client != nil || a.acAddr != "" {
		t.Fatalf("vanished controller was kept: client=%v addr=%q", a.client, a.acAddr)
	}
	if a.nextDiscover.IsZero() {
		t.Error("no re-discovery backoff was scheduled")
	}
	// The deferred cycle must return without broadcasting again.
	if err := a.Step(context.Background()); err != nil {
		t.Errorf("deferred cycle returned an error: %v", err)
	}
}

func TestStepNeverForgetsPinnedController(t *testing.T) {
	root := t.TempDir()
	a, err := New(Config{
		Root:     root,
		ACAddr:   "127.0.0.1:1",
		Runner:   newNeighborRunner(),
		Identity: Identity{MAC: testSelfMAC, Model: "XR1710G", Hostname: "ap250"},
	}, testLog())
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	for i := 0; i <= rediscoverAfterFailures; i++ {
		_ = a.Step(context.Background())
	}
	if !a.pinnedAC {
		t.Error("configured address was not pinned")
	}
	if a.client == nil || a.acAddr != "127.0.0.1:1" {
		t.Errorf("pinned controller was dropped: client=%v addr=%q", a.client, a.acAddr)
	}
}

// The whole point of the offline path: a controller that cannot be reached must
// not stop the agent from re-asserting 11k lists, nor from writing any config.
func TestStepKeepsNeighborsWhenControllerIsUnreachable(t *testing.T) {
	root := t.TempDir()
	runner := newNeighborRunner("phy0.1-ap0", "phy0.2-ap0")
	runner.own["hostapd.phy0.1-ap0"] = ownReportJSON("00:00:00:00:00:23", "SSID-A_5G", "00a")
	runner.own["hostapd.phy0.2-ap0"] = ownReportJSON("00:00:00:00:00:24", "SSID-A_5G", "00b")

	a, err := New(Config{
		Root:   root,
		ACAddr: "127.0.0.1:1", // nothing listens here
		Runner: runner,
		Identity: Identity{
			MAC:      testSelfMAC,
			Model:    "XR1710G",
			Firmware: "20261026",
			Hostname: "ap250",
		},
	}, testLog())
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	stateDir := filepath.Join(root, "etc", "ac")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	cache := []byte(`{"phy0.1-ap0":[["aa:bb:cc:dd:ee:01","SSID-A_5G","00aa"]]}`)
	if err := os.WriteFile(filepath.Join(stateDir, neighborsCacheFile), cache, 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	if err := a.Step(context.Background()); err == nil {
		t.Error("step reported success with an unreachable controller")
	}

	// The AP keeps advertising its own sibling BSS and the cached peer even though
	// the controller never answered.
	list := runner.currentList("phy0.1-ap0")
	entries := map[string]bool{}
	for _, e := range list {
		if len(e) > 0 {
			entries[strings.ToLower(e[0])] = true
		}
	}
	if !entries["aa:bb:cc:dd:ee:01"] || !entries["00:00:00:00:00:24"] {
		t.Errorf("offline list = %v, want cached peer and the sibling BSS", list)
	}
	if entries["00:00:00:00:00:23"] {
		t.Errorf("the BSS wrote its own BSSID back: %v", list)
	}

	// No configuration may change while the controller is unreachable.
	if _, err := os.Stat(filepath.Join(root, "etc", "config", "wireless")); !os.IsNotExist(err) {
		t.Errorf("wireless config written without a controller: %v", err)
	}
	if st := a.State(); st.AppliedRev != 0 {
		t.Errorf("applied rev = %d, want 0", st.AppliedRev)
	}
}

func ownReportJSON(bssid, ssid, report string) string {
	body, _ := json.Marshal(map[string]any{"value": []string{bssid, ssid, report}})
	return string(body)
}

// steerRunner answers the steering-daemon probe and records 11v requests. A
// usteer/dawn daemon makes the probe return it and must silence the agent;
// nrsyncd is neighbour-only and must not.
type steerRunner struct {
	mu     sync.Mutex
	calls  []string
	daemon string
	// caps is the payload for `hostapd.<if> get_clients`. Empty means the call
	// fails, which the capability gate must read as "unknown, go ahead".
	caps string
}

func (r *steerRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	key := name + " " + strings.Join(args, " ")
	r.mu.Lock()
	r.calls = append(r.calls, key)
	daemon := r.daemon
	caps := r.caps
	r.mu.Unlock()

	if r.daemon != "" && name == "/etc/init.d/"+daemon && len(args) == 1 && args[0] == "enabled" {
		return "", nil
	}
	if name == "ubus" && len(args) == 3 && args[2] == "bss_transition_request" {
		return "{}", nil
	}
	if name == "ubus" && len(args) == 3 && args[2] == "get_clients" && caps != "" {
		return caps, nil
	}
	return "", errors.New("not found")
}

func (r *steerRunner) btmCalls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		if strings.Contains(c, "bss_transition_request") {
			out = append(out, c)
		}
	}
	return out
}

func newSteerAgent(t *testing.T, runner *steerRunner, enabled bool) *Agent {
	t.Helper()
	bus := NewPeerBus(0, time.Minute, testSelfMAC, testLog())
	bus.ingest(peerAnnouncement(t, proto.PeerState{
		MAC: testPeerMAC,
		BSS: []proto.NeighborBSS{
			{Ifname: "phy1.1-ap0", BSSID: "bb:bb:cc:dd:ee:02", SSID: "SSID-A_5G", Report: "00bbbbccddee02"},
		},
	}))
	return &Agent{
		Config:  Config{PeerSteer: enabled},
		Applier: &Applier{Runner: runner, Log: testLog()},
		Runner:  runner,
		peerBus: bus,
		Log:     testLog(),
		btm:     newBTMGuard(),
	}
}

func weakHello() proto.Hello {
	return proto.Hello{
		MAC: testSelfMAC,
		Neighbors: []proto.NeighborBSS{
			{Ifname: "phy0.1-ap0", BSSID: "8c:de:52:11:22:33", SSID: "SSID-A_5G", Report: "own"},
		},
		Clients: []proto.ClientStat{
			{MAC: "aa:bb:cc:dd:ee:0a", Ifname: "phy0.1-ap0", SSID: "SSID-A_5G", Signal: -85, ConnectedSeconds: 120},
		},
	}
}

func TestMaybeSteerIsOffByDefault(t *testing.T) {
	runner := &steerRunner{}
	a := newSteerAgent(t, runner, false)
	a.maybeSteer(context.Background(), weakHello())
	if calls := runner.btmCalls(); len(calls) != 0 {
		t.Errorf("11v sent while disabled: %v", calls)
	}
}

func TestMaybeSteerYieldsToSteeringDaemon(t *testing.T) {
	for _, daemon := range []string{"usteer", "dawn"} {
		t.Run(daemon, func(t *testing.T) {
			runner := &steerRunner{daemon: daemon}
			a := newSteerAgent(t, runner, true)
			a.maybeSteer(context.Background(), weakHello())
			if calls := runner.btmCalls(); len(calls) != 0 {
				t.Errorf("11v sent while %s is enabled: %v", daemon, calls)
			}
		})
	}
}

// nrsyncd only distributes 11k neighbour lists; it never decides where a client
// should go, so the agent's own BTM suggestion may run alongside it.
func TestMaybeSteerRunsAlongsideNeighborOnlyDaemon(t *testing.T) {
	runner := &steerRunner{daemon: "nrsyncd"}
	a := newSteerAgent(t, runner, true)
	a.maybeSteer(context.Background(), weakHello())
	calls := runner.btmCalls()
	if len(calls) != 1 {
		t.Fatalf("11v calls = %v, want one alongside nrsyncd", calls)
	}
	if !strings.Contains(calls[0], "00bbbbccddee02") {
		t.Errorf("suggestion does not carry the peer's neighbour report: %s", calls[0])
	}
}

func TestMaybeSteerSuggestsPeerForWeakClient(t *testing.T) {
	runner := &steerRunner{}
	a := newSteerAgent(t, runner, true)

	a.maybeSteer(context.Background(), weakHello())

	calls := runner.btmCalls()
	if len(calls) != 1 {
		t.Fatalf("11v calls = %v, want exactly one", calls)
	}
	if !strings.Contains(calls[0], "00bbbbccddee02") {
		t.Errorf("suggestion does not carry the peer's neighbour report: %s", calls[0])
	}
	if strings.Contains(calls[0], "bb:bb:cc:dd:ee:02") {
		t.Errorf("suggestion passes a BSSID where hostapd wants a report: %s", calls[0])
	}
	if !strings.Contains(calls[0], `"disassociation_imminent":false`) {
		t.Errorf("suggestion uses the wrong ubus key: %s", calls[0])
	}
	if strings.Contains(calls[0], "8c:de:52:11:22:33") {
		t.Errorf("suggestion names the local BSSID: %s", calls[0])
	}

	// The same client is not asked again inside the cooldown window.
	a.maybeSteer(context.Background(), weakHello())
	if calls := runner.btmCalls(); len(calls) != 1 {
		t.Errorf("client was nudged twice inside the cooldown: %v", calls)
	}
}

// A station entry mac80211 has not expired yet still points at an AP the client
// left after roaming. Sending a BTM to it is wasted work and the log reads like
// active steering, so a stale sample is ignored even though its signal is weak.
func TestMaybeSteerIgnoresStaleStation(t *testing.T) {
	runner := &steerRunner{}
	a := newSteerAgent(t, runner, true)
	h := weakHello()
	h.Clients[0].InactiveSeconds = steerStaleSeconds

	a.maybeSteer(context.Background(), h)

	if calls := runner.btmCalls(); len(calls) != 0 {
		t.Errorf("stale station was steered: %v", calls)
	}
}

// A strong client must never be nudged: the agent has no measurement of the
// candidate link, so a healthy association is left alone.
func TestMaybeSteerIgnoresStrongClient(t *testing.T) {
	runner := &steerRunner{}
	a := newSteerAgent(t, runner, true)
	h := weakHello()
	h.Clients[0].Signal = -55
	a.maybeSteer(context.Background(), h)
	if calls := runner.btmCalls(); len(calls) != 0 {
		t.Errorf("strong client was nudged: %v", calls)
	}
}

// The ad-hoc path must go through the same outcome tracking as the policy
// loop: one suggestion per cooldown, and silence for a client that declines.
func TestMaybeSteerPacesRepeatedSuggestions(t *testing.T) {
	runner := &steerRunner{}
	a := newSteerAgent(t, runner, true)
	h := weakHello()

	a.maybeSteer(context.Background(), h)
	// Same cycle-safe instant: the suggestion is awaiting its outcome.
	a.maybeSteer(context.Background(), h)

	if calls := runner.btmCalls(); len(calls) != 1 {
		t.Fatalf("11v requests = %d, want 1 (the second must be paced)", len(calls))
	}
}

func TestMaybeSteerWithholdsFromSilencedClient(t *testing.T) {
	runner := &steerRunner{}
	a := newSteerAgent(t, runner, true)

	// A client that has already declined three suggestions.
	a.btm.mu.Lock()
	a.btm.clients["aa:bb:cc:dd:ee:0a"] = &btmRecord{
		attempts:   btmMaxUnanswered,
		silencedTo: time.Now().Add(time.Hour),
		lastSeen:   time.Now(),
	}
	a.btm.mu.Unlock()

	a.maybeSteer(context.Background(), weakHello())

	if calls := runner.btmCalls(); len(calls) != 0 {
		t.Fatalf("11v sent to a silenced client: %v", calls)
	}
	if got := a.btm.counts(); got == 0 {
		t.Fatal("the withheld suggestion was not counted")
	}
}

// withSteerLoop attaches a real policy loop, which is where the shared rate
// window and capability cache live. Both steering paths read them, so the
// ad-hoc path cannot bypass a gate the policy loop applies.
func withSteerLoop(a *Agent) {
	a.steer = NewSteerLoop()
	a.steer.Agent = a
}

// The ad-hoc path runs on every heartbeat, so it is the one that used to nag a
// client which is merely loud-but-weak. A station holding a usable rate is left
// alone even at -85 dBm.
func TestMaybeSteerRateGateSkipsHealthyClient(t *testing.T) {
	runner := &steerRunner{}
	a := newSteerAgent(t, runner, true)
	withSteerLoop(a)
	h := weakHello()
	h.Clients[0].RxRateMbps = 144

	a.maybeSteer(context.Background(), h)
	if calls := runner.btmCalls(); len(calls) != 0 {
		t.Fatalf("steered a client holding 144 Mbps: %v", calls)
	}
}

// The rate gate must not become a blanket exemption: the same station at the
// same signal, but with its rate collapsed, is exactly what should move.
func TestMaybeSteerRateGateStillSteersSlowClient(t *testing.T) {
	runner := &steerRunner{}
	a := newSteerAgent(t, runner, true)
	withSteerLoop(a)
	h := weakHello()
	h.Clients[0].RxRateMbps = 6

	a.maybeSteer(context.Background(), h)
	if calls := runner.btmCalls(); len(calls) != 1 {
		t.Fatalf("11v requests = %d, want 1", len(calls))
	}
}

// A station whose association request declared no 11k/11v capability cannot
// parse the neighbour report elements the request carries. Asking hostapd once
// replaces three wasted frames and half an hour of silence.
func TestMaybeSteerSkipsClientThatCannotReadBTM(t *testing.T) {
	runner := &steerRunner{
		caps: `{"clients":{"aa:bb:cc:dd:ee:0a":{"rrm":[0,0,0,0,0],"mbo":false}}}`,
	}
	a := newSteerAgent(t, runner, true)
	withSteerLoop(a)

	a.maybeSteer(context.Background(), weakHello())
	if calls := runner.btmCalls(); len(calls) != 0 {
		t.Fatalf("sent a BTM to a station without 11k: %v", calls)
	}
	if n := a.btm.unsupportedCount(); n != 1 {
		t.Fatalf("unsupported counter = %d, want 1", n)
	}
}

// Capability lookup failures must not silence the path: unknown is not "no".
func TestMaybeSteerProceedsWhenCapabilityUnknown(t *testing.T) {
	runner := &steerRunner{} // get_clients fails
	a := newSteerAgent(t, runner, true)
	withSteerLoop(a)

	a.maybeSteer(context.Background(), weakHello())
	if calls := runner.btmCalls(); len(calls) != 1 {
		t.Fatalf("11v requests = %d, want 1", len(calls))
	}
}
