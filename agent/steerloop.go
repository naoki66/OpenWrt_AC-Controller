package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/naoki66/ac-controller/shared/model"
	"github.com/naoki66/ac-controller/shared/proto"
)

// Local steering loop: the execution half of the 11v enhancement.
//
// The controller defines the policy and hands it down in the package; the
// loop runs on the AP at a few seconds' cadence — far below the hello
// interval — because sticky-client detection is inherently local. The shapes
// mirror the surveyed products: consecutive weak samples trigger a BTM with
// the AC-aggregated neighbour candidates, a client that ignores the BTM can be
// deauthenticated after ForceAfterMS (Omada "Force-Disassociation"), the ban
// keeps it off this BSS while it tries a neighbour (Huawei's re-association
// suppression), and a cooldown keeps a client that never roams from being
// kicked in circles.
//
// Everything here yields: no policy, dry-run, a steering daemon (usteer/dawn)
// enabled, or no candidate BSS — and the loop does nothing. nrsyncd only
// distributes 11k neighbour lists and does not conflict. Kicking without a
// better option is how disconnect storms are made.

const (
	defaultSteerInterval = 3 * time.Second
	// defaultSteerIdleInterval is the cadence used while no client needs
	// attention. Ten seconds is still an order of magnitude below the heartbeat
	// and far below the time a client takes to notice a dead link, so nothing is
	// missed; it simply stops paying for a fast sweep on a quiet AP.
	defaultSteerIdleInterval = 10 * time.Second
	// steerMapLimit bounds the per-client maps; a busy AP must not remember
	// every station forever. Mirrors the bound in maybeSteer.
	steerMapLimit = 256
	// bindingSteerCooldown spaces the BTM suggestions sent to a client that is
	// on the wrong SSID. Magnet steering is advice only: it never force-kicks.
	bindingSteerCooldown = 5 * time.Minute
	// bindingMinConnected protects a client that has just associated from being
	// immediately told to move again (same rationale as the policy default).
	bindingMinConnected = 30 * time.Second
	// steerSampleReuse is the freshness budget for the shared client sample. It
	// is shorter than every steering cadence, so the loop still sees a fresh
	// signal each tick; its only job is to collapse a simultaneous heartbeat
	// sample into the same collection instead of running `iw` twice.
	steerSampleReuse = 2 * time.Second
	// defaultMinRateMbps is the receive rate at which a weak-signal client is
	// considered healthy. 24 Mbps carries 4K video and ordinary office work;
	// below that a client is genuinely struggling, whatever its RSSI says.
	defaultMinRateMbps = 24
)

// effectiveMinRate resolves the policy's rate gate: zero defers to the
// default, negative disables the check.
func effectiveMinRate(policyValue int) int {
	if policyValue == 0 {
		return defaultMinRateMbps
	}
	return policyValue
}

// SteerCounts are lifetime counters since agent start, published in the status
// file so an operator can see the loop is doing something. Suppression for
// missing capability is counted by the guard, which both steering paths share.
type SteerCounts struct {
	BTM    int `json:"btm"`
	Forced int `json:"forced"`
}

// pendingKick remembers a BTM that may need forceful follow-up.
type pendingKick struct {
	ifname   string
	deadline time.Time
}

// SteerLoop runs the local detection/decision/execution cycle.
type SteerLoop struct {
	Agent *Agent
	// Interval is the fast cadence: a client needs watching right now.
	Interval time.Duration
	// IdleInterval is the slow cadence used when nothing needs attention. Zero
	// or less than Interval means "always fast", which is what tests and a
	// policy with a very short consecutive threshold want.
	IdleInterval time.Duration
	// Now is overridable for tests.
	Now func() time.Time

	mu       sync.Mutex
	policy   *model.SteerPolicy
	counts   SteerCounts
	weak     map[string]int
	cooldown map[string]time.Time
	pending  map[string]pendingKick
	// bindings is the controller's client→SSID intent, keyed by lower-case MAC.
	// It is independent of policy: an AP can be told to magnet a client to one
	// SSID without running the weak-signal steering loop at all.
	bindings map[string]proto.ClientBinding
	// merged is the per-tick candidate pool, keyed by interface. Rebuilt every
	// tick; reading it needs the same mutex as everything else.
	merged map[string][][]string
	// rates holds each client's recent best receive rate, the second opinion
	// the weak-signal test consults (ratewindow.go).
	rates *rateWindow
	// caps caches what each station declared it can do, so the capability gate
	// costs one ubus call per association (btmcap.go).
	caps *btmCapCache
}

// NewSteerLoop builds the loop. Policy and bindings arrive later.
func NewSteerLoop() *SteerLoop {
	return &SteerLoop{
		Interval:     defaultSteerInterval,
		IdleInterval: defaultSteerIdleInterval,
		Now:          time.Now,
		weak:         map[string]int{},
		cooldown:     map[string]time.Time{},
		pending:      map[string]pendingKick{},
		bindings:     map[string]proto.ClientBinding{},
		rates:        newRateWindow(steerMapLimit),
		caps:         newBTMCapCache(),
	}
}

// parseSteerPolicy decodes the steer-policy file from a package. Validation
// mirrors model.SteerPolicy.Validate; a policy that would misbehave is
// rejected rather than run.
func parseSteerPolicy(raw []byte) (*model.SteerPolicy, error) {
	var p model.SteerPolicy
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &p); err != nil {
		return nil, fmt.Errorf("steer-policy: %w", err)
	}
	p.Normalize()
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("steer-policy: %w", err)
	}
	return &p, nil
}

// SetPolicy installs a new policy and resets the per-client state: thresholds
// may have moved, so old consecutive-weak counts no longer mean anything.
func (s *SteerLoop) SetPolicy(p *model.SteerPolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policy = p
	s.weak = map[string]int{}
	s.cooldown = map[string]time.Time{}
	s.pending = map[string]pendingKick{}
}

// SetBindings installs the controller's client→SSID preferences. The
// per-client detection maps are reset: intent changed, so old streaks and
// cooldowns no longer describe what the AP should do.
func (s *SteerLoop) SetBindings(list []proto.ClientBinding) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindings = map[string]proto.ClientBinding{}
	for _, b := range list {
		mac := strings.ToLower(strings.TrimSpace(b.MAC))
		if mac == "" || b.SSID == "" {
			continue
		}
		b.MAC = mac
		s.bindings[mac] = b
	}
	s.weak = map[string]int{}
	s.cooldown = map[string]time.Time{}
	s.pending = map[string]pendingKick{}
}

// snapshot returns whether the loop has any intent plus the counters, for status.
func (s *SteerLoop) snapshot() (bool, SteerCounts) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policy != nil || len(s.bindings) > 0, s.counts
}

// run drives the loop until the context is cancelled. A tick that errors is
// logged and retried on the next tick; the loop never stops on its own.
//
// Two cadences, not one. A fixed 3 s tick costs the same on a quiet AP as on a
// busy one: it samples every BSS whether or not any client is worth acting on.
// The loop therefore runs fast only while something needs watching — a weak
// client, an unfinished kick, a station on the wrong SSID — and falls back to a
// ten-second sweep otherwise. Detection latency is unchanged where it matters,
// because the first weak sample promotes the loop to the fast cadence
// immediately; on an idle site the cost drops by two thirds.
func (s *SteerLoop) run(ctx context.Context) {
	hot := s.Interval
	if hot <= 0 {
		hot = defaultSteerInterval
	}
	idle := s.IdleInterval
	if idle < hot {
		idle = hot
	}
	if !s.waitIntent(ctx) {
		return
	}
	if p := s.policySnapshotStrict(); p != nil {
		s.Agent.Log.Info("steer loop running",
			"interval", hot.String(), "idle_interval", idle.String(), "btm_snr", p.BtmSNR)
	} else {
		s.Agent.Log.Info("steer loop running (client bindings only)",
			"interval", hot.String(), "idle_interval", idle.String())
	}
	// The first pass runs immediately: a policy that just arrived should act on
	// the state it already describes rather than wait out a cadence.
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		next := idle
		if s.tick(ctx) {
			next = hot
		}
		timer.Reset(next)
	}
}

// waitIntent blocks until a policy or a binding arrives (or the context ends),
// so the loop does not spin on an AP with no steering intent at all.
func (s *SteerLoop) waitIntent(ctx context.Context) bool {
	for {
		if ok, _ := s.snapshot(); ok {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Second):
		}
	}
}

func (s *SteerLoop) policySnapshot() (*model.SteerPolicy, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policy, s.policy != nil
}

func (s *SteerLoop) policySnapshotStrict() *model.SteerPolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policy
}

// tick performs one detection/decision pass. Errors are logged, never fatal:
// one unreadable interface must not stop steering on the others.
//
// It reports whether anything needs closer watching this pass, which selects
// the next cadence: a weak client, an unfinished kick, or a station sitting on
// the wrong SSID promotes the loop to the fast interval. Nothing to do means
// the slow one.
func (s *SteerLoop) tick(ctx context.Context) bool {
	p, hasPolicy := s.policySnapshot()
	s.mu.Lock()
	hasBindings := len(s.bindings) > 0
	s.mu.Unlock()
	if !hasPolicy && !hasBindings {
		return false
	}
	a := s.Agent
	if a == nil || a.Config.DryRun || a.Applier == nil || a.state.AppliedRev == 0 {
		return false
	}
	// Yield to a daemon that steers clients itself, exactly like the ad-hoc
	// suggestion path. nrsyncd is neighbour-list only and may run alongside.
	if steeringTakesOver(a.steeringState(ctx)) {
		return false
	}

	ifaces := s.managedInterfaces(ctx)
	if len(ifaces) == 0 {
		return false
	}

	// One candidate pool per tick: the AC's cached lists merged with this AP's
	// own reports and live peers — the same merge the 11k restore uses, so
	// what a client is told to roam to matches what its BSS advertises.
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
	s.mu.Lock()
	s.merged = merged
	s.mu.Unlock()

	now := s.Now()
	// One sample per tick, shared with the heartbeat through the collector's
	// snapshot cache. Sampling each BSS separately used to duplicate the whole
	// `iw station dump` sweep the hello cycle was already running.
	stats := a.Clients.Snapshot(ctx, steerSampleReuse)
	byIf := make(map[string][]proto.ClientStat, len(ifaces))
	for _, c := range stats {
		if c.Ifname != "" {
			byIf[c.Ifname] = append(byIf[c.Ifname], c)
		}
	}
	attention := false
	for _, ifc := range ifaces {
		for _, c := range byIf[ifc.ifname] {
			if s.processClient(ctx, p, now, ifc, c) {
				attention = true
			}
		}
	}
	s.trimMaps(now)
	return attention
}

// managedInterface is one BSS the loop watches.
type managedInterface struct {
	ifname string
	ssid   string
	bssid  string
	// info is this BSS's own operating point, decoded from its 11k report. It
	// is what makes candidate quality filtering possible: without it the loop
	// cannot tell that a candidate shares this BSS's channel.
	info neighborInfo
}

// managedInterfaces lists the BSSes to sample: the 11k self-report first (it
// is already cached and carries the SSID), falling back to netifd for an AP
// whose reports have not arrived yet.
func (s *SteerLoop) managedInterfaces(ctx context.Context) []managedInterface {
	a := s.Agent
	var out []managedInterface
	seen := map[string]bool{}
	for _, b := range a.neighborCache {
		if b.Ifname != "" && !seen[b.Ifname] {
			seen[b.Ifname] = true
			ifc := managedInterface{ifname: b.Ifname, ssid: b.SSID, bssid: b.BSSID}
			if info, ok := parseNeighborReport(b.Report); ok {
				ifc.info = info
			}
			out = append(out, ifc)
		}
	}
	if len(out) > 0 {
		return out
	}
	list, err := s.interfacesFromNetifd(ctx)
	if err != nil {
		a.Log.Debug("steer: wireless status unavailable", "err", err)
	}
	return list
}

func (s *SteerLoop) interfacesFromNetifd(ctx context.Context) ([]managedInterface, error) {
	out, err := s.Agent.Runner.Run(ctx, "ubus", "call", "network.wireless", "status")
	if err != nil {
		return nil, err
	}
	var status wirelessStatus
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		return nil, err
	}
	var list []managedInterface
	for _, radio := range status {
		for _, iface := range radio.Interfaces {
			if iface.Ifname == "" {
				continue
			}
			list = append(list, managedInterface{ifname: iface.Ifname, ssid: iface.Config.SSID})
		}
	}
	return list, nil
}

// processClient applies the policy to one station sample. c comes from the
// same `iw station dump` parsing the telemetry uses, so signal and connected
// time mean exactly what the controller's client table says they mean.
func (s *SteerLoop) processClient(ctx context.Context, p *model.SteerPolicy, now time.Time, ifc managedInterface, c proto.ClientStat) bool {
	mac := strings.ToLower(c.MAC)
	if mac == "" {
		return false
	}
	// A station dump entry survives a roam until mac80211's inactivity timeout.
	// Steering it would chase a client that already sits on another AP, so a
	// sample with no traffic for steerStaleSeconds is ignored.
	if c.InactiveSeconds >= steerStaleSeconds {
		return false
	}

	// A binding overrides weak-signal steering: the operator asked for this
	// station to be on one SSID, and a signal threshold must not fight that.
	s.mu.Lock()
	binding, bound := s.bindings[mac]
	s.mu.Unlock()
	if bound {
		return s.processBound(ctx, p, now, ifc, mac, c, binding)
	}
	if p == nil {
		return false
	}
	// A pending force kick is checked before any cooldown: it is the outcome
	// of a BTM already sent, not a new steering event.
	if pk, ok := s.takePending(mac); ok {
		if now.Before(pk.deadline) {
			s.putPending(mac, pk)
			// A kick is still pending: keep watching until it resolves.
			return true
		}
		s.force(ctx, p, pk.ifname, mac)
		return true
	}

	if c.ConnectedSeconds*1000 < p.MinConnectedMS {
		return false
	}

	// Strong enough: forget the weak streak entirely.
	if c.Signal > p.BtmSNR {
		s.mu.Lock()
		delete(s.weak, mac)
		s.mu.Unlock()
		return false
	}

	// Weak signal, but does the link still carry traffic? Rate adaptation
	// already reacted to the degradation, so a client that reached a usable
	// rate within the last window is not suffering — it merely looks bad on an
	// RSSI chart. Leaving it alone avoids the worst outcome of this whole
	// subsystem: moving a working client for no gain.
	if minRate := effectiveMinRate(p.MinRateMbps); minRate > 0 && s.rates != nil {
		if best, known := s.rates.observe(mac, ifc.ifname, c.RxRateMbps, now); known && best >= minRate {
			s.mu.Lock()
			delete(s.weak, mac)
			s.mu.Unlock()
			s.Agent.Log.Debug("steer: weak but still fast, left alone",
				"mac", mac, "ifname", ifc.ifname, "signal", c.Signal, "rx_mbps", best, "min_rate_mbps", minRate)
			return false
		}
	}

	s.mu.Lock()
	s.weak[mac]++
	streak := s.weak[mac]
	s.mu.Unlock()
	// A weak client is the reason the fast cadence exists, whether or not the
	// consecutive threshold is reached yet: reaching it sooner is the point.
	if streak < p.BtmConsecutive {
		return true
	}

	// Trigger point. No candidates means nowhere better to go: kicking would
	// only disconnect the client, so the streak is kept and retried later —
	// maybe a neighbour has appeared by then.
	cands := s.candidatesFor(ifc)
	// A target the client already rejected is not offered again during the
	// backoff window; the remaining candidates are still usable.
	cands = s.Agent.btm.filterTargets(mac, cands)
	if len(cands) == 0 {
		s.Agent.Log.Debug("steer: no candidates, client left alone", "mac", mac, "ifname", ifc.ifname, "signal", c.Signal)
		return true
	}

	// Pacing and outcome tracking live in one place now. A client that has
	// declined three suggestions will not be sent a fourth for half an hour;
	// before this, a client without 11v received one every cooldown forever.
	cooldown := time.Duration(p.CooldownMS) * time.Millisecond
	if cooldown <= 0 {
		cooldown = 2 * time.Minute
	}
	assocAt := assocTime(now, c.ConnectedSeconds)
	if ok, reason := s.Agent.btm.allow(mac, ifc.ifname, assocAt, cooldown); !ok {
		if reason == "silenced" {
			s.Agent.Log.Debug("steer: suggestion withheld, client keeps declining", "mac", mac)
		}
		return true
	}

	// Capability gate. Sending a BTM to a station that does not implement 11k
	// wastes three attempts and half an hour of silence before the guard works
	// it out; asking hostapd first costs one ubus call and settles it now.
	if supports, known := s.clientSupportsBTM(ctx, ifc.ifname, mac, assocAt); known && !supports {
		s.Agent.btm.silence(mac)
		s.mu.Lock()
		delete(s.weak, mac)
		s.mu.Unlock()
		s.Agent.Log.Info("steer: client cannot parse a BTM, leaving it alone",
			"mac", mac, "ifname", ifc.ifname, "signal", c.Signal, "rx_mbps", c.RxRateMbps)
		return false
	}

	if s.sendBTM(ctx, p.BtmDisassocImminent, ifc.ifname, mac, cands) {
		s.Agent.btm.sent(mac, ifc.ifname, assocAt)
	}
	s.mu.Lock()
	delete(s.weak, mac)
	// Kept for the binding path, which shares the map: a client that was just
	// advised should not immediately be magnet-steered as well.
	s.cooldown[mac] = now.Add(cooldown)
	if p.ForceAfterMS > 0 {
		s.pending[mac] = pendingKick{ifname: ifc.ifname, deadline: now.Add(time.Duration(p.ForceAfterMS) * time.Millisecond)}
	}
	s.counts.BTM++
	s.mu.Unlock()
	// A BTM was just sent: watch closely to see whether it was taken.
	return true
}

// processBound handles a client with a magnet binding. Being on the requested
// SSID (and AP, when one was named) is success and suppresses weak-signal
// steering. Being elsewhere produces a non-imminent BTM aimed only at BSSes
// that satisfy the binding; it never force-kicks, because a client that has
// nowhere to go would just be disconnected.
func (s *SteerLoop) processBound(ctx context.Context, p *model.SteerPolicy, now time.Time, ifc managedInterface, mac string, c proto.ClientStat, b proto.ClientBinding) bool {
	if s.bindingSatisfied(b, ifc, c) {
		s.mu.Lock()
		delete(s.weak, mac)
		delete(s.pending, mac)
		delete(s.cooldown, mac)
		s.mu.Unlock()
		return false
	}

	s.mu.Lock()
	until, cooling := s.cooldown[mac]
	s.mu.Unlock()
	if cooling && now.Before(until) {
		return false
	}

	minConnected := bindingMinConnected
	if p != nil && p.MinConnectedMS > 0 {
		minConnected = time.Duration(p.MinConnectedMS) * time.Millisecond
	}
	if int64(c.ConnectedSeconds)*1000 < minConnected.Milliseconds() {
		return false
	}

	cands := s.Agent.btm.filterTargets(mac, s.bindingCandidates(b))
	if len(cands) == 0 {
		s.Agent.Log.Debug("magnet: no candidate BSS for binding",
			"mac", mac, "want_ssid", b.SSID, "want_ap", b.APMAC, "on_ssid", ifc.ssid)
		// The station is still on the wrong SSID with nowhere to go yet; keep
		// the fast cadence so a BSS that appears is used promptly.
		return true
	}
	s.sendBTM(ctx, false, ifc.ifname, mac, cands)
	s.mu.Lock()
	s.cooldown[mac] = now.Add(bindingSteerCooldown)
	delete(s.weak, mac)
	s.counts.BTM++
	s.mu.Unlock()
	return true
}

// bindingSatisfied reports whether the current association already meets the
// binding. An unknown local BSSID (netifd fallback) is treated as a match on
// the SSID/AP constraints the controller did send.
func (s *SteerLoop) bindingSatisfied(b proto.ClientBinding, ifc managedInterface, c proto.ClientStat) bool {
	if b.APMAC != "" && !strings.EqualFold(s.Agent.Identity.MAC, b.APMAC) {
		return false
	}
	ssid := ifc.ssid
	if ssid == "" {
		ssid = c.SSID
	}
	if b.SSID != "" && !strings.EqualFold(ssid, b.SSID) {
		return false
	}
	if b.BSSID != "" && ifc.bssid != "" && !strings.EqualFold(ifc.bssid, b.BSSID) {
		return false
	}
	return true
}

// bindingCandidates collects every known BSS on the bound SSID, across all
// interfaces. The per-interface merged pool is SSID-scoped (a client should
// only be offered BSSes it can join), which is exactly why a client on the
// wrong SSID needs this wider view.
func (s *SteerLoop) bindingCandidates(b proto.ClientBinding) [][]string {
	s.mu.Lock()
	merged := s.merged
	s.mu.Unlock()

	seen := map[string]bool{}
	var all [][]string
	add := func(entry []string) {
		if len(entry) < 2 || entry[0] == "" {
			return
		}
		key := strings.ToLower(entry[0])
		if seen[key] {
			return
		}
		seen[key] = true
		all = append(all, entry)
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, entry := range merged[k] {
			add(entry)
		}
	}
	for _, n := range s.Agent.lastOwnNeighbors {
		if n.BSSID != "" && n.Report != "" {
			add([]string{n.BSSID, n.SSID, n.Report})
		}
	}
	if s.Agent.peerBus != nil {
		for _, n := range s.Agent.peerBus.Snapshot().BSS {
			if n.BSSID != "" && n.Report != "" {
				add([]string{n.BSSID, n.SSID, n.Report})
			}
		}
	}

	out := make([][]string, 0, len(all))
	for _, e := range all {
		if b.BSSID != "" {
			if strings.EqualFold(e[0], b.BSSID) {
				out = append(out, e)
			}
			continue
		}
		if b.SSID != "" && strings.EqualFold(e[1], b.SSID) {
			out = append(out, e)
		}
	}
	return out
}

// force deauthenticates a client that ignored its BTM. The ban_time is what
// makes the kick productive: the AP refuses the client back onto this BSS, so
// its next association attempt lands on a neighbour.
func (s *SteerLoop) force(ctx context.Context, p *model.SteerPolicy, ifname, mac string) {
	arg, err := json.Marshal(map[string]any{
		"addr":     mac,
		"reason":   5, // disassociated because AP is unable to handle all associated STAs
		"deauth":   true,
		"ban_time": p.BanMS,
	})
	if err != nil {
		return
	}
	if _, err := s.Agent.Runner.Run(ctx, "ubus", "call", "hostapd."+ifname, "del_client", string(arg)); err != nil {
		s.Agent.Log.Warn("steer: force kick failed", "mac", mac, "ifname", ifname, "err", err)
		return
	}
	s.Agent.Log.Info("steer: client forced off after ignored BTM", "mac", mac, "ifname", ifname, "ban_ms", p.BanMS)
	s.mu.Lock()
	s.counts.Forced++
	s.mu.Unlock()
}

// sendBTM asks the client to move, listing the candidate BSSes. imminent is
// explicit because a binding suggestion must never carry the policy's
// disassociation_imminent flag.
//
// hostapd's ubus method takes neighbour report elements (the hex body produced
// by rrm_nr_get_own), not BSSIDs: a bare BSSID array is rejected with "Invalid
// argument" (verified on 10.0.30.251, 2026-11-03).
func (s *SteerLoop) sendBTM(ctx context.Context, imminent bool, ifname, mac string, cands [][]string) bool {
	neighbors := make([]string, 0, len(cands))
	bssids := make([]string, 0, len(cands))
	for _, e := range cands {
		if len(e) >= 3 && e[0] != "" && e[2] != "" {
			neighbors = append(neighbors, e[2])
			bssids = append(bssids, e[0])
		}
	}
	if len(neighbors) == 0 {
		return false
	}
	arg, err := json.Marshal(map[string]any{
		"addr":                    mac,
		"disassociation_imminent": imminent,
		"abridged":                true,
		"neighbors":               neighbors,
	})
	if err != nil {
		return false
	}
	if _, err := s.Agent.Runner.Run(ctx, "ubus", "call", "hostapd."+ifname, "bss_transition_request", string(arg)); err != nil {
		s.Agent.Log.Warn("steer: BTM request failed", "mac", mac, "ifname", ifname, "err", err)
		return false
	}
	s.Agent.Log.Info("steer: BTM sent",
		"mac", mac, "ifname", ifname, "candidates", strings.Join(bssids, ","),
		"disassociation_imminent", imminent)
	return true
}

// takePending/putPending are tiny helpers around the pending map so the force
// check stays readable under the lock.
func (s *SteerLoop) takePending(mac string) (pendingKick, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pk, ok := s.pending[mac]
	if ok {
		delete(s.pending, mac)
	}
	return pk, ok
}

func (s *SteerLoop) putPending(mac string, pk pendingKick) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[mac] = pk
}

// candidatesFor returns the merged candidate list for one interface,
// degrading to the controller's cache alone when the merge produced nothing.
// The result is filtered and ordered by rankCandidates: a client is never
// offered its own BSS, and same-channel BSSes come last.
func (s *SteerLoop) candidatesFor(ifc managedInterface) [][]string {
	s.mu.Lock()
	merged := s.merged
	s.mu.Unlock()
	if list, ok := merged[ifc.ifname]; ok && len(list) > 0 {
		return rankCandidates(list, ifc.info)
	}
	if cached := s.Agent.Applier.cachedNeighbors(); cached != nil {
		if m, err := decodeNeighbors(cached); err == nil {
			return rankCandidates(m[ifc.ifname], ifc.info)
		}
	}
	return nil
}

// trimMaps keeps the per-client maps bounded on a busy AP.
func (s *SteerLoop) trimMaps(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cooldown) > steerMapLimit {
		for mac, until := range s.cooldown {
			if now.After(until) {
				delete(s.cooldown, mac)
			}
		}
	}
	if len(s.weak) > steerMapLimit {
		s.weak = map[string]int{}
	}
}
