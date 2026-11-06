package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/naoki66/ac-controller/internal/proto"
)

func peerPayload(t *testing.T, mac string, bss []proto.NeighborBSS, clients []proto.PeerClient) []byte {
	t.Helper()
	st := proto.PeerState{
		Type:     proto.MsgPeerNR,
		V:        proto.PeerVersion,
		MAC:      mac,
		Hostname: "peer",
		BSS:      sanitizePeerBSS(bss),
		Clients:  clients,
	}
	body, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal peer state: %v", err)
	}
	return body
}

// A peer's client list churns every announcement and changes nothing this AP
// writes; its BSS set is what the neighbour lists are built from.
func TestPeerVersionTracksBSSSetOnly(t *testing.T) {
	bus := NewPeerBus(0, time.Second, "00:00:00:00:00:21", testLog())
	bssA := []proto.NeighborBSS{{Ifname: "phy0.0-ap0", SSID: "SSID-A", BSSID: "aa:bb:cc:dd:ee:01", Report: "r1"}}

	bus.ingest(peerPayload(t, "00:00:00:00:00:99", bssA, nil))
	bus.Snapshot()
	baseline := bus.Version()

	// Same BSS set, different clients.
	bus.ingest(peerPayload(t, "00:00:00:00:00:99", bssA,
		[]proto.PeerClient{{MAC: "aa:aa:aa:aa:aa:01", SSID: "SSID-A", Signal: -60}}))
	bus.Snapshot()
	if got := bus.Version(); got != baseline {
		t.Fatalf("client churn moved the version: %d -> %d", baseline, got)
	}

	// A new BSS on the same peer.
	bssB := append([]proto.NeighborBSS{}, bssA...)
	bssB = append(bssB, proto.NeighborBSS{Ifname: "phy0.1-ap0", SSID: "SSID-A_5G", BSSID: "aa:bb:cc:dd:ee:02", Report: "r2"})
	bus.ingest(peerPayload(t, "00:00:00:00:00:99", bssB, nil))
	bus.Snapshot()
	if got := bus.Version(); got == baseline {
		t.Fatal("an appearing BSS must move the version")
	}
}

// Announcement order must not masquerade as a change.
func TestPeerVersionIgnoresBSSOrdering(t *testing.T) {
	bus := NewPeerBus(0, time.Second, "00:00:00:00:00:21", testLog())
	a := proto.NeighborBSS{SSID: "SSID-A", BSSID: "aa:bb:cc:dd:ee:01", Report: "r1"}
	b := proto.NeighborBSS{SSID: "SSID-A_5G", BSSID: "aa:bb:cc:dd:ee:02", Report: "r2"}

	bus.ingest(peerPayload(t, "00:00:00:00:00:99", []proto.NeighborBSS{a, b}, nil))
	bus.Snapshot()
	baseline := bus.Version()

	bus.ingest(peerPayload(t, "00:00:00:00:00:99", []proto.NeighborBSS{b, a}, nil))
	bus.Snapshot()
	if got := bus.Version(); got != baseline {
		t.Fatalf("reordering moved the version: %d -> %d", baseline, got)
	}
}

// A peer that stops announcing removes its BSSes from every list, so expiry is
// a change too.
func TestPeerVersionTracksPeerExpiry(t *testing.T) {
	bus := NewPeerBus(0, 20*time.Millisecond, "00:00:00:00:00:21", testLog())
	bus.ingest(peerPayload(t, "00:00:00:00:00:99",
		[]proto.NeighborBSS{{SSID: "SSID-A", BSSID: "aa:bb:cc:dd:ee:01", Report: "r1"}}, nil))
	bus.Snapshot()
	start := bus.Version()
	if len(bus.Snapshot().BSS) != 1 {
		t.Fatal("peer should be live")
	}

	// Age the entry past the expiry rather than waiting out the 90 s floor.
	bus.mu.Lock()
	for mac, e := range bus.peers {
		e.seen = time.Now().Add(-2 * time.Hour)
		bus.peers[mac] = e
	}
	bus.mu.Unlock()

	if got := bus.Snapshot(); len(got.BSS) != 0 {
		t.Fatal("peer should have expired")
	}
	if bus.Version() == start {
		t.Fatal("a peer disappearing must move the version")
	}
}

// End to end: a peer appearing must reach hostapd without waiting for the
// five-minute periodic refresh.
func TestPeerChangeReassertsListsImmediately(t *testing.T) {
	r := newNeighborRunner("phy0.1-ap0")
	ap := &Applier{StateDir: t.TempDir(), Runner: r, Log: testLog(), Sleep: func(time.Duration) {}}
	ap.cacheNeighbors([]byte(`{"phy0.1-ap0":[["aa:bb:cc:dd:ee:01","SSID-A_5G","00"]]}`))

	bus := NewPeerBus(0, time.Second, "00:00:00:00:00:21", testLog())
	a := &Agent{Applier: ap, peerBus: bus, Log: testLog()}
	// This AP's own BSS, so the merge knows which SSID each interface serves.
	a.lastOwnNeighbors = []proto.NeighborBSS{{
		Ifname: "phy0.1-ap0", SSID: "SSID-A_5G", BSSID: "00:00:00:00:00:21", Report: "self",
	}}

	// Opening snapshot: recorded, not acted on.
	bus.ingest(peerPayload(t, "00:00:00:00:00:99", nil, nil))
	bus.Snapshot()
	a.restoreNeighborsOnPeerChange(context.Background())
	if n := len(r.setCalls()); n != 0 {
		t.Fatalf("first observation wrote %d time(s), want 0", n)
	}

	// A peer arrives advertising a second BSS on the same SSID.
	bus.ingest(peerPayload(t, "00:00:00:00:00:99",
		[]proto.NeighborBSS{{SSID: "SSID-A_5G", BSSID: "aa:bb:cc:dd:ee:02", Report: "peer"}}, nil))
	bus.Snapshot()
	a.restoreNeighborsOnPeerChange(context.Background())

	if n := len(r.setCalls()); n == 0 {
		t.Fatal("a new peer BSS must be written to hostapd immediately")
	}
	if got := r.currentList("phy0.1-ap0"); len(got) != 2 {
		t.Fatalf("list = %v, want the controller entry plus the peer entry", got)
	}
}

// Nothing changed means no write: the version only moves on a real change.
func TestPeerChangeIsIdempotent(t *testing.T) {
	r := newNeighborRunner("phy0.1-ap0")
	ap := &Applier{StateDir: t.TempDir(), Runner: r, Log: testLog(), Sleep: func(time.Duration) {}}
	bus := NewPeerBus(0, time.Second, "00:00:00:00:00:21", testLog())
	a := &Agent{Applier: ap, peerBus: bus, Log: testLog()}

	bus.Snapshot()
	a.restoreNeighborsOnPeerChange(context.Background())
	before := len(r.calls)

	bus.Snapshot()
	a.restoreNeighborsOnPeerChange(context.Background())
	if got := len(r.calls); got != before {
		t.Fatalf("an unchanged peer set caused %d extra calls", got-before)
	}
}
