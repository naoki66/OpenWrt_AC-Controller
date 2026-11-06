package agent

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/naoki66/ac-controller/internal/proto"
)

// AP-to-AP roaming coordination.
//
// The controller already builds 802.11k neighbour lists from its stored BSS
// identities (docs/ac-11k-neighbors.md). This file adds the fallback the design
// needs: when the controller is unreachable, when an AP joins while it is down,
// or when hostapd has forgotten its lists, agents merge each other's reports
// directly and write them back locally. Nothing here depends on a working AC.
//
// Local 11v suggestions (PeerSteer) are on by default and deliberately
// conservative. APs cannot measure a station that is associated elsewhere, so a
// suggestion is only made when the local link is already weak enough that staying
// is the worse option. There is no forced disassociation, no kick, and no repeated
// nagging. The same path keeps working after the controller is lost, for any
// number of APs, without depending on nrsyncd.

const (
	// steerWeakSignal is the local RSSI at or below which a client is about to
	// lose its link (-78 dBm). Above it, the client is left alone.
	steerWeakSignal = -78
	// steerMinConnected avoids fighting the association the client just made.
	steerMinConnected = 30
	// steerCooldown is per-client; a client that did not take the suggestion is
	// not asked again for a while.
	steerCooldown = 2 * time.Minute
	// steerMaxPerCycle bounds shell calls in one cycle; one suggestion is
	// enough to demonstrate the path without turning a weak client into a burst
	// of BTM frames.
	steerMaxPerCycle = 1
	// steerMaxCandidates keeps the BTM neighbour list short and deterministic.
	steerMaxCandidates = 4
	// steerStaleSeconds skips a station whose last frame is older than this.
	// mac80211 keeps the station entry of a client that roamed to another AP
	// until the inactivity timeout (5 min by default), so a naive read would
	// keep steering a ghost that already left (observed on 10.0.30.251,
	// 2026-11-03: BTM suggestions every 2 minutes for a client on the peer AP).
	steerStaleSeconds = 60
)

// coordinatePeers publishes this AP's state and, when enabled, makes a local
// roaming suggestion. Called from buildHello, so it runs even on cycles where
// the controller cannot be reached.
func (a *Agent) coordinatePeers(ctx context.Context, h proto.Hello) {
	// Keep the freshest self-report; the periodic neighbour restore uses it to
	// rebuild lists without another ubus read.
	a.lastOwnNeighbors = h.Neighbors
	if a.peerBus == nil {
		return
	}
	st := peerStateFrom(h)
	// Peers carry the applied site time sources: a new AP that joined while the
	// controller was down can adopt the list without ever seeing a hello reply.
	st.NTP = a.state.NTPServers
	a.peerBus.Announce(st)
	snap := a.peerBus.Snapshot()
	if len(snap.NTP) > 0 {
		a.peerNTP = snap.NTP
	}
	a.maybeSteer(ctx, h)
}

// neighborBody merges the controller's cached lists with this AP's own reports
// and every live peer's reports.
//
// Precedence does not matter: entries are deduplicated by BSSID and all three
// sources describe the same thing. Keeping the controller's entries means a
// peer outage cannot make a previously good list worse, while adding peers
// covers the controller outage case.
func (a *Agent) neighborBody() []byte {
	if a.Applier == nil {
		return nil
	}
	var controller map[string][][]string
	if cached := a.Applier.cachedNeighbors(); cached != nil {
		if m, err := decodeNeighbors(cached); err == nil {
			controller = m
		}
	}

	var peers []proto.NeighborBSS
	if a.peerBus != nil {
		peers = a.peerBus.Snapshot().BSS
	}

	merged := mergeNeighborLists(controller, a.lastOwnNeighbors, peers)
	if len(merged) == 0 {
		return nil
	}
	body, err := json.Marshal(merged)
	if err != nil {
		return nil
	}
	return body
}

// mergeNeighborLists builds the per-interface rrm_nr_set payload.
//
// The key must be a local interface; entries from peers that serve a different
// SSID are excluded, because a client told to move to a BSS it cannot join will
// start treating the whole list as noise. hostapd adds the executing BSS itself,
// so the BSS's own BSSID is never written back.
func mergeNeighborLists(controller map[string][][]string, own, peers []proto.NeighborBSS) map[string][][]string {
	if len(own) == 0 {
		// No local report yet: the controller's list is all we know.
		return controller
	}

	type localBSS struct {
		ssid  string
		bssid string
	}
	local := map[string]localBSS{}
	for _, b := range own {
		if b.Ifname == "" || b.BSSID == "" {
			continue
		}
		local[b.Ifname] = localBSS{ssid: b.SSID, bssid: strings.ToLower(b.BSSID)}
	}

	out := map[string][][]string{}
	for ifname, self := range local {
		seen := make(map[string]bool)
		var list [][]string
		add := func(entry []string) {
			if len(entry) < 1 || entry[0] == "" {
				return
			}
			key := strings.ToLower(entry[0])
			if key == self.bssid || seen[key] {
				return
			}
			seen[key] = true
			list = append(list, entry)
		}

		for _, entry := range controller[ifname] {
			add(entry)
		}
		// Sibling BSSes on this AP (same SSID, different BSSID) are legitimate
		// neighbours too, and the controller list may predate them.
		for _, b := range own {
			if b.Ifname == ifname || b.SSID != self.ssid || b.BSSID == "" || b.Report == "" {
				continue
			}
			add([]string{b.BSSID, b.SSID, b.Report})
		}
		for _, b := range peers {
			if b.SSID != self.ssid || b.BSSID == "" || b.Report == "" {
				continue
			}
			add([]string{b.BSSID, b.SSID, b.Report})
		}

		if len(list) == 0 {
			continue
		}
		sort.Slice(list, func(i, j int) bool {
			return strings.ToLower(list[i][0]) < strings.ToLower(list[j][0])
		})
		out[ifname] = list
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// peerStateFrom converts a hello into the compact announcement peers consume.
func peerStateFrom(h proto.Hello) proto.PeerState {
	st := proto.PeerState{
		MAC:      h.MAC,
		Model:    h.Model,
		Hostname: h.Hostname,
		BSS:      h.Neighbors,
	}
	for _, c := range h.Clients {
		st.Clients = append(st.Clients, proto.PeerClient{
			MAC:              c.MAC,
			Ifname:           c.Ifname,
			SSID:             c.SSID,
			Band:             c.Band,
			Signal:           c.Signal,
			ConnectedSeconds: c.ConnectedSeconds,
		})
	}
	for _, r := range h.Radios {
		st.Radios = append(st.Radios, proto.PeerRadio{
			Band:        r.Band,
			Channel:     r.Channel,
			Utilization: r.Utilization,
			Clients:     r.Clients,
		})
	}
	return st
}

// maybeSteer sends at most one 11v suggestion per cycle.
//
// Best-effort hint: without a measurement of the candidate link this can only
// advise. When usteer/dawn is enabled the agent stays out of the way entirely
// — two decision-makers fight over the same client. nrsyncd only distributes
// 11k neighbour lists, so it does not conflict. A device that has never been
// adopted is left alone: its local wireless may not even be controller-shaped
// yet, so it must not start steering clients on its own.
func (a *Agent) maybeSteer(ctx context.Context, h proto.Hello) {
	if !a.Config.PeerSteer || a.Config.DryRun || a.Applier == nil || len(h.Clients) == 0 || a.state.AppliedRev == 0 {
		return
	}
	if steeringTakesOver(a.steeringState(ctx)) {
		return
	}
	// A template-provided steering policy supersedes the ad-hoc suggestion:
	// the loop is watching the same clients at a much finer cadence, and two
	// decision-makers would just double the BTM traffic.
	if a.steer != nil {
		if _, active := a.steer.policySnapshot(); active {
			return
		}
	}
	snapshot := a.peerBus.Snapshot()
	if len(snapshot.BSS) == 0 {
		return
	}

	ownBSSID := map[string]string{}
	for _, b := range h.Neighbors {
		if b.Ifname != "" {
			ownBSSID[b.Ifname] = strings.ToLower(b.BSSID)
		}
	}

	now := time.Now()
	attempts := 0

	for _, c := range h.Clients {
		if attempts >= steerMaxPerCycle {
			break
		}
		if c.Ifname == "" || c.SSID == "" || c.Signal == 0 || c.Signal > steerWeakSignal {
			continue
		}
		if c.InactiveSeconds >= steerStaleSeconds {
			continue
		}
		if time.Duration(c.ConnectedSeconds)*time.Second < steerMinConnected {
			continue
		}
		// Same rate gate as the policy loop, and here it matters more: this
		// path runs on every heartbeat, so it is the one that used to nag a
		// client whose signal merely looks bad.
		if a.steer != nil && a.steer.rates != nil {
			if best, known := a.steer.rates.observe(c.MAC, c.Ifname, c.RxRateMbps, now); known && best >= defaultMinRateMbps {
				a.Log.Debug("11v: weak but still fast, left alone",
					"client", c.MAC, "ifname", c.Ifname, "signal", c.Signal, "rx_mbps", best)
				continue
			}
		}

		// hostapd's bss_transition_request wants full neighbour report elements
		// (the hex body from rrm_nr_get_own), not bare BSSIDs: anything else is
		// rejected with "Invalid argument" (verified on 10.0.30.251, 2026-11-03).
		//
		// The pool goes through the same quality filter the policy loop uses, so
		// this path cannot point a client at a BSS on its own channel either.
		var pooled [][]string
		for _, b := range snapshot.BSS {
			if b.SSID != c.SSID || b.BSSID == "" || b.Report == "" {
				continue
			}
			if strings.ToLower(b.BSSID) == ownBSSID[c.Ifname] {
				continue
			}
			pooled = append(pooled, []string{b.BSSID, b.SSID, b.Report})
		}
		self := neighborInfo{}
		for _, b := range h.Neighbors {
			if b.Ifname != c.Ifname || b.Report == "" {
				continue
			}
			if info, ok := parseNeighborReport(b.Report); ok {
				self = info
			}
			break
		}
		pooled = rankCandidates(pooled, self)
		if len(pooled) > steerMaxCandidates {
			pooled = pooled[:steerMaxCandidates]
		}
		candidates := make([]string, 0, len(pooled))
		candidateBSSIDs := make([]string, 0, len(pooled))
		for _, e := range pooled {
			candidates = append(candidates, e[2])
			candidateBSSIDs = append(candidateBSSIDs, e[0])
		}
		if len(candidates) == 0 {
			continue
		}

		// Pace the client and, more importantly, stop talking to one that has
		// declined three times: a client without 11v will never act on a BTM,
		// and repeating it only spends airtime and log lines.
		assocAt := assocTime(now, c.ConnectedSeconds)
		if ok, reason := a.btm.allow(c.MAC, c.Ifname, assocAt, steerCooldown); !ok {
			if reason == "silenced" {
				a.Log.Debug("11v suggestion withheld, client keeps declining", "client", c.MAC)
			}
			continue
		}

		// Capability gate, shared with the policy loop: a station that declared
		// no 11k/11v capability cannot parse the neighbour report elements this
		// request carries, so one ubus question replaces three wasted BTMs.
		if a.steer != nil {
			if supports, known := a.steer.clientSupportsBTM(ctx, c.Ifname, c.MAC, assocAt); known && !supports {
				a.btm.silence(c.MAC)
				a.Log.Info("11v: client cannot parse a BTM, leaving it alone",
					"client", c.MAC, "ifname", c.Ifname, "signal", c.Signal, "rx_mbps", c.RxRateMbps)
				continue
			}
		}

		arg, err := json.Marshal(map[string]any{
			"addr":                    c.MAC,
			"disassociation_imminent": false,
			"abridged":                true,
			"neighbors":               candidates,
		})
		if err != nil {
			continue
		}
		_, err = a.Runner.Run(ctx, "ubus", "call", "hostapd."+c.Ifname, "bss_transition_request", string(arg))
		a.btm.sent(c.MAC, c.Ifname, assocAt)
		a.adhocBTMs++
		attempts++
		if err != nil {
			a.Log.Warn("11v suggestion failed", "client", c.MAC, "ifname", c.Ifname, "err", err)
			continue
		}
		// Debug, not Info: one suggestion per weak client per cooldown is normal
		// operation, and at Info level it floods logread on a site that always
		// has one sticky client. The counter in status.json is the durable view.
		a.Log.Debug("suggested roaming", "client", c.MAC, "signal", c.Signal,
			"from", c.Ifname, "candidates", strings.Join(candidateBSSIDs, ","))
	}
}
