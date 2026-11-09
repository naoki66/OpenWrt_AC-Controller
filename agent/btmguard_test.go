package agent

import (
	"testing"
	"time"
)

type guardClock struct{ t time.Time }

func (c *guardClock) now() time.Time          { return c.t }
func (c *guardClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestGuard() (*btmGuard, *guardClock) {
	c := &guardClock{t: time.Unix(1793698800, 0)}
	g := newBTMGuard()
	g.now = c.now
	return g, c
}

// The behaviour that was missing: a suggestion now has an outcome. A client
// that stays put is counted, and after btmMaxUnanswered of those it is left
// alone instead of being asked again every cooldown forever.
func TestBTMGuardSilencesAfterRepeatedDeclines(t *testing.T) {
	g, clock := newTestGuard()
	window := 2 * time.Minute
	assocAt := clock.now().Add(-5 * time.Minute)

	for i := 0; i < btmMaxUnanswered; i++ {
		if ok, reason := g.allow("aa:bb:cc:dd:ee:01", "wlan0", assocAt, window); !ok {
			t.Fatalf("attempt %d refused (%s); the client should get its chances", i+1, reason)
		}
		g.sent("aa:bb:cc:dd:ee:01", "wlan0", assocAt)

		// Inside the window the next call is paced, not counted.
		if ok, reason := g.allow("aa:bb:cc:dd:ee:01", "wlan0", assocAt, window); ok || reason != "awaiting outcome" {
			t.Fatalf("attempt %d: re-issued inside the window (ok=%v reason=%s)", i+1, ok, reason)
		}
		clock.advance(window + time.Second)
	}

	// Fourth time: three declines is enough.
	if ok, reason := g.allow("aa:bb:cc:dd:ee:01", "wlan0", assocAt, window); ok || reason != "silenced" {
		t.Fatalf("after %d declines: ok=%v reason=%s, want silenced", btmMaxUnanswered, ok, reason)
	}
	if got := g.counts(); got == 0 {
		t.Fatal("a withheld suggestion must be counted")
	}
}

func TestBTMGuardGivesTheClientAnotherChanceAfterSilence(t *testing.T) {
	g, clock := newTestGuard()
	window := 2 * time.Minute
	assocAt := clock.now().Add(-5 * time.Minute)

	for i := 0; i < btmMaxUnanswered; i++ {
		if ok, _ := g.allow("aa", "wlan0", assocAt, window); ok {
			g.sent("aa", "wlan0", assocAt)
		}
		clock.advance(window + time.Second)
	}
	if ok, _ := g.allow("aa", "wlan0", assocAt, window); ok {
		t.Fatal("expected silence")
	}

	clock.advance(btmSilencePeriod + time.Second)
	if ok, reason := g.allow("aa", "wlan0", assocAt, window); !ok {
		t.Fatalf("after the silence period the client must be retried, got %s", reason)
	}
}

// Leaving the interface (a roam) is success: the advice worked, so the count
// starts over even though the client is now on a much weaker BSS elsewhere.
func TestBTMGuardTreatsRoamAsSuccess(t *testing.T) {
	g, clock := newTestGuard()
	window := time.Minute
	assocAt := clock.now().Add(-5 * time.Minute)

	for i := 0; i < btmMaxUnanswered; i++ {
		g.allow("aa", "wlan0", assocAt, window)
		g.sent("aa", "wlan0", assocAt)
		clock.advance(window + time.Second)
	}
	if ok, _ := g.allow("aa", "wlan0", assocAt, window); ok {
		t.Fatal("expected silence before the roam")
	}

	// Same client, different BSS on this AP.
	if ok, reason := g.allow("aa", "wlan1", assocAt, window); !ok {
		t.Fatalf("a client that roamed must be eligible again, got %s", reason)
	}
}

// A client that dropped and rejoined shows a later association time. That is a
// move too, and it must also end a silence.
func TestBTMGuardTreatsReassociationAsSuccess(t *testing.T) {
	g, clock := newTestGuard()
	window := time.Minute
	assocAt := clock.now().Add(-5 * time.Minute)

	for i := 0; i < btmMaxUnanswered; i++ {
		g.allow("aa", "wlan0", assocAt, window)
		g.sent("aa", "wlan0", assocAt)
		clock.advance(window + time.Second)
	}
	if ok, _ := g.allow("aa", "wlan0", assocAt, window); ok {
		t.Fatal("expected silence")
	}

	// Rejoined 10 seconds ago on the same BSS.
	fresh := clock.now().Add(-10 * time.Second)
	if ok, reason := g.allow("aa", "wlan0", fresh, window); !ok {
		t.Fatalf("a reassociated client must be eligible again, got %s", reason)
	}
}

// The one-second resolution of `iw`'s connected time must not look like a
// reassociation, or every sample would reset the count and nothing would ever
// be silenced.
func TestBTMGuardIgnoresConnectedTimeJitter(t *testing.T) {
	g, clock := newTestGuard()
	window := time.Minute
	assocAt := clock.now().Add(-5 * time.Minute)

	g.allow("aa", "wlan0", assocAt, window)
	g.sent("aa", "wlan0", assocAt)
	clock.advance(window + time.Second)

	// One second of drift between two readings of the same association.
	jittered := assocAt.Add(time.Second)
	if ok, reason := g.allow("aa", "wlan0", jittered, window); !ok {
		t.Fatalf("jitter reset the count (reason=%s); it must be treated as a decline", reason)
	}
}

func TestBTMGuardRecordsSentStateAndClears(t *testing.T) {
	g, clock := newTestGuard()
	window := time.Minute
	assocAt := clock.now().Add(-5 * time.Minute)

	g.sent("aa", "wlan0", assocAt)
	if ok, reason := g.allow("aa", "wlan0", assocAt, window); ok || reason != "awaiting outcome" {
		t.Fatalf("a pending suggestion must hold the client back, got ok=%v reason=%s", ok, reason)
	}

	g.clear("aa")
	if ok, _ := g.allow("aa", "wlan0", assocAt, window); !ok {
		t.Fatal("clear must forget the client entirely")
	}
}

// A real rejection is stronger evidence than the association-based inference:
// after three of them the client is parked without waiting for a window, and
// the target it refused is not offered again.
func TestBTMGuardReportRejectionSilencesAndBacksOffTarget(t *testing.T) {
	g, clock := newTestGuard()
	mac := "aa:bb:cc:dd:ee:01"
	target := "11:22:33:44:55:66"

	for i := 0; i < btmMaxUnanswered; i++ {
		g.report(mac, 1, target)
		if !g.suppressedTarget(mac, target) {
			t.Fatalf("rejection %d did not back off the target", i+1)
		}
	}
	if ok, reason := g.allow(mac, "wlan0", time.Time{}, time.Minute); ok || reason != "silenced" {
		t.Fatalf("after real rejections: ok=%v reason=%s, want silenced", ok, reason)
	}

	stats := g.stats()
	if stats.Rejected != btmMaxUnanswered || stats.Accepted != 0 || stats.Suppressed != 1 {
		t.Fatalf("stats = %+v, want rejected %d, accepted 0, suppressed 1", stats, btmMaxUnanswered)
	}

	other := "77:88:99:aa:bb:cc"
	cands := [][]string{{target, "ssid", "rpt"}, {other, "ssid2", "rpt2"}}
	filtered := g.filterTargets(mac, cands)
	if len(filtered) != 1 || filtered[0][0] != other {
		t.Fatalf("filterTargets = %v, want only the other BSS", filtered)
	}

	// The backoff is per target and expires; the client itself stays silenced
	// for its own, longer period.
	clock.advance(btmTargetBackoff + time.Second)
	if g.suppressedTarget(mac, target) {
		t.Fatal("target backoff did not expire after 10 minutes")
	}
	if got := g.filterTargets(mac, cands); len(got) != 2 {
		t.Fatalf("after expiry filterTargets = %v, want both candidates", got)
	}
}

// An accepted response settles the pending suggestion and ends a streak.
func TestBTMGuardReportAcceptedClearsAttempts(t *testing.T) {
	g, clock := newTestGuard()
	mac := "aa:bb:cc:dd:ee:02"
	target := "11:22:33:44:55:67"

	for i := 0; i < btmMaxUnanswered; i++ {
		g.report(mac, 1, target)
		clock.advance(time.Second)
	}
	if ok, _ := g.allow(mac, "wlan0", time.Time{}, time.Minute); ok {
		t.Fatal("expected silence before the accepted response")
	}

	g.report(mac, 0, target)
	if ok, reason := g.allow(mac, "wlan0", time.Time{}, time.Minute); !ok {
		t.Fatalf("acceptance must give the client a fresh start, got %s", reason)
	}
	if g.suppressedTarget(mac, target) {
		t.Fatal("acceptance must clear the target backoff")
	}

	stats := g.stats()
	if stats.Accepted != 1 || stats.Rejected != btmMaxUnanswered {
		t.Fatalf("stats = %+v, want accepted 1 rejected %d", stats, btmMaxUnanswered)
	}
}

func TestBTMGuardStatsCountSentFrames(t *testing.T) {
	g, _ := newTestGuard()
	g.sent("aa", "wlan0", time.Time{})
	g.report("aa", 0, "")

	stats := g.stats()
	if stats.Sent != 1 || stats.Accepted != 1 {
		t.Fatalf("stats = %+v, want sent 1 accepted 1", stats)
	}
}

// A nil guard is the "feature not wired" case; it must never block steering.
func TestNilBTMGuardAllows(t *testing.T) {
	var g *btmGuard
	if ok, _ := g.allow("aa", "wlan0", time.Time{}, time.Minute); !ok {
		t.Fatal("nil guard must allow")
	}
	g.sent("aa", "wlan0", time.Time{})
	g.clear("aa")
	if got := g.counts(); got != 0 {
		t.Fatalf("counts = %d, want 0", got)
	}
}
