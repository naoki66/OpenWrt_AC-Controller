package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

// setStationDump swaps the fake `iw` output and drops the collector's cached
// sample, so the next tick observes the new state instead of the previous one.
func (f *steerFixture) setStationDump(dump string) {
	f.runner.replies["iw dev phy0.1-ap0 station dump"] = dump
	f.agent.Clients.cacheMu.Lock()
	f.agent.Clients.cache = nil
	f.agent.Clients.cacheOK = false
	f.agent.Clients.cacheMu.Unlock()
}

// effectiveRate keeps the best rate of a window, not the latest one: the rate
// is instantaneous and jumps by design, so a single slow sample must not
// condemn a client whose ceiling is still high.
func TestRateWindowKeepsTheBestRecentSample(t *testing.T) {
	w := newRateWindow(steerMapLimit)
	now := time.Unix(1793698800, 0)

	if _, known := w.observe("aa:bb", "wlan1", 0, now); known {
		t.Fatal("an unknown rate must not count as known")
	}
	if best, known := w.observe("aa:bb", "wlan1", 144, now); !known || best != 144 {
		t.Fatalf("got best=%d known=%v, want 144/true", best, known)
	}
	// The link drops to 6 Mbps: still the same window, so the mark stands.
	if best, _ := w.observe("aa:bb", "wlan1", 6, now.Add(10*time.Second)); best != 144 {
		t.Fatalf("a slow sample inside the window lowered the mark to %d", best)
	}
	// Past the window the old ceiling is no longer evidence of anything.
	if best, _ := w.observe("aa:bb", "wlan1", 6, now.Add(rateWindowKeep+time.Second)); best != 6 {
		t.Fatalf("window did not expire: best=%d, want 6", best)
	}
}

// A roam invalidates the ceiling: the old value describes a link that no
// longer exists, and carrying it over would exempt the client from steering on
// its new BSS forever.
func TestRateWindowResetsOnInterfaceChange(t *testing.T) {
	w := newRateWindow(steerMapLimit)
	now := time.Unix(1793698800, 0)
	w.observe("aa:bb", "wlan1", 866, now)

	if best, known := w.observe("aa:bb", "wlan2", 12, now.Add(time.Second)); !known || best != 12 {
		t.Fatalf("got best=%d known=%v after roam, want 12/true", best, known)
	}
}

// A clock that jumps backwards must not freeze a stale mark in place.
func TestRateWindowHandlesBackwardsClock(t *testing.T) {
	w := newRateWindow(steerMapLimit)
	now := time.Unix(1793698800, 0)
	w.observe("aa:bb", "wlan1", 866, now)

	if best, _ := w.observe("aa:bb", "wlan1", 24, now.Add(-time.Minute)); best != 24 {
		t.Fatalf("backwards clock kept the old mark: %d", best)
	}
}

// A weak client that still holds a usable rate is on a slow-looking but
// working link. Sending it a BTM risks dropping a working association for no
// gain, which is the worst outcome this subsystem can produce.
func TestWeakSignalWithHealthyRateIsLeftAlone(t *testing.T) {
	f := newSteerFixture(t)
	f.seedCandidates()
	p := defaultPolicy()
	p.MinRateMbps = 24
	f.loop.SetPolicy(p)
	ctx := context.Background()
	f.setStationDump("Station aa:bb:cc:dd:ee:ff\n" +
		" signal: -85\n" +
		" rx bitrate: 144.4 MBit/s\n" +
		" connected time: 120 seconds\n")

	for i := 0; i < 6; i++ {
		f.loop.tick(ctx)
		f.advance(4 * time.Second)
	}
	if calls := callsJoined(f); strings.Contains(calls, "bss_transition_request") {
		t.Fatalf("healthy-rate client was steered:\n%s", calls)
	}
}

// The case the feature exists for: a decent-looking signal whose rate
// collapsed is being interfered with, and is worth moving.
func TestWeakSignalWithCollapsedRateIsSteered(t *testing.T) {
	f := newSteerFixture(t)
	f.seedCandidates()
	p := defaultPolicy()
	p.MinRateMbps = 24
	f.loop.SetPolicy(p)
	ctx := context.Background()
	f.setStationDump("Station aa:bb:cc:dd:ee:ff\n" +
		" signal: -85\n" +
		" rx bitrate: 6.0 MBit/s\n" +
		" connected time: 120 seconds\n")

	for i := 0; i < 3; i++ {
		f.loop.tick(ctx)
		f.advance(4 * time.Second)
	}
	if calls := callsJoined(f); !strings.Contains(calls, "bss_transition_request") {
		t.Fatalf("slow client was not steered:\n%s", calls)
	}
}

// A driver that reports no rate at all must fall back to the signal test
// rather than treat every client as healthy — "unknown" is not "fast".
func TestMissingRateFallsBackToSignalOnly(t *testing.T) {
	f := newSteerFixture(t)
	f.seedCandidates()
	p := defaultPolicy()
	p.MinRateMbps = 24
	f.loop.SetPolicy(p)
	ctx := context.Background()
	f.setStationDump("Station aa:bb:cc:dd:ee:ff\n" +
		" signal: -85\n" +
		" connected time: 120 seconds\n")

	for i := 0; i < 3; i++ {
		f.loop.tick(ctx)
		f.advance(4 * time.Second)
	}
	if calls := callsJoined(f); !strings.Contains(calls, "bss_transition_request") {
		t.Fatalf("unknown rate suppressed steering:\n%s", calls)
	}
}

// A negative value disables the gate, restoring pure RSSI behaviour for a site
// that distrusts its driver's rate reporting.
func TestRateGateCanBeDisabled(t *testing.T) {
	f := newSteerFixture(t)
	f.seedCandidates()
	p := defaultPolicy()
	p.MinRateMbps = -1
	f.loop.SetPolicy(p)
	ctx := context.Background()
	f.setStationDump("Station aa:bb:cc:dd:ee:ff\n" +
		" signal: -85\n" +
		" rx bitrate: 866.7 MBit/s\n" +
		" connected time: 120 seconds\n")

	for i := 0; i < 3; i++ {
		f.loop.tick(ctx)
		f.advance(4 * time.Second)
	}
	if calls := callsJoined(f); !strings.Contains(calls, "bss_transition_request") {
		t.Fatalf("min_rate_mbps=-1 did not disable the gate:\n%s", calls)
	}
}
