package agent

import (
	"context"
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

// PeerBus is the AP-to-AP coordination channel: one UDP socket per agent that
// announces this AP's 802.11k self-reports and clients, and collects the same
// from peers on the management LAN.
//
// It deliberately does not depend on the controller. The controller remains the
// source of truth when it is reachable, but roaming data has to survive an AC
// outage, a factory-fresh AP joining during maintenance, and hostapd forgetting
// its neighbour lists after a reload.
//
// Cost: one small broadcast per interval (default 30 s) and one socket, plus a
// bounded in-memory peer table. No extra processes, no disk writes.
type PeerBus struct {
	Port     int
	Interval time.Duration
	Log      *slog.Logger

	mu           sync.Mutex
	conn         *net.UDPConn
	peers        map[string]peerEntry
	selfMAC      string
	lastAnnounce time.Time
	// lastSig is the BSS signature of the previous snapshot, and version counts
	// how often it changed. Only the BSS set matters: a peer's client list moves
	// constantly and drives no local action, while an appearing or disappearing
	// BSS changes every 11k list this AP advertises.
	lastSig string
	version uint64
}

type peerEntry struct {
	state proto.PeerState
	seen  time.Time
}

// PeerSnapshot is one consistent read of the live peer table.
type PeerSnapshot struct {
	// BSS entries are every BSS advertised by a live peer, in AP MAC order.
	BSS []proto.NeighborBSS
	// APs is the set of live peer AP MACs.
	APs []string
}

// NewPeerBus prepares a bus; Start binds the socket.
func NewPeerBus(port int, interval time.Duration, selfMAC string, log *slog.Logger) *PeerBus {
	if interval <= 0 {
		interval = defaultPeerInterval
	}
	return &PeerBus{
		Port:     port,
		Interval: interval,
		Log:      log,
		peers:    map[string]peerEntry{},
		selfMAC:  normalizePeerMAC(selfMAC),
	}
}

// Start binds the UDP port and begins collecting announcements. A port already
// in use is reported so the agent can continue without peer coordination rather
// than fail the provisioning loop.
func (b *PeerBus) Start(ctx context.Context) error {
	if b == nil || b.Port <= 0 {
		return nil
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: b.Port})
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.conn = conn
	b.mu.Unlock()

	go b.readLoop(ctx, conn)
	return nil
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
	buf := make([]byte, 64<<10)
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

// ingest validates and stores one announcement. Malformed messages are dropped
// silently: a peer's bug must not spam the log, and the next announcement fixes
// whatever was missed.
func (b *PeerBus) ingest(body []byte) {
	if len(body) == 0 || len(body) > peerMaxPayload {
		return
	}
	var st proto.PeerState
	if err := json.Unmarshal(body, &st); err != nil {
		return
	}
	if st.Type != proto.MsgPeer || st.V > proto.PeerVersion {
		return
	}
	mac := normalizePeerMAC(st.MAC)
	if mac == "" || mac == b.selfMAC {
		return
	}
	st.MAC = mac

	b.mu.Lock()
	b.peers[mac] = peerEntry{state: st, seen: time.Now()}
	b.mu.Unlock()
}

// Announce publishes this AP's state, throttled to Interval. It is safe to call
// from any cycle path, including the two extra hellos sent after an apply.
func (b *PeerBus) Announce(state proto.PeerState) {
	if b == nil {
		return
	}
	b.mu.Lock()
	conn := b.conn
	if conn == nil || time.Since(b.lastAnnounce) < b.Interval {
		b.mu.Unlock()
		return
	}
	b.lastAnnounce = time.Now()
	b.mu.Unlock()

	body, err := marshalPeerAnnouncement(state)
	if err != nil {
		b.Log.Warn("peer announcement dropped", "err", err)
		return
	}
	for _, target := range broadcastTargets(b.Port) {
		if _, err := conn.WriteToUDP(body, target); err != nil {
			b.Log.Debug("peer announce failed", "target", target.String(), "err", err)
		}
	}
}

// marshalPeerAnnouncement renders what goes on the wire.
//
// A very busy AP can push the client list past the bound. BSS reports are the
// part peers cannot work without, so the load view is dropped first; anything
// still over the limit means the payload is malformed or hostile.
func marshalPeerAnnouncement(state proto.PeerState) ([]byte, error) {
	state.Type = proto.MsgPeer
	state.V = proto.PeerVersion
	body, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	if len(body) <= peerMaxPayload {
		return body, nil
	}
	state.Clients = nil
	body, err = json.Marshal(state)
	if err != nil {
		return nil, err
	}
	if len(body) > peerMaxPayload {
		return nil, fmt.Errorf("announcement is %d bytes, limit %d", len(body), peerMaxPayload)
	}
	return body, nil
}

// Snapshot returns the live peers after pruning entries that stopped announcing.
func (b *PeerBus) Snapshot() PeerSnapshot {
	if b == nil {
		return PeerSnapshot{}
	}
	now := time.Now()

	b.mu.Lock()
	defer b.mu.Unlock()
	// Computed under the lock: the interval is now adjustable (it tightens when
	// the controller is lost), and the expiry is derived from it.
	expiry := b.expiry()
	for mac, entry := range b.peers {
		if now.Sub(entry.seen) > expiry {
			delete(b.peers, mac)
		}
	}

	apMACs := make([]string, 0, len(b.peers))
	for mac := range b.peers {
		apMACs = append(apMACs, mac)
	}
	sort.Strings(apMACs)

	var bss []proto.NeighborBSS
	for _, mac := range apMACs {
		for _, entry := range b.peers[mac].state.BSS {
			if entry.BSSID == "" || entry.Report == "" {
				continue
			}
			bss = append(bss, entry)
		}
	}

	// Snapshot is the single place where the live BSS set is decided (it also
	// prunes expired peers), so it is also the right place to notice a change.
	if sig := bssSignature(bss); sig != b.lastSig {
		b.lastSig = sig
		b.version++
	}
	return PeerSnapshot{BSS: bss, APs: apMACs}
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
// twice under different spellings.
func normalizePeerMAC(s string) string {
	raw := strings.ToLower(strings.TrimSpace(s))
	if raw == "" {
		return ""
	}
	if _, err := net.ParseMAC(raw); err != nil {
		return ""
	}
	return raw
}

const (
	// defaultPeerInterval is the announcement cadence. It matches the heartbeat
	// interval closely enough that a peer table never goes stale for long, while
	// keeping the broadcast at two packets per minute.
	defaultPeerInterval = 30 * time.Second
	// defaultPeerIntervalOffline is the cadence used while the controller is
	// known to be gone. Peer coordination is then the only thing keeping 11k and
	// client awareness current across the site, so it converges faster; the cost
	// is a few KB per minute of broadcast on the management LAN.
	defaultPeerIntervalOffline = 20 * time.Second
	// peerMinExpiry keeps a peer alive through one missed announcement even when
	// the interval is configured very short.
	peerMinExpiry = 90 * time.Second
	// peerReadTimeout lets the read loop observe context cancellation without a
	// separate wake-up goroutine.
	peerReadTimeout = 5 * time.Second
	// peerMaxPayload bounds both directions. 18 BSS reports are a few KB;
	// anything near this is malformed or hostile.
	peerMaxPayload = 48 << 10
)
