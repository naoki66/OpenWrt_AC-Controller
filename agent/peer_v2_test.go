package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naoki66/ac-controller/shared/proto"
)

func signedPeer(t *testing.T, secret string, st proto.PeerState) []byte {
	t.Helper()
	st.Type = proto.MsgPeerNR
	st.V = proto.PeerVersion
	st.BSS = sanitizePeerBSS(st.BSS)
	peerSeal(&st, secret)
	body, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal signed peer: %v", err)
	}
	return body
}

func peerBSS(bssid, ssid string) proto.NeighborBSS {
	return proto.NeighborBSS{
		Ifname: "phy1.1-ap0", BSSID: bssid, SSID: ssid, Report: testNRReport(bssid),
	}
}

func TestPeerHMACAcceptsSignedAndRejectsTampered(t *testing.T) {
	bus := NewPeerBusWithOptions(PeerOptions{
		Port: 0, Interval: time.Minute, SelfMAC: testSelfMAC, Log: testLog(), Secret: "site-key",
	})
	st := proto.PeerState{
		MAC: testPeerMAC, Boot: "boot-a", TS: time.Now().Unix(), Counter: 1,
		BSS: []proto.NeighborBSS{peerBSS("00:11:22:33:44:01", "SSID-A")},
	}
	bus.ingest(signedPeer(t, "site-key", st))
	if got := bus.Snapshot().BSS; len(got) != 1 {
		t.Fatalf("signed message was not accepted: %+v", got)
	}

	// A changed field with the old signature must be ignored.
	var bad proto.PeerState
	if err := json.Unmarshal(signedPeer(t, "site-key", st), &bad); err != nil {
		t.Fatalf("decode: %v", err)
	}
	bad.BSS[0].SSID = "attacker"
	body, _ := json.Marshal(bad)
	bus.ingest(body)
	if got := bus.Snapshot().BSS; len(got) != 1 || got[0].SSID != "SSID-A" {
		t.Fatalf("tampered message changed the table: %+v", got)
	}

	// No signature at all is rejected while a secret is configured.
	unsigned := st
	unsigned.Boot, unsigned.Counter, unsigned.Sig = "boot-b", 2, ""
	body, _ = json.Marshal(unsigned)
	bus.ingest(body)
	if got := bus.Snapshot().BSS; len(got) != 1 || got[0].SSID != "SSID-A" {
		t.Fatalf("unsigned message changed the table: %+v", got)
	}
}

func TestValidatePeerBSSRejectsBadReports(t *testing.T) {
	valid := peerBSS("00:11:22:33:44:01", "SSID-A")
	cases := []proto.NeighborBSS{
		{Ifname: "x", BSSID: "zz:zz", SSID: "SSID-A", Report: testNRReport("00:11:22:33:44:01")},
		{Ifname: "x", BSSID: "00:11:22:33:44:01", SSID: strings.Repeat("x", 33), Report: testNRReport("00:11:22:33:44:01")},
		{Ifname: "x", BSSID: "00:11:22:33:44:01", SSID: "SSID-A", Report: "abcd"},
		{Ifname: "x", BSSID: "00:11:22:33:44:01", SSID: "SSID-A", Report: testNRReport("00:11:22:33:44:99")},
		{Ifname: "x", BSSID: "00:11:22:33:44:01", SSID: "SSID-A", Report: testNRReport("00:11:22:33:44:01")[:25]},
	}
	for i, c := range cases {
		if got := validatePeerBSS([]proto.NeighborBSS{c}); len(got) != 0 {
			t.Errorf("case %d accepted %+v", i, got)
		}
	}
	if got := validatePeerBSS([]proto.NeighborBSS{valid}); len(got) != 1 {
		t.Fatalf("valid report rejected: %+v", got)
	}

	// Duplicate BSSIDs collapse; the list is capped at PeerMaxReports.
	dup := validatePeerBSS([]proto.NeighborBSS{valid, valid})
	if len(dup) != 1 {
		t.Errorf("duplicate BSSID kept: %+v", dup)
	}
	var many []proto.NeighborBSS
	for i := 0; i < proto.PeerMaxReports+4; i++ {
		bssid := "00:11:22:33:" + strings.ToLower(strings.TrimSpace(
			time.Unix(int64(i), 0).Format("04:05")))
		many = append(many, peerBSS(bssid, "SSID-A"))
	}
	if got := validatePeerBSS(many); len(got) > proto.PeerMaxReports {
		t.Errorf("report cap exceeded: %d", len(got))
	}
}

func TestPeerSplitReportsReassemble(t *testing.T) {
	bus := NewPeerBusWithOptions(PeerOptions{
		Port: 0, Interval: time.Minute, SelfMAC: testSelfMAC, Log: testLog(),
	})
	items := []proto.NeighborBSS{
		peerBSS("00:11:22:33:44:01", "SSID-A"),
		peerBSS("00:11:22:33:44:02", "SSID-A"),
		peerBSS("00:11:22:33:44:03", "SSID-A"),
	}
	for i, item := range items {
		st := proto.PeerState{
			Type: proto.MsgPeerNR, V: proto.PeerVersion, MAC: testPeerMAC,
			Gen: 7, Part: i + 1, Parts: len(items),
			BSS: []proto.NeighborBSS{item},
		}
		body, err := json.Marshal(st)
		if err != nil {
			t.Fatalf("marshal part: %v", err)
		}
		bus.ingest(body)
	}
	if got := bus.Snapshot().BSS; len(got) != len(items) {
		t.Fatalf("split reports did not reassemble: %+v", got)
	}

	// A later unsplit generation replaces the assembled set.
	st := proto.PeerState{
		Type: proto.MsgPeerNR, V: proto.PeerVersion, MAC: testPeerMAC,
		Gen: 8, Part: 1, Parts: 1,
		BSS: []proto.NeighborBSS{peerBSS("00:11:22:33:44:09", "SSID-A")},
	}
	body, _ := json.Marshal(st)
	bus.ingest(body)
	if got := bus.Snapshot().BSS; len(got) != 1 || got[0].BSSID != "00:11:22:33:44:09" {
		t.Fatalf("unsplit generation did not replace the set: %+v", got)
	}
}

func TestPeerNRChunksStayWithinFrameBudget(t *testing.T) {
	bus := NewPeerBusWithOptions(PeerOptions{
		Port: 0, Interval: time.Minute, SelfMAC: testSelfMAC, Log: testLog(),
	})
	var reports []proto.NeighborBSS
	for i := 0; i < 40; i++ {
		bssid := fmt.Sprintf("00:11:22:33:00:%02x", i)
		reports = append(reports, peerBSS(bssid, "SSID-A"))
	}
	base := proto.PeerState{Type: proto.MsgPeerNR, V: proto.PeerVersion, MAC: testPeerMAC}
	chunks := bus.nrChunks(base, 1, reports)
	if len(chunks) < 2 {
		t.Fatalf("40 reports fit in %d frame(s); test no longer exercises splitting", len(chunks))
	}
	total := 0
	for i, chunk := range chunks {
		total += len(chunk)
		if bus.nrFrameTooBig(base, 1, chunk) {
			t.Errorf("chunk %d exceeds the frame budget: %d reports", i, len(chunk))
		}
	}
	if total != len(reports) {
		t.Errorf("chunks carry %d reports, want %d", total, len(reports))
	}
}

func TestPeerSnapshotCarriesNTPServers(t *testing.T) {
	bus := NewPeerBusWithOptions(PeerOptions{
		Port: 0, Interval: time.Minute, SelfMAC: testSelfMAC, Log: testLog(),
	})
	st := proto.PeerState{
		Type: proto.MsgPeerState, V: proto.PeerVersion, MAC: testPeerMAC,
		NTP: []string{"10.0.10.1", "ntp.aliyun.com"},
	}
	body, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	bus.ingest(body)
	if got := bus.Snapshot().NTP; !slices.Equal(got, st.NTP) {
		t.Fatalf("snapshot NTP = %v, want %v", got, st.NTP)
	}
}

func TestPeerNTPIsLearnedForAnOfflineAP(t *testing.T) {
	bus := NewPeerBusWithOptions(PeerOptions{
		Port: 0, Interval: time.Minute, SelfMAC: testSelfMAC, Log: testLog(),
	})
	st := proto.PeerState{
		Type: proto.MsgPeerState, V: proto.PeerVersion, MAC: testPeerMAC,
		NTP: []string{"10.0.10.1"},
	}
	body, _ := json.Marshal(st)
	bus.ingest(body)

	a := &Agent{
		Config:  Config{Root: t.TempDir(), PeerSteer: false},
		Log:     testLog(),
		peerBus: bus,
	}
	a.coordinatePeers(context.Background(), proto.Hello{MAC: testSelfMAC})
	if !slices.Equal(a.peerNTP, st.NTP) {
		t.Fatalf("peer NTP = %v, want %v", a.peerNTP, st.NTP)
	}
}

func TestPeerReplayWindow(t *testing.T) {
	bus := NewPeerBusWithOptions(PeerOptions{
		Port: 0, Interval: time.Minute, SelfMAC: testSelfMAC, Log: testLog(),
	})
	now := time.Unix(1_800_000_000, 0)
	bus.now = func() time.Time { return now }

	mk := func(boot string, counter uint64, ts int64, bssid string) []byte {
		st := proto.PeerState{
			Type: proto.MsgPeerNR, V: proto.PeerVersion, MAC: testPeerMAC,
			Boot: boot, Counter: counter, TS: ts,
			BSS: []proto.NeighborBSS{peerBSS(bssid, "SSID-A")},
		}
		body, err := json.Marshal(st)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return body
	}
	first := func() string {
		snap := bus.Snapshot().BSS
		if len(snap) == 0 {
			return ""
		}
		return snap[0].BSSID
	}

	bus.ingest(mk("boot-1", 100, now.Unix(), "00:11:22:33:44:01"))
	if got := first(); got != "00:11:22:33:44:01" {
		t.Fatalf("first message not accepted: %q", got)
	}
	bus.ingest(mk("boot-1", 100, now.Unix(), "00:11:22:33:44:02"))
	if got := first(); got != "00:11:22:33:44:01" {
		t.Errorf("duplicate counter accepted: %q", got)
	}
	bus.ingest(mk("boot-1", 80, now.Unix(), "00:11:22:33:44:03"))
	if got := first(); got != "00:11:22:33:44:03" {
		t.Errorf("in-window out-of-order message rejected: %q", got)
	}
	bus.ingest(mk("boot-1", 10, now.Unix(), "00:11:22:33:44:04"))
	if got := first(); got != "00:11:22:33:44:03" {
		t.Errorf("counter behind the window accepted: %q", got)
	}
	bus.ingest(mk("boot-1", 101, now.Add(-61*time.Minute).Unix(), "00:11:22:33:44:05"))
	if got := first(); got != "00:11:22:33:44:03" {
		t.Errorf("stale timestamp accepted: %q", got)
	}
	bus.ingest(mk("boot-2", 1, now.Unix(), "00:11:22:33:44:06"))
	if got := first(); got != "00:11:22:33:44:06" {
		t.Errorf("new boot id did not reset the window: %q", got)
	}
}

func TestPeerLegacyOnlyAcceptedWithoutSecret(t *testing.T) {
	legacy := proto.PeerState{
		Type: proto.MsgPeer, V: proto.PeerVersionLegacy, MAC: testPeerMAC,
		BSS: sanitizePeerBSS([]proto.NeighborBSS{peerBSS("00:11:22:33:44:01", "SSID-A")}),
	}
	body, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}

	open := NewPeerBusWithOptions(PeerOptions{
		Port: 0, Interval: time.Minute, SelfMAC: testSelfMAC, Log: testLog(),
	})
	open.ingest(body)
	if got := open.Snapshot().BSS; len(got) != 1 {
		t.Errorf("legacy message rejected without a secret: %+v", got)
	}

	locked := NewPeerBusWithOptions(PeerOptions{
		Port: 0, Interval: time.Minute, SelfMAC: testSelfMAC, Log: testLog(), Secret: "site-key",
	})
	locked.ingest(body)
	if got := locked.Snapshot().BSS; len(got) != 0 {
		t.Errorf("unsigned legacy message accepted with a secret: %+v", got)
	}
}

func TestPeerConfigFromEnv(t *testing.T) {
	t.Setenv("AC_PEER_NETWORK", "mgmt")
	t.Setenv("AC_SECRET", "site-key")
	cfg := Load(Config{})
	if cfg.PeerNetwork != "mgmt" || cfg.Secret != "site-key" {
		t.Errorf("peer config = network %q secret %q", cfg.PeerNetwork, cfg.Secret)
	}

	t.Setenv("AC_PEER_NETWORK", "")
	t.Setenv("AC_SECRET", "")
	cfg = Load(Config{})
	if cfg.PeerNetwork != "lan" {
		t.Errorf("default peer network = %q, want lan", cfg.PeerNetwork)
	}
}

func TestPeerStartFallsBackToIPv4WithoutNetwork(t *testing.T) {
	// Pick a free UDP port, release it, then let Start bind it. With no
	// management network the bus must choose the IPv4 broadcast path rather
	// than fail the provisioning loop.
	probe, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	_ = probe.Close()

	bus := NewPeerBusWithOptions(PeerOptions{
		Port: port, Interval: time.Minute, SelfMAC: testSelfMAC, Log: testLog(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bus.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer bus.Stop()
	if bus.transport != "ipv4" {
		t.Errorf("transport = %q, want ipv4 fallback", bus.transport)
	}
}

func TestPeerResolveNetworkDevice(t *testing.T) {
	runner := &fakeRunner{replies: map[string]string{
		"ubus call network.interface.lan status": `{"up":true,"l3_device":"br-lan"}`,
	}}
	bus := NewPeerBusWithOptions(PeerOptions{
		Port: 0, Interval: time.Minute, SelfMAC: testSelfMAC, Log: testLog(),
		Network: "lan", Runner: runner,
	})
	if got := bus.resolveDevice(context.Background()); got != "br-lan" {
		t.Errorf("resolved device = %q, want br-lan", got)
	}

}
