package proto

// Peer coordination between AP agents (no controller on the path).
//
// The controller already aggregates BSS identities in the normal case. This
// message exists for the case the design has to survive: the controller is
// unreachable, an AP was just added, or a radio lost its neighbour list to a
// reload. Agents then exchange the same 802.11k self-reports directly on the
// management LAN, so roaming keeps working without the AC.
//
// Version 2 split the old single announcement into two messages: neighbour
// reports (AP_PEER_NR) and load/client state (AP_PEER_STATE). Keeping the NR
// message small enough for the IPv6 minimum MTU avoids fragmentation, while the
// optional state message can be dropped without losing the data peers cannot
// work without. Both messages carry a boot id, a timestamp, a counter and an
// optional HMAC so a receiver can reject replays and injectors.

// Message types. MsgPeer is the legacy v1 announcement, still accepted when no
// shared secret is configured so a rolling upgrade keeps seeing old peers.
const (
	MsgPeer      = "AP_PEER"
	MsgPeerNR    = "AP_PEER_NR"
	MsgPeerState = "AP_PEER_STATE"
)

// PeerPort is the default UDP port agents listen on. It is deliberately one
// above DiscoverPort so the two listeners never collide.
const PeerPort = 29821

// PeerGroup is a link-local, transient IPv6 multicast group. ff12 keeps the
// packets on the local segment; the group id spells "ac 11k".
const PeerGroup = "ff12::ac11:6b"

// PeerVersion is the wire version of the v2 messages. PeerVersionLegacy is the
// pre-HMAC version, accepted only while no peer secret is configured.
const (
	PeerVersion       = 2
	PeerVersionLegacy = 1
)

// Wire limits. They follow the same shape as uneighbord: a neighbour report
// body is 26..1024 hex characters and its first six bytes are the BSSID; a
// sender never puts more than 1232 bytes in one frame (IPv6 minimum MTU minus
// headers), and a receiver bounds the number of peers and reports it keeps.
const (
	PeerMaxSend    = 1232
	PeerMaxPacket  = 8192
	PeerMaxPeers   = 64
	PeerMaxReports = 64
	PeerMaxNRHex   = 1024
	PeerMaxSSID    = 32
)

// PeerState is one AP's announcement. A neighbour-report message carries BSS
// (optionally split into Part/Parts with a shared Gen); a state message carries
// Clients and Radios. Model and Hostname are for operator-visible debugging.
type PeerState struct {
	Type string `json:"type"`
	V    int    `json:"v"`
	MAC  string `json:"mac"`
	// Boot is a random per-process id. A receiver resets its replay state when
	// the id changes, which is how a rebooted AP is told apart from a replay.
	Boot string `json:"boot,omitempty"`
	// TS is Unix seconds. A receiver rejects messages outside its freshness
	// window, so an old capture cannot be replayed after the window.
	TS int64 `json:"ts,omitempty"`
	// Counter increases with every sent message. Repeated or too-old counters
	// are rejected; the boot id makes a reset unambiguous.
	Counter uint64 `json:"ctr,omitempty"`
	// Gen identifies one complete BSS set when a report is split across parts.
	Gen   uint64 `json:"gen,omitempty"`
	Part  int    `json:"part,omitempty"`
	Parts int    `json:"parts,omitempty"`
	// Sig is hex(HMAC-SHA256(secret, message with Sig empty)). Empty means an
	// unsigned legacy message; a receiver with a configured secret rejects it.
	Sig string `json:"sig,omitempty"`

	Model    string `json:"model,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	// BSS is this AP's own 802.11k report, one entry per BSS. Peer agents copy
	// these verbatim into rrm_nr_set, so the controller never has to interpret
	// operating classes.
	BSS []NeighborBSS `json:"bss,omitempty"`
	// Clients is the associated-station set this AP currently serves. It is not
	// used to compute another AP's neighbour list; it exists so a peer can see
	// whether a candidate radio is carrying load.
	Clients []PeerClient `json:"clients,omitempty"`
	// Radios is the operating point of each radio, with airtime utilisation.
	Radios []PeerRadio `json:"radios,omitempty"`
	// NTP is the time-source list this AP has applied. A peer that joined
	// during a controller outage can adopt it, so time sync survives when the
	// AP was never able to receive the hello reply.
	NTP []string `json:"ntp,omitempty"`
}

// PeerClient is one station as a peer AP sees it.
type PeerClient struct {
	MAC              string `json:"mac"`
	Ifname           string `json:"ifname,omitempty"`
	SSID             string `json:"ssid,omitempty"`
	Band             string `json:"band,omitempty"`
	Signal           int    `json:"signal_dbm"`
	ConnectedSeconds int    `json:"connected_seconds,omitempty"`
}

// PeerRadio is one radio's load as reported by a peer.
type PeerRadio struct {
	Band        string `json:"band,omitempty"`
	Channel     int    `json:"channel,omitempty"`
	Utilization int    `json:"utilization"`
	Clients     int    `json:"clients,omitempty"`
}
