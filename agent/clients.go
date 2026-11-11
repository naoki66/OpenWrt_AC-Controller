package agent

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/naoki66/ac-controller/shared/proto"
)

// ClientCollector reads the associated-station list from the AP.
//
// The controller is never on the data path (design C1), so the AP is the only
// place client signal information exists. Everything here is read-only: `ubus`
// for the section→interface mapping, `iw` for the per-station counters, `ip
// neigh` for a best-effort address.
type ClientCollector struct {
	Runner Runner
	Log    *slog.Logger
	// Probe overrides the debugfs station probe; nil uses the default root.
	// Only an explicit "this BSS has zero stations" skips the iw call.
	Probe *stationProbe
	// Source selects how the station table is read: "auto" (netlink, falling
	// back to iw), "nl80211", or "iw". Empty means auto.
	Source string
	// Netlink enables the direct-kernel paths for the BSS list and the
	// neighbour table. False keeps the old ubus/ip commands; see AC_NETLINK.
	Netlink bool
	// warnedMissing is set once so a missing source does not log every cycle.
	warnedMissing bool

	probeOnce sync.Once
	probe     *stationProbe

	// readerMu guards replacement when the kernel lacks the station source.
	readerMu sync.Mutex
	reader   StationReader

	// bssReader and neighReader follow the same pattern one level up: they are
	// resolved once and permanently replaced by their command-line equivalent
	// if netlink turns out not to work on this kernel.
	bssMu     sync.Mutex
	bssReader BSSReader
	neighMu   sync.Mutex
	neighRead NeighbourReader

	// cache holds the last successful sample so the steering loop and the
	// heartbeat share one collection instead of each running its own. Guarded
	// by cacheMu; the slice is read-only for callers.
	cacheMu    sync.Mutex
	cacheAt    time.Time
	cache      []proto.ClientStat
	cacheOK    bool
	sampleOnce sync.Once
	sampleGate chan struct{}
}

// stationProbe returns the presence probe, building the default one on first
// use. The collector is shared by the steering loop and the heartbeat cycle, so
// the probe is cached rather than rebuilt per call.
func (c *ClientCollector) stationProbe() *stationProbe {
	c.probeOnce.Do(func() {
		if c.Probe != nil {
			c.probe = c.Probe
			return
		}
		c.probe = newStationProbe("")
	})
	return c.probe
}

// skipsEmptyBSS reports whether the BSS is known to have no associated station,
// in which case `iw station dump` would return an empty string for the price of
// a fork+exec. An unknown answer never skips: a wrong "empty" would turn a
// driver quirk into permanently empty telemetry.
func (c *ClientCollector) skipsEmptyBSS(ifname string) bool {
	present, known := c.stationProbe().has(ifname)
	return known && !present
}

var (
	stationRe   = regexp.MustCompile(`^Station\s+([0-9a-fA-F:]{17})`)
	signalRe    = regexp.MustCompile(`^\s*signal:\s*(-?\d+)`)
	rxBitrateRe = regexp.MustCompile(`^\s*rx bitrate:\s*([0-9.]+)\s*MBit/s`)
	txBitrateRe = regexp.MustCompile(`^\s*tx bitrate:\s*([0-9.]+)\s*MBit/s`)
	connectedRe = regexp.MustCompile(`^\s*connected time:\s*(\d+)\s*seconds`)
	inactiveRe  = regexp.MustCompile(`^\s*inactive time:\s*(\d+)\s*ms`)
)

// Collect returns an authoritative list, or nil if any part of the sweep failed.
func (c *ClientCollector) Collect(ctx context.Context) []proto.ClientStat {
	return c.Snapshot(ctx, 0)
}

// Snapshot returns the station list, reusing a sample younger than maxAge.
//
// Why a shared snapshot: the steering loop samples every few seconds and the
// heartbeat every twenty, and both used to run the same `iw station dump` over
// every BSS — the same data collected twice, once per consumer. On a busy AP
// that is the single most expensive periodic operation the agent performs.
//
// maxAge is a freshness budget, not a cache lifetime: a caller that must not
// steer on stale signal (the loop) passes a value below its own period, while
// the heartbeat — whose telemetry is already sampled once per cycle by the
// controller — can tolerate more. Zero always samples.
//
// A failed sample returns nil and is not cached: nil means "not reported" to
// the controller, which leaves its client table alone, and caching it would
// turn a transient ubus failure into an empty client list for a whole cycle.
func (c *ClientCollector) Snapshot(ctx context.Context, maxAge time.Duration) []proto.ClientStat {
	if c == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c.sampleOnce.Do(func() { c.sampleGate = make(chan struct{}, 1) })
	select {
	case c.sampleGate <- struct{}{}:
		defer func() { <-c.sampleGate }()
	case <-ctx.Done():
		return nil
	}
	c.cacheMu.Lock()
	list, fresh := c.cache, maxAge > 0 && c.cacheOK && time.Since(c.cacheAt) < maxAge
	c.cacheMu.Unlock()
	if ctx.Err() != nil {
		return nil
	}
	if fresh {
		return list
	}
	list = c.sample(ctx)
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if list == nil || ctx.Err() != nil {
		c.cacheOK = false
		return nil
	}
	c.cache, c.cacheAt, c.cacheOK = list, time.Now(), true
	return list
}

// sample performs one full collection. The caller holds sampleGate, so two
// consumers can never read the same BSS at the same time.
func (c *ClientCollector) sample(ctx context.Context) []proto.ClientStat {
	bsses := c.bssList(ctx)
	if bsses == nil {
		return nil
	}
	neigh := c.neighbours(ctx)

	clients := make([]proto.ClientStat, 0, 16)
	for _, b := range bsses {
		if ctx.Err() != nil {
			return nil
		}
		list, err := c.stations(ctx, b, neigh)
		if err != nil {
			return nil
		}
		clients = append(clients, list...)
	}
	return clients
}

// bssList returns the BSSes to sample, or nil when the list is unavailable. nil
// is not an empty list: no BSS list means the sample is missing, and the
// controller must keep its client table rather than be told everyone left.
func (c *ClientCollector) bssList(ctx context.Context) []bssInfo {
	reader := c.bssSource()
	list, err := reader.BSS(ctx)
	if err == nil {
		return list
	}
	if ctx.Err() != nil {
		return nil
	}
	if _, ok := reader.(netifdBSSReader); ok {
		c.Log.Debug("client collect: wireless status unavailable", "err", err)
		return nil
	}
	// netlink worked for the station table but not for the interface list:
	// fall back for the rest of the process rather than paying a failing
	// syscall on every cycle.
	c.Log.Warn("client collect: netlink BSS list failed, using netifd from now on", "err", err)
	c.setBSSSource(netifdBSSReader{runner: c.Runner})
	fallback := c.bssSource()
	list, err = fallback.BSS(ctx)
	if err != nil {
		c.Log.Debug("client collect: wireless status unavailable", "err", err)
		return nil
	}
	return list
}

func (c *ClientCollector) bssSource() BSSReader {
	c.bssMu.Lock()
	defer c.bssMu.Unlock()
	if c.bssReader == nil {
		c.bssReader = resolveBSSReader(c.Runner, c.Netlink, c.Log)
	}
	return c.bssReader
}

func (c *ClientCollector) setBSSSource(r BSSReader) {
	c.bssMu.Lock()
	defer c.bssMu.Unlock()
	c.bssReader = r
}

// neighbours returns MAC→IPv4 from the configured source. Failure is silent by
// design: an address is a bonus, and a missing one must not cost a client row.
func (c *ClientCollector) neighbours(ctx context.Context) map[string]string {
	reader := c.neighbourSource()
	table, err := reader.Neighbours(ctx)
	if err == nil {
		return table
	}
	if _, ok := reader.(ipNeighbourReader); ok {
		return nil
	}
	c.Log.Debug("client collect: netlink neighbours failed, using ip from now on", "err", err)
	c.neighMu.Lock()
	c.neighRead = ipNeighbourReader{runner: c.Runner}
	c.neighMu.Unlock()
	return nil
}

func (c *ClientCollector) neighbourSource() NeighbourReader {
	c.neighMu.Lock()
	defer c.neighMu.Unlock()
	if c.neighRead == nil {
		c.neighRead = resolveNeighbourReader(c.Runner, c.Netlink, c.Log)
	}
	return c.neighRead
}

// Neighbours exposes the address table of the configured source, error and all.
// The collector itself swallows failures — an address is a bonus — but the
// on-device comparison tool needs to see them.
func (c *ClientCollector) Neighbours(ctx context.Context) (map[string]string, error) {
	return c.neighbourSource().Neighbours(ctx)
}

// decorate attaches the per-BSS context a raw station sample does not carry.
func (c *ClientCollector) decorate(list []proto.ClientStat, ifname, ssid, band string, neigh map[string]string) []proto.ClientStat {
	for i := range list {
		list[i].SSID = ssid
		list[i].Band = band
		list[i].Ifname = ifname
		if ip, ok := neigh[list[i].MAC]; ok {
			list[i].IP = ip
		}
	}
	return list
}

// stations reads one BSS's station table through the configured source.
func (c *ClientCollector) stations(ctx context.Context, b bssInfo, neigh map[string]string) ([]proto.ClientStat, error) {
	// An empty BSS is the common case on a multi-SSID AP: eight of eleven BSSes
	// on the reference site carry no station at all, and each of them would
	// otherwise cost a full read to learn nothing.
	if c.skipsEmptyBSS(b.Ifname) {
		return nil, nil
	}
	reader := c.stationReader()
	list, err := reader.Stations(ctx, b.Ifname, b.Ifindex)
	if err != nil && ctx.Err() == nil {
		if _, alreadyIW := reader.(iwReader); !alreadyIW {
			fallback := iwReader{runner: c.Runner}
			if errors.Is(err, ErrStationSourceUnsupported) {
				c.readerMu.Lock()
				c.reader = fallback
				c.readerMu.Unlock()
			}
			list, err = fallback.Stations(ctx, b.Ifname, b.Ifindex)
		}
	}
	if err != nil {
		if !c.warnedMissing && c.Log != nil {
			// iw is not part of the minimal OpenWrt image on every build, and
			// netlink can be missing from a stripped kernel: report once and
			// keep provisioning working without client telemetry.
			c.Log.Warn("client collect: incomplete sweep, omitting client report",
				"source", reader.Name(), "ifname", b.Ifname, "err", err)
			c.warnedMissing = true
		}
		return nil, err
	}
	return c.decorate(list, b.Ifname, b.SSID, b.Band, neigh), nil
}

// stationReader returns the configured source, building it on first use.
func (c *ClientCollector) stationReader() StationReader {
	c.readerMu.Lock()
	defer c.readerMu.Unlock()
	if c.reader == nil {
		c.reader = resolveStationSource(c.Source, c.Runner, c.Log)
	}
	return c.reader
}

// parseStationDump parses `iw dev <ifname> station dump`. Kept separate from the
// collector so the netlink reader can be verified against it field by field.
func parseStationDump(out string) []proto.ClientStat {
	var (
		result []proto.ClientStat
		cur    *proto.ClientStat
	)
	flush := func() {
		if cur == nil {
			return
		}
		result = append(result, *cur)
		cur = nil
	}

	for _, line := range strings.Split(out, "\n") {
		if m := stationRe.FindStringSubmatch(line); m != nil {
			flush()
			cur = &proto.ClientStat{MAC: strings.ToLower(m[1])}
			continue
		}
		if cur == nil {
			continue
		}
		if m := signalRe.FindStringSubmatch(line); m != nil {
			cur.Signal, _ = strconv.Atoi(m[1])
			continue
		}
		if m := rxBitrateRe.FindStringSubmatch(line); m != nil {
			cur.RxRateMbps = roundMbps(m[1])
			continue
		}
		if m := txBitrateRe.FindStringSubmatch(line); m != nil {
			cur.TxRateMbps = roundMbps(m[1])
			continue
		}
		if m := connectedRe.FindStringSubmatch(line); m != nil {
			cur.ConnectedSeconds, _ = strconv.Atoi(m[1])
			continue
		}
		if m := inactiveRe.FindStringSubmatch(line); m != nil {
			ms, _ := strconv.Atoi(m[1])
			cur.InactiveSeconds = ms / 1000
			continue
		}
	}
	flush()
	return result
}

var neighRe = regexp.MustCompile(`^(\d+\.\d+\.\d+\.\d+)\s+dev\s+\S+\s+lladdr\s+([0-9a-fA-F:]{17})`)

func parseNeighbours(out string) map[string]string {
	result := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if m := neighRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			result[strings.ToLower(m[2])] = m[1]
		}
	}
	return result
}

func roundMbps(s string) int {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	// 1200.9 -> 1201; the dashboard never needs sub-Mbps precision.
	return int(f + 0.5)
}
