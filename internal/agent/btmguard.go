package agent

import (
	"sync"
	"time"
)

// Suppressing steering suggestions that a client ignores.
//
// Observed on a live two-AP site (2026-11-04): 02:00:00:00:00:9a sat at
// -81 dBm on the 2.4 GHz BSS for hours while the agent suggested a 5 GHz
// neighbour every two minutes. The client never moved — it almost certainly
// does not implement 11v, so every BTM was a wasted management frame, a wasted
// airtime slot and a log line. Nothing in the loop noticed, because "sent" and
// "worked" were the same event.
//
// This guard adds the missing half: a suggestion now has an outcome. The client
// either left the interface, reassociated (its connected time restarted), or
// stayed put. Three consecutive "stayed put" verdicts mean the client will not
// be talked into moving, and it is left alone for half an hour — long enough for
// a real roam or a driver reload to change the situation, short enough that a
// client which was merely asleep is retried later.
//
// Silence is not "ignore the client": the weak-signal path still counts it, so
// a policy with ForceAfterMS can still deauthenticate it. What is suppressed is
// the repeated advice, which the client has already declined three times.

const (
	// btmMaxUnanswered is how many declined suggestions before silence. Three is
	// also the policy default for "consecutive weak samples", so a client is
	// given exactly as many chances to move as it was given samples to notice.
	btmMaxUnanswered = 3
	// btmSilencePeriod is how long a client that declines is left alone.
	btmSilencePeriod = 30 * time.Minute
	// btmAssocSlack absorbs the one-second resolution of `iw`'s connected time:
	// two samples of the same association can differ by up to a second, and
	// without slack that would read as "the client reassociated".
	btmAssocSlack = 2 * time.Second
	// btmGuardLimit bounds the per-client table.
	btmGuardLimit = 256
)

// btmGuard tracks whether suggestions are working.
type btmGuard struct {
	// now is overridable for tests.
	now func() time.Time

	mu      sync.Mutex
	clients map[string]*btmRecord
	// suppressed counts refusals since start, for the status file.
	suppressed int
	// unsupported counts clients parked because their association request
	// declared no 11k/11v capability at all. Kept apart from "declined": one is
	// a capability mismatch, the other a decision by the client.
	unsupported int
}

type btmRecord struct {
	// attempts is the number of consecutive suggestions the client ignored.
	attempts int
	// sentAt is when the last suggestion went out; zero means none is awaiting
	// a verdict.
	sentAt time.Time
	// ifname and assocAt describe the association a suggestion was sent to. A
	// different interface, or a later association time, proves the client moved.
	ifname  string
	assocAt time.Time
	// silencedTo is when a declining client becomes eligible again.
	silencedTo time.Time
	lastSeen   time.Time
}

func newBTMGuard() *btmGuard {
	return &btmGuard{now: time.Now, clients: map[string]*btmRecord{}}
}

// allow reports whether a suggestion may be sent to mac right now. The reason
// is for logs: "awaiting outcome" is normal pacing, "silenced" means the client
// has declined repeatedly.
func (g *btmGuard) allow(mac, ifname string, assocAt time.Time, window time.Duration) (bool, string) {
	if g == nil || mac == "" {
		return true, ""
	}
	now := g.now()

	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.record(mac, now)

	// Any evidence that the client moved settles everything: a different
	// interface means it roamed, a later association time means it dropped and
	// rejoined. It also ends a silence — the client did what was asked — and
	// closes any suggestion still waiting for a verdict, which that move just
	// answered.
	if clientMoved(ifname, assocAt, r) {
		r.attempts = 0
		r.silencedTo = time.Time{}
		r.sentAt = time.Time{}
	}

	// A finished silence period gives the client a fresh set of chances.
	if r.attempts >= btmMaxUnanswered && !now.Before(r.silencedTo) {
		r.attempts = 0
		r.silencedTo = time.Time{}
	}

	// Settle the previous suggestion once its observation window has passed.
	// Still on the same association after the window means it was declined.
	if !r.sentAt.IsZero() {
		if now.Sub(r.sentAt) < window {
			r.lastSeen = now
			return false, "awaiting outcome"
		}
		r.attempts++
		r.sentAt = time.Time{}
	}
	r.lastSeen = now

	if now.Before(r.silencedTo) {
		g.suppressed++
		return false, "silenced"
	}
	if r.attempts >= btmMaxUnanswered {
		// Declined once too often: stop for a while.
		r.silencedTo = now.Add(btmSilencePeriod)
		g.suppressed++
		return false, "silenced"
	}
	return true, ""
}

// sent records that a suggestion went out, so the next allow can judge it.
func (g *btmGuard) sent(mac, ifname string, assocAt time.Time) {
	if g == nil || mac == "" {
		return
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.record(mac, now)
	r.sentAt = now
	r.ifname = ifname
	r.assocAt = assocAt
	r.lastSeen = now
}

// silence parks a client whose suggestions cannot work, regardless of how many
// have been sent. It is how a capability verdict reaches the pacing layer
// without pretending three suggestions were declined: a station that cannot
// parse a neighbour report element was never a candidate for advice.
//
// The silence still expires and still ends early if the client moves, so a
// station that re-associates with new capabilities gets a fresh verdict.
func (g *btmGuard) silence(mac string) {
	if g == nil || mac == "" {
		return
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.record(mac, now)
	r.attempts = btmMaxUnanswered
	r.silencedTo = now.Add(btmSilencePeriod)
	r.sentAt = time.Time{}
	r.lastSeen = now
	g.suppressed++
	g.unsupported++
}

// clear forgets a client whose association is healthy, so a future streak starts
// from zero instead of inheriting an old verdict.
func (g *btmGuard) clear(mac string) {
	if g == nil || mac == "" {
		return
	}
	g.mu.Lock()
	delete(g.clients, mac)
	g.mu.Unlock()
}

// counts returns the number of suggestions suppressed since start.
func (g *btmGuard) counts() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.suppressed
}

// unsupportedCount returns how many clients were parked for lacking 11k/11v
// capability rather than for declining suggestions.
func (g *btmGuard) unsupportedCount() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.unsupported
}

// record returns the entry for mac, creating it and pruning stale entries when
// the table is full.
func (g *btmGuard) record(mac string, now time.Time) *btmRecord {
	if r, ok := g.clients[mac]; ok {
		return r
	}
	if len(g.clients) >= btmGuardLimit {
		for k, v := range g.clients {
			if v.sentAt.IsZero() && now.Sub(v.lastSeen) > btmSilencePeriod {
				delete(g.clients, k)
			}
		}
	}
	r := &btmRecord{lastSeen: now}
	g.clients[mac] = r
	return r
}

// clientMoved reports whether the client left since the suggestion: a different
// interface means it roamed, a later association time means it dropped and
// rejoined (a roam the AP sees as a fresh association).
func clientMoved(ifname string, assocAt time.Time, r *btmRecord) bool {
	if r.ifname != "" && ifname != "" && ifname != r.ifname {
		return true
	}
	if r.assocAt.IsZero() || assocAt.IsZero() {
		return false
	}
	return assocAt.After(r.assocAt.Add(btmAssocSlack))
}

// assocTime converts a station's "connected N seconds" into the wall-clock
// moment it associated. Comparing these is how a reassociation is detected
// without storing per-client timers.
func assocTime(now time.Time, connectedSeconds int) time.Time {
	if connectedSeconds <= 0 {
		return time.Time{}
	}
	return now.Add(-time.Duration(connectedSeconds) * time.Second)
}
