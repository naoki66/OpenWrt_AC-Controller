package agent

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/naoki66/ac-controller/internal/proto"
)

// PeerBus is the AP-to-AP coordination channel: one socket per agent that
// announces this AP's 802.11k self-reports and load state, and collects the
// same from peers on the management LAN.
//
// It deliberately does not depend on the controller. The controller remains the
// source of truth when it is reachable, but roaming data has to survive an AC
// outage, a factory-fresh AP joining during maintenance, and hostapd forgetting
// its neighbour lists after a reload.
//
// Transport is IPv6 link-local multicast bound to the management interface,
// with an automatic IPv4 broadcast fallback for a LAN where IPv6 is disabled.
// Messages are split into a small neighbour-report frame and an optional
// client/radio frame, so the part peers cannot work without stays below the
// IPv6 minimum MTU and is never fragmented.
//
// A configured peer secret adds HMAC-SHA256 authentication, a boot id, a
// timestamp and a counter. The receiver rejects stale timestamps, replays and
// messages whose signature does not match. With no secret the bus remains on
// the old trusted-L2 assumption and logs a warning at start.
type PeerBus struct {
	Port     int
	Interval time.Duration
	Log      *slog.Logger
	// Secret is the shared site key. Empty disables authentication.
	Secret string
	// Network is the logical OpenWrt network carrying AP-to-AP traffic, not a
	// device name. It is resolved to l3_device before the IPv6 socket is bound.
	Network string
	// Runner is used to resolve the logical network through ubus. Nil means
	// "skip IPv6 and use the IPv4 fallback".
	Runner Runner

	mu           sync.Mutex
	conn         *net.UDPConn
	transport    string
	device       string
	group        net.IP
	peers        map[string]peerEntry
	replay       map[string]*peerReplay
	parts        map[string]*nrAssembly
	selfMAC      string
	boot         string
	counter      uint64
	nrGen        uint64
	lastAnnounce time.Time
	// lastSig is the BSS signature of the previous snapshot, and version counts
	// how often it changed. Only the BSS set matters: a peer's client list moves
	// constantly and drives no local action, while an appearing or disappearing
	// BSS changes every 11k list this AP advertises.
	lastSig string
	version uint64
	now     func() time.Time
}

type peerEntry struct {
	state     proto.PeerState
	seen      time.Time
	lastNR    time.Time
	lastState time.Time
}

// nrAssembly is the partial state of one split neighbour-report generation.
type nrAssembly struct {
	gen      uint64
	parts    map[int][]proto.NeighborBSS
	expected int
	updated  time.Time
}

// peerReplay is the per-peer anti-replay window. A new boot id resets it.
type peerReplay struct {
	boot string
	max  uint64
	seen map[uint64]struct{}
}

// PeerSnapshot is one consistent read of the live peer table.
type PeerSnapshot struct {
	// BSS entries are every BSS advertised by a live peer, in AP MAC order.
	BSS []proto.NeighborBSS
	// APs is the set of live peer AP MACs.
	APs []string
	// NTP is the first non-empty time-source list announced by a live peer. It
	// lets an AP that missed the controller reply still adopt the site list.
	NTP []string
}

// PeerOptions configures a bus. Zero values keep the historical defaults so
// existing tests and callers can use NewPeerBus unchanged.
type PeerOptions struct {
	Port     int
	Interval time.Duration
	SelfMAC  string
	Log      *slog.Logger
	Secret   string
	Network  string
	Runner   Runner
}

// NewPeerBus prepares a bus with the historical arguments.
func NewPeerBus(port int, interval time.Duration, selfMAC string, log *slog.Logger) *PeerBus {
	return NewPeerBusWithOptions(PeerOptions{
		Port: port, Interval: interval, SelfMAC: selfMAC, Log: log,
	})
}

// NewPeerBusWithOptions prepares a bus; Start binds the socket.
func NewPeerBusWithOptions(opt PeerOptions) *PeerBus {
	if opt.Interval <= 0 {
		opt.Interval = defaultPeerInterval
	}
	return &PeerBus{
		Port:     opt.Port,
		Interval: opt.Interval,
		Log:      opt.Log,
		Secret:   opt.Secret,
		Network:  opt.Network,
		Runner:   opt.Runner,
		peers:    map[string]peerEntry{},
		replay:   map[string]*peerReplay{},
		parts:    map[string]*nrAssembly{},
		selfMAC:  normalizePeerMAC(opt.SelfMAC),
		boot:     peerBootID(),
		now:      time.Now,
	}
}

// Start binds the peer socket and begins collecting announcements. IPv6
// link-local multicast is preferred; if the management network has no usable
// IPv6 interface, the bus falls back to the original IPv4 broadcast. A port
// already in use is reported so the agent can continue without peer
// coordination rather than fail the provisioning loop.
func (b *PeerBus) Start(ctx context.Context) error {
	if b == nil || b.Port <= 0 {
		return nil
	}
	if b.Secret == "" && b.Log != nil {
		b.Log.Warn("peer authentication disabled; set peer_secret on an untrusted segment")
	}

	if device := b.resolveDevice(ctx); device != "" {
		conn, err := listenPeerMulticast(b.Port, device, proto.PeerGroup)
		if err == nil {
			group := net.ParseIP(proto.PeerGroup)
			b.mu.Lock()
			b.conn = conn
			b.transport = "ipv6"
			b.device = device
			b.group = group
			b.mu.Unlock()
			if b.Log != nil {
				b.Log.Info("peer coordination listening", "transport", "ipv6-multicast",
					"device", device, "group", proto.PeerGroup, "port", b.Port)
			}
			go b.readLoop(ctx, conn)
			return nil
		}
		if b.Log != nil {
			b.Log.Warn("IPv6 peer multicast unavailable, falling back to IPv4 broadcast",
				"device", device, "err", err)
		}
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: b.Port})
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.conn = conn
	b.transport = "ipv4"
	b.group = nil
	b.mu.Unlock()
	if b.Log != nil {
		b.Log.Info("peer coordination listening", "transport", "ipv4-broadcast", "port", b.Port)
	}
	go b.readLoop(ctx, conn)
	return nil
}

// resolveDevice maps peer_network (e.g. "lan") to its l3_device. A bare device
// name is accepted too, which keeps the option usable on a hand-built image.
func (b *PeerBus) resolveDevice(ctx context.Context) string {
	if b == nil || b.Network == "" || b.Runner == nil {
		return ""
	}
	out, err := b.Runner.Run(ctx, "ubus", "call", "network.interface."+b.Network, "status")
	if err == nil {
		var st struct {
			Up       bool   `json:"up"`
			L3Device string `json:"l3_device"`
		}
		if json.Unmarshal([]byte(out), &st) == nil && st.Up && st.L3Device != "" {
			return st.L3Device
		}
	}
	if ifi, err := net.InterfaceByName(b.Network); err == nil {
		return ifi.Name
	}
	return ""
}

// Stop closes the socket. The read loop then returns.
func (b *PeerBus) Stop() {
	if b == nil {
		return
	}
	b.mu.Lock()
	conn := b.conn
	b.conn = nil
	b.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (b *PeerBus) readLoop(ctx context.Context, conn *net.UDPConn) {
	buf := make([]byte, proto.PeerMaxPacket)
	for {
		if ctx.Err() != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(peerReadTimeout))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			// A closed socket is the normal shutdown path.
			return
		}
		b.ingest(buf[:n])
	}
}

// ingest verifies, validates and stores one announcement. Malformed messages
// are dropped silently: a peer's bug must not spam the log, and the next
// announcement fixes whatever was missed.
func (b *PeerBus) ingest(body []byte) {
	if len(body) == 0 || len(body) > proto.PeerMaxPacket {
		return
	}
	var st proto.PeerState
	if err := json.Unmarshal(body, &st); err != nil {
		return
	}
	if !peerTypeAllowed(st.Type) {
		return
	}
	// Version 2 is the signed/split protocol. Version 1 is the legacy single
	// announcement; it is accepted only while no peer secret is configured.
	if st.V == proto.PeerVersionLegacy {
		if b.Secret != "" || st.Type != proto.MsgPeer {
			return
		}
	} else if st.V != proto.PeerVersion {
		return
	}
	mac := normalizePeerMAC(st.MAC)
	if mac == "" || mac == b.selfMAC {
		return
	}
	st.MAC = mac

	if st.V == proto.PeerVersion && b.Secret != "" {
		if !peerVerify(&st, b.Secret) {
			return
		}
	}
	// Validate before storing, so a malformed report never reaches hostapd.
	st.BSS = validatePeerBSS(st.BSS)

	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.acceptReplayLocked(mac, st, now) {
		return
	}
	switch st.Type {
	case proto.MsgPeerNR:
		b.applyNR(mac, &st, now)
	case proto.MsgPeerState:
		b.applyState(mac, &st, now)
	case proto.MsgPeer:
		b.applyLegacy(mac, &st, now)
	}
}

func peerTypeAllowed(t string) bool {
	return t == proto.MsgPeer || t == proto.MsgPeerNR || t == proto.MsgPeerState
}

// acceptReplayLocked enforces the freshness/counter window. Legacy messages
// (and unit-test fixtures without the new fields) skip it; a v2 sender always
// fills them.
func (b *PeerBus) acceptReplayLocked(mac string, st proto.PeerState, now time.Time) bool {
	if st.Boot == "" || st.Counter == 0 {
		return true
	}
	if st.TS > 0 {
		delta := now.Unix() - st.TS
		if delta < 0 {
			delta = -delta
		}
		if delta > int64(peerFreshness/time.Second) {
			return false
		}
	}
	r := b.replay[mac]
	if r == nil {
		r = &peerReplay{seen: map[uint64]struct{}{}}
		b.replay[mac] = r
	}
	if r.boot != st.Boot {
		r.boot = st.Boot
		r.max = st.Counter
		r.seen = map[uint64]struct{}{st.Counter: {}}
		return true
	}
	if st.Counter+peerReplayWindow <= r.max {
		return false
	}
	if _, ok := r.seen[st.Counter]; ok {
		return false
	}
	r.seen[st.Counter] = struct{}{}
	if st.Counter > r.max {
		r.max = st.Counter
	}
	for c := range r.seen {
		if c+peerReplayWindow <= r.max {
			delete(r.seen, c)
		}
	}
	return true
}

func (b *PeerBus) applyNR(mac string, st *proto.PeerState, now time.Time) {
	e, ok := b.peers[mac]
	if !ok && len(b.peers) >= proto.PeerMaxPeers {
		return
	}
	if !ok {
		e = peerEntry{state: proto.PeerState{MAC: mac}}
	}
	e.state.MAC = mac
	e.state.Model = st.Model
	e.state.Hostname = st.Hostname
	e.seen = now
	e.lastNR = now

	if st.Parts <= 1 {
		e.state.BSS = st.BSS
		delete(b.parts, mac)
		b.peers[mac] = e
		return
	}
	if st.Part < 1 || st.Part > st.Parts {
		return
	}
	asm := b.parts[mac]
	if asm == nil || asm.gen != st.Gen {
		asm = &nrAssembly{gen: st.Gen, parts: map[int][]proto.NeighborBSS{}, expected: st.Parts}
		b.parts[mac] = asm
	}
	asm.parts[st.Part] = st.BSS
	asm.updated = now
	if len(asm.parts) >= asm.expected {
		var all []proto.NeighborBSS
		for i := 1; i <= asm.expected; i++ {
			all = append(all, asm.parts[i]...)
		}
		if len(all) > proto.PeerMaxReports {
			all = all[:proto.PeerMaxReports]
		}
		e.state.BSS = all
		delete(b.parts, mac)
	}
	b.peers[mac] = e
}

func (b *PeerBus) applyState(mac string, st *proto.PeerState, now time.Time) {
	e, ok := b.peers[mac]
	if !ok && len(b.peers) >= proto.PeerMaxPeers {
		return
	}
	if !ok {
		e = peerEntry{state: proto.PeerState{MAC: mac}}
	}
	e.state.MAC = mac
	e.state.Model = st.Model
	e.state.Hostname = st.Hostname
	e.state.Clients = st.Clients
	e.state.Radios = st.Radios
	e.state.NTP = append([]string(nil), st.NTP...)
	e.seen = now
	e.lastState = now
	b.peers[mac] = e
}

func (b *PeerBus) applyLegacy(mac string, st *proto.PeerState, now time.Time) {
	e, ok := b.peers[mac]
	if !ok && len(b.peers) >= proto.PeerMaxPeers {
		return
	}
	if !ok {
		e = peerEntry{state: proto.PeerState{MAC: mac}}
	}
	st.MAC = mac
	st.Type = proto.MsgPeer
	e.state = *st
	e.seen = now
	e.lastNR = now
	e.lastState = now
	delete(b.parts, mac)
	b.peers[mac] = e
}

// Announce publishes this AP's state, throttled to Interval. It is safe to call
// from any cycle path, including the two extra hellos sent after an apply.
func (b *PeerBus) Announce(state proto.PeerState) {
	if b == nil {
		return
	}
	now := b.now()
	b.mu.Lock()
	conn := b.conn
	if conn == nil || now.Sub(b.lastAnnounce) < b.Interval {
		b.mu.Unlock()
		return
	}
	b.lastAnnounce = now
	b.nrGen++
	gen := b.nrGen
	transport := b.transport
	group := b.group
	boot := b.boot
	b.mu.Unlock()

	state.Boot = boot
	state.TS = now.Unix()
	state.V = proto.PeerVersion
	b.sendNR(conn, transport, group, state, gen)
	b.sendState(conn, transport, group, state)
}

// sendNR splits the neighbour reports into frames that fit the IPv6 minimum
// MTU. A busy AP with many BSSes then sends a few small frames instead of one
// fragmented datagram, and the receiver reassembles by Gen/Part/Parts.
func (b *PeerBus) sendNR(conn *net.UDPConn, transport string, group net.IP, base proto.PeerState, gen uint64) {
	reports := base.BSS
	if len(reports) > proto.PeerMaxReports {
		reports = reports[:proto.PeerMaxReports]
	}
	base.Type = proto.MsgPeerNR
	base.BSS = nil

	if len(reports) == 0 {
		msg := base
		msg.Gen = gen
		msg.Part = 1
		msg.Parts = 1
		b.sendOne(conn, transport, group, &msg, b.nextCounter())
		return
	}

	chunks := b.nrChunks(base, gen, reports)
	for i, chunk := range chunks {
		msg := base
		msg.Gen = gen
		msg.Part = i + 1
		msg.Parts = len(chunks)
		msg.BSS = chunk
		b.sendOne(conn, transport, group, &msg, b.nextCounter())
	}
}

// nrChunks splits reports so every frame stays inside the MTU budget. A report
// that cannot fit on its own is dropped with a warning; the rest still go out.
func (b *PeerBus) nrChunks(base proto.PeerState, gen uint64, reports []proto.NeighborBSS) [][]proto.NeighborBSS {
	var chunks [][]proto.NeighborBSS
	cur := make([]proto.NeighborBSS, 0, len(reports))
	for _, r := range reports {
		candidate := append(cur, r)
		if len(cur) > 0 && b.nrFrameTooBig(base, gen, candidate) {
			chunks = append(chunks, cur)
			cur = nil
			candidate = []proto.NeighborBSS{r}
		}
		if len(candidate) == 0 {
			continue
		}
		if b.nrFrameTooBig(base, gen, candidate) {
			if b.Log != nil {
				b.Log.Warn("peer report dropped, it does not fit one frame", "bssid", r.BSSID)
			}
			continue
		}
		cur = append(cur, r)
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

func (b *PeerBus) nextCounter() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.counter++
	return b.counter
}

func (b *PeerBus) nrFrameTooBig(base proto.PeerState, gen uint64, bss []proto.NeighborBSS) bool {
	probe := base
	probe.Type = proto.MsgPeerNR
	probe.Gen = gen
	probe.Part = 1
	probe.Parts = 99
	probe.Counter = 999999
	probe.TS = b.now().Unix()
	probe.BSS = bss
	peerSeal(&probe, b.Secret)
	body, err := json.Marshal(probe)
	return err != nil || len(body) > proto.PeerMaxSend-peerFrameMargin
}

// sendState carries clients and radios. It is optional: if the frame cannot fit
// even after dropping the client list, the radios are dropped too rather than
// fragmenting. Neighbour reports are never lost because of this path.
func (b *PeerBus) sendState(conn *net.UDPConn, transport string, group net.IP, base proto.PeerState) {
	if len(base.Clients) == 0 && len(base.Radios) == 0 && len(base.NTP) == 0 {
		return
	}
	msg := base
	msg.Type = proto.MsgPeerState
	msg.BSS = nil
	msg.Gen = 0
	msg.Part = 0
	msg.Parts = 0
	msg.Counter = 999999
	msg.TS = b.now().Unix()
	peerSeal(&msg, b.Secret)
	body, err := json.Marshal(msg)
	if err != nil {
		return
	}
	if len(body) > proto.PeerMaxSend-peerFrameMargin && len(msg.Clients) > 0 {
		msg.Clients = nil
		peerSeal(&msg, b.Secret)
		body, err = json.Marshal(msg)
		if err != nil {
			return
		}
	}
	if len(body) > proto.PeerMaxSend-peerFrameMargin {
		if b.Log != nil {
			b.Log.Debug("peer state message dropped, it does not fit one frame")
		}
		return
	}
	b.sendOne(conn, transport, group, &msg, b.nextCounter())
}

// sendOne seals the message with a fresh counter and writes it on the selected
// transport.
func (b *PeerBus) sendOne(conn *net.UDPConn, transport string, group net.IP, msg *proto.PeerState, counter uint64) {
	msg.V = proto.PeerVersion
	msg.Counter = counter
	msg.TS = b.now().Unix()
	peerSeal(msg, b.Secret)
	body, err := json.Marshal(msg)
	if err != nil {
		return
	}
	if len(body) > proto.PeerMaxSend {
		if b.Log != nil {
			b.Log.Warn("peer frame dropped, it exceeds the MTU budget", "bytes", len(body))
		}
		return
	}
	if transport == "ipv6" && group != nil {
		if _, err := conn.WriteToUDP(body, &net.UDPAddr{IP: group, Port: b.Port}); err != nil {
			if b.Log != nil {
				b.Log.Debug("peer multicast send failed", "err", err)
			}
		}
		return
	}
	for _, target := range broadcastTargets(b.Port) {
		if _, err := conn.WriteToUDP(body, target); err != nil {
			if b.Log != nil {
				b.Log.Debug("peer announce failed", "target", target.String(), "err", err)
			}
		}
	}
}

// peerSeal fills Sig with HMAC-SHA256 over the message rendered with an empty
// Sig. Go marshals struct fields in declaration order, so the canonical form is
// stable between all agents of this version.
func peerSeal(st *proto.PeerState, secret string) {
	st.Sig = ""
	if secret == "" {
		return
	}
	mac := hmac.New(sha256.New, []byte(secret))
	body, err := json.Marshal(st)
	if err != nil {
		return
	}
	mac.Write(body)
	st.Sig = hex.EncodeToString(mac.Sum(nil))
}

// peerVerify recomputes the HMAC and compares in constant time.
func peerVerify(st *proto.PeerState, secret string) bool {
	if secret == "" {
		return true
	}
	sig := strings.ToLower(st.Sig)
	if sig == "" {
		return false
	}
	st.Sig = ""
	body, err := json.Marshal(st)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal([]byte(sig), []byte(hex.EncodeToString(mac.Sum(nil))))
}

// validatePeerBSS enforces the uneighbord-style report rules: a real 6-byte
// BSSID, a 1..32 byte SSID, a 26..1024 hex-character neighbour report, and a
// report whose first six bytes are the BSSID it claims. Entries are deduplicated
// by BSSID and capped so a peer cannot flood the local list.
func validatePeerBSS(list []proto.NeighborBSS) []proto.NeighborBSS {
	if len(list) == 0 {
		return nil
	}
	out := make([]proto.NeighborBSS, 0, min(len(list), proto.PeerMaxReports))
	seen := map[string]bool{}
	for _, e := range list {
		bssid, rawBSSID, ok := normalizeBSSID(e.BSSID)
		if !ok {
			continue
		}
		ssid := e.SSID
		if len(ssid) == 0 || len([]byte(ssid)) > proto.PeerMaxSSID {
			continue
		}
		report := strings.ToLower(strings.TrimSpace(e.Report))
		if len(report) < 26 || len(report) > proto.PeerMaxNRHex || len(report)%2 != 0 {
			continue
		}
		raw, err := hex.DecodeString(report)
		if err != nil || len(raw) < 13 {
			continue
		}
		match := true
		for i := 0; i < 6; i++ {
			if raw[i] != rawBSSID[i] {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		key := strings.ToLower(bssid)
		if seen[key] {
			continue
		}
		seen[key] = true
		e.BSSID = bssid
		e.SSID = ssid
		e.Report = report
		out = append(out, e)
		if len(out) >= proto.PeerMaxReports {
			break
		}
	}
	return out
}

// Snapshot returns the live peers after pruning entries that stopped announcing.
func (b *PeerBus) Snapshot() PeerSnapshot {
	if b == nil {
		return PeerSnapshot{}
	}
	now := b.now()

	b.mu.Lock()
	defer b.mu.Unlock()
	// Computed under the lock: the interval is now adjustable (it tightens when
	// the controller is lost), and the expiry is derived from it.
	expiry := b.expiry()
	for mac, entry := range b.peers {
		if now.Sub(entry.seen) > expiry {
			delete(b.peers, mac)
			delete(b.replay, mac)
			delete(b.parts, mac)
		}
	}
	for mac, asm := range b.parts {
		if now.Sub(asm.updated) > expiry {
			delete(b.parts, mac)
		}
	}

	apMACs := make([]string, 0, len(b.peers))
	for mac := range b.peers {
		apMACs = append(apMACs, mac)
	}
	sort.Strings(apMACs)

	var bss []proto.NeighborBSS
	var ntp []string
	for _, mac := range apMACs {
		for _, entry := range b.peers[mac].state.BSS {
			if entry.BSSID == "" || entry.Report == "" {
				continue
			}
			bss = append(bss, entry)
		}
		if len(ntp) == 0 && len(b.peers[mac].state.NTP) > 0 {
			ntp = append([]string(nil), b.peers[mac].state.NTP...)
		}
	}

	// Snapshot is the single place where the live BSS set is decided (it also
	// prunes expired peers), so it is also the right place to notice a change.
	if sig := bssSignature(bss); sig != b.lastSig {
		b.lastSig = sig
		b.version++
	}
	return PeerSnapshot{BSS: bss, APs: apMACs, NTP: ntp}
}

// Version counts changes to the live peer BSS set. The agent uses it to reassert
// its 802.11k lists as soon as a peer appears or disappears, instead of waiting
// for the next periodic refresh — five minutes at the default cadence.
func (b *PeerBus) Version() uint64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.version
}

// bssSignature renders a peer's BSS set in a form that is stable across
// announcements: same entries in any order compare equal.
func bssSignature(bss []proto.NeighborBSS) string {
	parts := make([]string, 0, len(bss))
	for _, e := range bss {
		parts = append(parts, strings.ToLower(e.BSSID)+"|"+e.SSID+"|"+e.Report)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

// SetInterval changes the announcement cadence. Safe to call on every cycle: it
// is a no-op unless the value actually changed. The caller must hold no locks;
// Announce and Snapshot read the interval under mu.
func (b *PeerBus) SetInterval(d time.Duration) {
	if b == nil || d <= 0 {
		return
	}
	b.mu.Lock()
	changed := b.Interval != d
	b.Interval = d
	b.mu.Unlock()
	if changed && b.Log != nil {
		b.Log.Info("peer announcement cadence changed", "interval", d.String())
	}
}

func (b *PeerBus) expiry() time.Duration {
	d := 3 * b.Interval
	if d < peerMinExpiry {
		d = peerMinExpiry
	}
	return d
}

// normalizePeerMAC lower-cases and validates a MAC so the same AP cannot appear
// twice under different spellings. A peer identity must be a 6-byte address.
func normalizePeerMAC(s string) string {
	raw := strings.ToLower(strings.TrimSpace(s))
	if raw == "" {
		return ""
	}
	hw, err := net.ParseMAC(raw)
	if err != nil || len(hw) != 6 {
		return ""
	}
	return hw.String()
}

// normalizeBSSID returns the canonical BSSID and its bytes for the report
// consistency check.
func normalizeBSSID(s string) (string, [6]byte, bool) {
	var out [6]byte
	hw, err := net.ParseMAC(strings.ToLower(strings.TrimSpace(s)))
	if err != nil || len(hw) != 6 {
		return "", out, false
	}
	copy(out[:], hw)
	return hw.String(), out, true
}

func peerBootID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return hex.EncodeToString(raw[:])
	}
	return fmt.Sprintf("t%x", time.Now().UnixNano())
}

const (
	// defaultPeerInterval is the announcement cadence. It matches the heartbeat
	// interval closely enough that a peer table never goes stale for long, while
	// keeping the broadcast at two packets per minute.
	defaultPeerInterval = 30 * time.Second
	// defaultPeerIntervalOffline is the cadence used while the controller is
	// known to be gone. Peer coordination is then the only thing keeping 11k and
	// client awareness current across the site, so it converges faster; the cost
	// is a few KB per minute on the management LAN.
	defaultPeerIntervalOffline = 20 * time.Second
	// peerMinExpiry keeps a peer alive through one missed announcement even when
	// the interval is configured very short.
	peerMinExpiry = 90 * time.Second
	// peerReadTimeout lets the read loop observe context cancellation without a
	// separate wake-up goroutine.
	peerReadTimeout = 5 * time.Second
	// peerFreshness bounds how far a message timestamp may deviate from the
	// receiver's clock. OpenWrt can step the wall clock forward by 12-22 minutes
	// at boot before NTP settles; one hour keeps a freshly booted AP able to
	// accept peers long enough to learn the site NTP list. Replay protection
	// still comes from the boot id and counter.
	peerFreshness = time.Hour
	// peerReplayWindow is how many counters behind the highest accepted one a
	// message may still arrive. It absorbs UDP reordering across split frames.
	peerReplayWindow uint64 = 64
	// peerFrameMargin reserves room for a counter that grew a few digits between
	// the size probe and the final frame.
	peerFrameMargin = 32
)
