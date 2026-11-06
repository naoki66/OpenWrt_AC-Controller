package agent

import (
	"sync"
	"time"
)

// Effective-rate window for sticky-client detection.
//
// RSSI alone is a poor proxy for whether a client is suffering. mac80211's
// rate adaptation already reacts to a degrading link: a station sitting at
// -81 dBm that still negotiates 144 Mbps is on a quiet 2.4 GHz channel and
// telling it to roam only risks a drop. The opposite case matters more — a
// station at -65 dBm whose rate collapsed to 6 Mbps is being interfered with
// and is exactly the client worth moving.
//
// So the weak-signal test consults the receive rate as a second opinion. The
// rate itself is instantaneous and jumps around by design, which is why one
// sample must never decide the question. The window keeps the **best** rate
// seen in the last rateWindowKeep and compares against that: a client that
// reached a usable rate at any point in the window still has a working link,
// while a client whose ceiling has collapsed cannot fake it. Erring toward
// "leave it alone" is deliberate — a missed roam costs throughput, a wrong
// roam costs the association.

// rateWindowKeep is how far back the best-rate high-water mark is trusted.
// Thirty seconds spans several rate-adaptation cycles without holding on to
// a ceiling the link lost minutes ago.
const rateWindowKeep = 30 * time.Second

// rateSample is one client's high-water mark within a window.
type rateSample struct {
	ifname string
	best   int
	since  time.Time
}

// rateWindow tracks the best receive rate per client for a short window.
type rateWindow struct {
	mu    sync.Mutex
	items map[string]*rateSample
	limit int
}

func newRateWindow(limit int) *rateWindow {
	if limit <= 0 {
		limit = steerMapLimit
	}
	return &rateWindow{items: map[string]*rateSample{}, limit: limit}
}

// observe folds one sample in and returns the window's best rate.
//
// known is false while no non-zero rate has been seen in the window, which is
// the common case on a driver that does not report one. Callers must fall back
// to the signal-only test then: "unknown" is not "slow".
func (w *rateWindow) observe(mac, ifname string, rate int, now time.Time) (best int, known bool) {
	if mac == "" {
		return 0, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.items[mac]
	// A different interface means the client roamed: the old ceiling belongs
	// to a link that no longer exists.
	if s == nil || s.ifname != ifname || now.Sub(s.since) > rateWindowKeep || now.Before(s.since) {
		s = &rateSample{ifname: ifname, since: now}
		w.items[mac] = s
		if len(w.items) > w.limit {
			w.trim(now)
		}
	}
	if rate > s.best {
		s.best = rate
	}
	return s.best, s.best > 0
}

// trim drops expired entries once the map has grown past its bound, and clears
// it outright in the pathological case where nothing has expired.
func (w *rateWindow) trim(now time.Time) {
	for mac, s := range w.items {
		if now.Sub(s.since) > rateWindowKeep {
			delete(w.items, mac)
		}
	}
	if len(w.items) > w.limit {
		w.items = map[string]*rateSample{}
	}
}

// forget removes a client, used when its binding or association changes.
func (w *rateWindow) forget(mac string) {
	if mac == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.items, mac)
}
