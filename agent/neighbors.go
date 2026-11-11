package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/naoki66/ac-controller/shared/proto"
)

// NeighborCollector reports how this AP wants to be advertised under 802.11k.
//
// The controller needs one entry per BSS to build the peer lists it pushes back,
// and it must be hostapd's own encoding: operating class, channel and PHY type
// are derived from runtime state that the controller cannot see from a template.
// Read-only, same as the client collector.
type NeighborCollector struct {
	Runner Runner
	Log    *slog.Logger
	// Ubus, when set, answers the per-BSS self-report over the ubusd socket. Nil
	// means the CLI, which is both the previous behaviour and the fallback.
	//
	// The report body is opaque, so this is the one
	// call where "do not interpret" matters most: moving it to the socket keeps
	// the bytes and the decoder exactly as they were.
	Ubus *ubusClient
}

// call returns one ubus method's JSON reply, preferring the socket.
func (c *NeighborCollector) call(ctx context.Context, obj, method string) ([]byte, error) {
	return ubusReply(ctx, c.Ubus, c.Runner, c.Log, obj, method)
}

// ownReport is the shape of `ubus call hostapd.<if> rrm_nr_get_own`.
type ownReport struct {
	Value []string `json:"value"`
}

// Collect returns one entry per hostapd BSS. Interfaces whose report cannot be
// read are skipped: an empty entry for one BSS must not blind the other 17.
func (c *NeighborCollector) Collect(ctx context.Context) []proto.NeighborBSS {
	out, err := c.Runner.Run(ctx, "ubus", "list")
	if err != nil {
		c.Log.Debug("neighbor collect: ubus list unavailable", "err", err)
		return nil
	}

	var res []proto.NeighborBSS
	for _, obj := range strings.Split(out, "\n") {
		ifname, ok := strings.CutPrefix(strings.TrimSpace(obj), "hostapd.")
		if !ok || ifname == "" {
			continue
		}
		raw, err := c.call(ctx, "hostapd."+ifname, "rrm_nr_get_own")
		if err != nil {
			// A BSS that does not implement rr, or is mid-reload, is not fatal.
			c.Log.Debug("neighbor collect: rrm_nr_get_own failed", "ifname", ifname, "err", err)
			continue
		}
		var own ownReport
		if err := json.Unmarshal(raw, &own); err != nil || len(own.Value) != 3 {
			c.Log.Debug("neighbor collect: unexpected report shape", "ifname", ifname)
			continue
		}
		res = append(res, proto.NeighborBSS{
			Ifname: ifname,
			BSSID:  own.Value[0],
			SSID:   own.Value[1],
			Report: own.Value[2],
		})
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Ifname < res[j].Ifname })
	return res
}

// decodeNeighbors parses the neighbours file from a configuration package.
//
// The file is a JSON object keyed by interface name, and each value is the exact
// array rrm_nr_set expects, so the agent never interprets the report body.
func decodeNeighbors(body []byte) (map[string][][]string, error) {
	var m map[string][][]string
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse neighbours: %w", err)
	}
	return m, nil
}

// EnsureNeighbors makes hostapd's 11k peer lists match the controller's intent.
//
// This is deliberately not UCI: hostapd keeps the list in memory only, so it has
// to be re-asserted after every reload, radio restart or reboot. Without the
// re-assertion the lists would silently stay empty until the next configuration
// change, which may be weeks away — the feature would look installed and do
// nothing.
//
// Cheap: one read per BSS, and a write only where the BSSID set diverges. Safe
// to call on every cycle, though the agent paces it.
//
// A write is followed by a read-back, because a successful rrm_nr_set does not
// guarantee the entry survives. `wifi reload` returns before hostapd has
// finished rebuilding every radio, so a list written during that window lands on
// an instance that is about to be replaced — observed on real hardware, where an
// apply left all 11 lists empty until the next cycle. Read-back turns that silent
// loss into a retry.
func (a *Applier) EnsureNeighbors(ctx context.Context, body []byte) {
	if a.DryRun {
		// A dry run must not modify the device at all. Writing the lists would
		// change hostapd's runtime state even though no file was touched, and
		// the staged copy of the neighbours file is enough to review them.
		return
	}
	want, err := decodeNeighbors(body)
	if err != nil {
		a.Log.Warn("neighbours file ignored", "err", err)
		return
	}
	// One `ubus list` for this sweep; every hasInterface below reuses it.
	a.invalidateBSSSet()

	names := make([]string, 0, len(want))
	for ifname := range want {
		names = append(names, ifname)
	}
	sort.Strings(names)

	for _, ifname := range names {
		list := want[ifname]
		if !a.hasInterface(ctx, ifname) {
			// The BSS does not exist locally (template changed, radio up still
			// in progress). hostapd would reject the call anyway.
			a.Log.Debug("neighbour target absent, skipped", "ifname", ifname)
			continue
		}
		if a.neighbourListMatches(ctx, ifname, list) {
			continue
		}
		arg, err := json.Marshal(map[string]any{"list": list})
		if err != nil {
			continue
		}

		for attempt := 1; attempt <= neighbourSetAttempts; attempt++ {
			if _, err := a.Runner.Run(ctx, "ubus", "call", "hostapd."+ifname, "rrm_nr_set", string(arg)); err != nil {
				a.Log.Warn("rrm_nr_set failed", "ifname", ifname, "entries", len(list), "err", err)
				break
			}
			if a.neighbourListMatches(ctx, ifname, list) {
				a.Log.Info("11k neighbour list set", "ifname", ifname, "entries", len(list), "attempt", attempt)
				break
			}
			if attempt < neighbourSetAttempts {
				// hostapd is probably still rebuilding this radio.
				a.sleep(neighbourRetryDelay * time.Duration(attempt))
			} else {
				a.Log.Warn("11k neighbour list did not stick", "ifname", ifname, "entries", len(list))
			}
		}
	}
}

// pendingNeighbors lists the BSSes that do not hold their wanted list yet.
// A BSS hostapd does not expose counts as pending: it is the one that is still
// coming up, and it is exactly the one we have to come back for.
func (a *Applier) pendingNeighbors(ctx context.Context, want map[string][][]string) []string {
	// Each pass polls the live set: the whole point of the settle window is to
	// see a BSS that was not there on the previous pass.
	a.invalidateBSSSet()

	names := make([]string, 0, len(want))
	for ifname := range want {
		names = append(names, ifname)
	}
	sort.Strings(names)

	var pending []string
	for _, ifname := range names {
		if !a.hasInterface(ctx, ifname) {
			pending = append(pending, ifname+"(absent)")
			continue
		}
		if !a.neighbourListMatches(ctx, ifname, want[ifname]) {
			pending = append(pending, ifname)
		}
	}
	return pending
}

// settleNeighbors re-asserts the lists until every BSS holds them, or the settle
// window runs out.
//
// Why this exists: the health check treats a radio as up as soon as hostapd
// reports the radio up, but a BSS on a radio that is still doing ACS is not
// serviced yet. Measured on an XR1710G: `wifi reload` returned, the radio
// reported up, and the 2.4 GHz BSS only reached AP-ENABLED nine seconds later.
// hostapd drops whatever was written to it while it was still starting, so the
// first pass after an apply silently misses those BSSes and they stay empty
// until the periodic refresh — fifteen cycles, about five minutes — happens to
// run. Everything looks applied while 11k is quietly doing nothing.
//
// This polls the end state instead of a readiness signal, so it does not depend
// on guessing when hostapd is "really" up, and it cannot fail an apply: it only
// ever writes lists, and a BSS that never comes up is reported and left to the
// periodic refresh.
func (a *Applier) settleNeighbors(ctx context.Context, body []byte) {
	want, err := decodeNeighbors(body)
	if err != nil {
		return
	}

	window := a.NeighbourSettle
	if window == 0 {
		window = neighbourSettleTimeout
	}
	deadline := time.Now().Add(window)

	for {
		pending := a.pendingNeighbors(ctx, want)
		if len(pending) == 0 {
			return
		}
		if time.Now().After(deadline) {
			a.Log.Warn("11k neighbour lists still incomplete after settle window",
				"pending", strings.Join(pending, ", "), "window", window)
			return
		}
		a.EnsureNeighbors(ctx, body)
		select {
		case <-ctx.Done():
			return
		default:
		}
		a.sleep(neighbourSettlePoll)
	}
}

// neighbourSettleTimeout bounds the settle window and neighbourSettlePoll paces
// it. ACS takes a few seconds; a DFS channel can legally take a minute before a
// BSS is enabled, so the window is generous. Anything slower than this is not
// worth blocking an apply for — the periodic refresh picks it up.
const (
	neighbourSettleTimeout = 60 * time.Second
	neighbourSettlePoll    = 2 * time.Second
)

// neighbourSetAttempts and neighbourRetryDelay bound the retry loop. Three tries
// over roughly a second covers a radio completing its rebuild without making a
// genuinely broken BSS expensive to poll.
const (
	neighbourSetAttempts = 3
	neighbourRetryDelay  = 250 * time.Millisecond
)

// neighbourListMatches reports whether hostapd currently holds the wanted set.
func (a *Applier) neighbourListMatches(ctx context.Context, ifname string, list [][]string) bool {
	cur, err := a.Runner.Run(ctx, "ubus", "call", "hostapd."+ifname, "rrm_nr_list")
	if err != nil {
		return false
	}
	return sameNeighborList(cur, list)
}

// sleep is injectable so tests do not wait out the retry backoff.
func (a *Applier) sleep(d time.Duration) {
	if a.Sleep != nil {
		a.Sleep(d)
		return
	}
	time.Sleep(d)
}

// hostapdBSSSet returns the set of BSS interfaces hostapd currently exposes.
//
// The callers (EnsureNeighbors, EnsureWireless, pendingNeighbors) need this once
// per BSS, and `ubus list` costs the same regardless of how many objects it
// returns, so one listing per sweep replaces eleven spawns.
//
// The set is cached only for the duration of one sweep, not for a wall-clock
// TTL: each sweep invalidates first. A time-based cache was wrong here for two
// reasons. The settle loop has to notice a BSS that appears mid-window, and a
// cache that outlives the check hides exactly that; and a TTL keyed on the
// injectable clock never expires at all under a frozen test clock, which turns
// a latency tweak into a stuck radio. A nil result means ubus could not answer,
// which every caller already treats as "not present".
func (a *Applier) hostapdBSSSet(ctx context.Context) map[string]bool {
	a.bssMu.Lock()
	defer a.bssMu.Unlock()
	if a.bssSet != nil {
		return a.bssSet
	}
	out, err := a.Runner.Run(ctx, "ubus", "list")
	if err != nil {
		return nil
	}
	set := make(map[string]bool, 16)
	for _, line := range strings.Split(out, "\n") {
		if ifname, ok := strings.CutPrefix(strings.TrimSpace(line), "hostapd."); ok && ifname != "" {
			set[ifname] = true
		}
	}
	a.bssSet = set
	return set
}

// invalidateBSSSet drops the cached set so the next sweep reads the live one.
// Called at the start of every sweep, and before an apply: hostapd restarts
// there, and a cached answer would then describe an instance that is gone.
func (a *Applier) invalidateBSSSet() {
	a.bssMu.Lock()
	a.bssSet = nil
	a.bssMu.Unlock()
}

// hasInterface reports whether hostapd currently exposes the BSS.
func (a *Applier) hasInterface(ctx context.Context, ifname string) bool {
	set := a.hostapdBSSSet(ctx)
	if set == nil {
		return false
	}
	return set[ifname]
}

// cacheNeighbors persists the peer lists where a reboot can find them again.
// hostapd forgets rrm_nr_set contents on restart, and the controller only pushes
// a revision when something changed, so without a local copy the lists would not
// come back until the next template edit.
func (a *Applier) cacheNeighbors(body []byte) {
	if err := os.MkdirAll(a.StateDir, 0o755); err != nil {
		a.Log.Debug("neighbour cache dir failed", "err", err)
		return
	}
	path := filepath.Join(a.StateDir, neighborsCacheFile)
	if old, err := os.ReadFile(path); err == nil && string(old) == string(body) {
		return
	}
	if err := writeAll(map[string]string{path: string(body)}); err != nil {
		a.Log.Debug("neighbour cache write failed", "err", err)
	}
}

// cachedNeighbors returns the last peer lists received, or nil.
func (a *Applier) cachedNeighbors() []byte {
	body, err := os.ReadFile(filepath.Join(a.StateDir, neighborsCacheFile))
	if err != nil {
		return nil
	}
	return body
}

// neighborsCacheFile lives in the agent's state directory, not in /etc/config:
// it is derived data that the controller can always regenerate.
const neighborsCacheFile = "neighbors.json"

// sameNeighborList compares hostapd's current list against the desired one.
// Compare the full report: a channel change can keep the same BSSID.
func sameNeighborList(cur string, want [][]string) bool {
	var parsed struct {
		List [][]string `json:"list"`
	}
	if err := json.Unmarshal([]byte(cur), &parsed); err != nil {
		return false
	}
	if len(parsed.List) != len(want) {
		return false
	}
	key := func(e []string) [3]string {
		return [3]string{strings.ToLower(e[0]), e[1], strings.ToLower(e[2])}
	}
	seen := make(map[[3]string]int, len(want))
	for _, e := range parsed.List {
		if len(e) != 3 {
			return false
		}
		seen[key(e)]++
	}
	for _, e := range want {
		if len(e) != 3 {
			return false
		}
		n := key(e)
		if seen[n] == 0 {
			return false
		}
		seen[n]--
	}
	return true
}
