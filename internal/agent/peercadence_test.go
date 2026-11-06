package agent

import (
	"testing"
	"time"
)

func TestPeerCadenceTightensWhenControlIsLost(t *testing.T) {
	bus := NewPeerBus(0, 30*time.Second, "00:00:00:00:00:21", testLog())
	a := &Agent{
		peerBus: bus,
		Config:  Config{PeerInterval: 30 * time.Second, PeerIntervalOffline: 20 * time.Second},
		Log:     testLog(),
	}

	a.reconcilePeerCadence()
	if bus.Interval != 30*time.Second {
		t.Fatalf("healthy interval = %s, want 30s", bus.Interval)
	}

	// One or two missed heartbeats are noise; the cadence must not chase them.
	a.helloFailures = rediscoverAfterFailures - 1
	a.reconcilePeerCadence()
	if bus.Interval != 30*time.Second {
		t.Fatalf("interval moved on a transient failure: %s", bus.Interval)
	}

	a.helloFailures = rediscoverAfterFailures
	a.reconcilePeerCadence()
	if bus.Interval != 20*time.Second {
		t.Fatalf("offline interval = %s, want 20s", bus.Interval)
	}

	// Recovery puts it back.
	a.helloFailures = 0
	a.reconcilePeerCadence()
	if bus.Interval != 30*time.Second {
		t.Fatalf("interval after recovery = %s, want 30s", bus.Interval)
	}
}

func TestPeerCadenceIgnoresUnsetConfig(t *testing.T) {
	bus := NewPeerBus(0, 30*time.Second, "00:00:00:00:00:21", testLog())
	a := &Agent{peerBus: bus, Log: testLog()}
	a.helloFailures = 10
	a.reconcilePeerCadence()
	// A zero configured interval must not blank the cadence the bus was built
	// with.
	if bus.Interval != 30*time.Second {
		t.Fatalf("interval = %s, want the bus default", bus.Interval)
	}
}

func TestPeerSetIntervalIsIdempotent(t *testing.T) {
	bus := NewPeerBus(0, 30*time.Second, "00:00:00:00:00:21", testLog())
	bus.SetInterval(20 * time.Second)
	bus.SetInterval(20 * time.Second)
	if bus.Interval != 20*time.Second {
		t.Fatalf("interval = %s, want 20s", bus.Interval)
	}
	// Guard values must not disable announcements entirely.
	bus.SetInterval(0)
	bus.SetInterval(-time.Second)
	if bus.Interval != 20*time.Second {
		t.Fatalf("interval = %s, a non-positive value must be ignored", bus.Interval)
	}
}

// Expiry is derived from the interval, so it must be read consistently with it.
// A 20 s cadence plus the 90 s floor keeps a peer alive through four missed
// announcements, which is deliberate: one lost broadcast must not drop a peer.
func TestPeerExpiryFloorHoldsForOfflineCadence(t *testing.T) {
	bus := NewPeerBus(0, 20*time.Second, "00:00:00:00:00:21", testLog())
	bus.mu.Lock()
	got := bus.expiry()
	bus.mu.Unlock()
	if got != peerMinExpiry {
		t.Fatalf("expiry = %s, want the %s floor", got, peerMinExpiry)
	}
}

// The periodic 11k restore exists for "hostapd forgot its lists after a reload",
// which cannot happen while no revision can arrive. An offline AP therefore
// halves the cadence; peer set changes still reassert within one heartbeat.
func TestNeighborRefreshSlowsDownWhileControlIsLost(t *testing.T) {
	a := &Agent{Log: testLog()}
	if got := a.neighborRefreshPeriod(); got != neighborRefreshCycles {
		t.Fatalf("healthy period = %d, want %d", got, neighborRefreshCycles)
	}
	a.helloFailures = rediscoverAfterFailures
	if got := a.neighborRefreshPeriod(); got != neighborRefreshCyclesOffline {
		t.Fatalf("offline period = %d, want %d", got, neighborRefreshCyclesOffline)
	}
	a.helloFailures = 0
	if got := a.neighborRefreshPeriod(); got != neighborRefreshCycles {
		t.Fatalf("period after recovery = %d, want %d", got, neighborRefreshCycles)
	}
}
