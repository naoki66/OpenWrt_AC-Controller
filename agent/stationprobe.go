package agent

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Station-presence probe, the cheap half of the client sample.
//
// The steering loop samples every managed BSS every few seconds with
// `iw dev <bss> station dump`, and each call is a fork+exec. Measured on an
// XR1710G (AN7581, 11 managed BSSes, 14 stations): 6.0 ms per call, six of
// which are process start-up. Eight of those eleven BSSes had no station at
// all, so 73% of the work produced an empty string.
//
// mac80211 already publishes the association table as a directory tree:
//
//	/sys/kernel/debug/ieee80211/phy0/netdev:phy0.1-ap2/stations/<mac>/
//
// Reading it costs roughly two orders of magnitude less than spawning iw and
// answers the only question needed to skip the call: is it empty?
//
// The probe deliberately distinguishes three answers, not two. "Zero stations",
// "has stations" and "I do not know this BSS" are different: collapsing the
// third into the first would silence telemetry for every driver that is not
// mac80211 (or any device where debugfs is not mounted), and the failure would
// look like "no clients" rather than "no data". Only an explicit zero skips the
// iw call; everything else falls back to it, which is exactly the behaviour
// before this file existed.
//
// The debugfs station directories carry airtime and AMSDU counters but not
// signal, rate or connected time (verified on kernel 6.18.55), so this is a
// gate, not a replacement for iw. Removing the exec entirely needs nl80211
// netlink.

const (
	// debugfsStationRoot is where mac80211 exposes its netdev table.
	debugfsStationRoot = "/sys/kernel/debug/ieee80211"
	// netdevPrefix is the directory-name prefix in front of the interface name.
	netdevPrefix = "netdev:"
	// stationProbeTTL bounds how stale a presence answer may be. The steering
	// loop runs at 3–10 s and the heartbeat at 20 s, so one second keeps both
	// consumers on a fresh answer while collapsing their two probes into one.
	stationProbeTTL = time.Second
)

// stationProbe caches per-BSS station counts from debugfs.
//
// It is safe for concurrent use: the steering loop and the heartbeat cycle run
// in different goroutines and both sample clients.
type stationProbe struct {
	// root is the debugfs ieee80211 directory; empty means the default.
	root string
	// prefix is the netdev directory prefix mac80211 uses. It is a field only
	// because Windows forbids ':' in a directory name, so the test tree cannot
	// be built with the real one.
	prefix string
	// now is overridable for tests.
	now func() time.Time

	mu     sync.Mutex
	at     time.Time
	counts map[string]int
}

func newStationProbe(root string) *stationProbe {
	if root == "" {
		root = debugfsStationRoot
	}
	return &stationProbe{root: root, prefix: netdevPrefix, now: time.Now}
}

// has reports whether ifname currently has at least one associated station.
//
// known is false whenever the answer cannot be trusted: debugfs is missing, the
// BSS is not exposed there, or the read failed. A caller must then run iw
// instead of skipping it.
func (p *stationProbe) has(ifname string) (present, known bool) {
	if p == nil || ifname == "" {
		return false, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()
	if p.counts == nil || now.Sub(p.at) >= stationProbeTTL {
		p.refresh(now)
	}
	n, ok := p.counts[ifname]
	if !ok {
		return false, false
	}
	return n > 0, true
}

// refresh rebuilds the ifname→count table. A read failure yields an empty
// table rather than an error: every BSS then reports "unknown", the collectors
// fall back to iw, and the next probe after the TTL retries. That self-heals if
// debugfs is mounted late, which is the case during early boot.
func (p *stationProbe) refresh(now time.Time) {
	p.at = now
	counts := map[string]int{}

	phys, err := os.ReadDir(p.root)
	if err != nil {
		p.counts = counts
		return
	}
	for _, phy := range phys {
		if !phy.IsDir() {
			continue
		}
		phyPath := filepath.Join(p.root, phy.Name())
		entries, err := os.ReadDir(phyPath)
		if err != nil {
			continue
		}
		for _, e := range entries {
			ifname, ok := strings.CutPrefix(e.Name(), p.prefix)
			if !ok || ifname == "" || !e.IsDir() {
				continue
			}
			stations, err := os.ReadDir(filepath.Join(phyPath, e.Name(), "stations"))
			if err != nil {
				continue
			}
			counts[ifname] = len(stations)
		}
	}
	p.counts = counts
}
