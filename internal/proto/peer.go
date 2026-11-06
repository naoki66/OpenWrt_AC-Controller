package proto

// Peer coordination between AP agents (no controller on the path).
//
// The controller already aggregates BSS identities in the normal case. This
// message exists for the case the design has to survive: the controller is
// unreachable, an AP was just added, or a radio lost its neighbour list to a
// reload. Agents then exchange the same 802.11k self-reports directly over
// the management LAN, exactly the way nrsyncd does, so roaming keeps working
// without the AC.

// MsgPeer is the UDP message type for AP-to-AP announcements.
const MsgPeer = "AP_PEER"

// PeerPort is the default UDP port agents listen on. It is deliberately one
// above DiscoverPort so the two listeners never collide.
const PeerPort = 29821

// PeerVersion is the wire version of PeerState.
const PeerVersion = 1

// PeerState is one AP's periodic announcement.
type PeerState struct {
	Type string `json:"type"`
	V    int    `json:"v"`
	MAC  string `json:"mac"`
	// Model and Hostname are for operator-visible debugging; nothing acts on
	// them.
	Model    string `json:"model,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	// BSS is this AP's own 802.11k report, one entry per BSS. Peer agents copy
	// these verbatim into rrm_nr_set, so the controller never has to interpret
	// operating classes.
	BSS []NeighborBSS `json:"bss"`
	// Clients is the associated-station set this AP currently serves. It is not
	// used to compute another AP's neighbour list; it exists so a peer can see
	// whether a candidate radio is carrying load.
	Clients []PeerClient `json:"clients,omitempty"`
	// Radios is the operating point of each radio, with airtime utilisation.
	Radios []PeerRadio `json:"radios,omitempty"`
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
