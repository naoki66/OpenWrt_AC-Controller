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
