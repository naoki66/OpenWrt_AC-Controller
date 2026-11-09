// Package proto defines the wire format spoken between the controller and the
// AP agent.
//
// Both binaries are built from this repository, so the structs here are the
// single definition: the controller's discovery listener, the AP-facing HTTP
// API and the agent all import them. Bumping ProtocolVersion is required for any
// incompatible change.
package proto

// UDP discovery message types.
const (
	MsgDiscover = "AP_DISCOVER"
	MsgOffer    = "AC_OFFER"
)

// ProtocolVersion is carried in every message so a newer agent can detect an
// older controller instead of misinterpreting a field.
//
// v1 — discovery, adoption, package distribution.
// v2 — Hello gained Radios (per-radio operating channel and airtime).
// v3 — Hello gained FirmwareState/Message; HelloReply gained Firmware.
// v4 — Hello gained Uptime (seconds since boot).
// v5 — HelloReply gained Bindings; action set gained kick_client.
// v6 — action set gained led_set (status LED on/off).
// v7 — Hello gained Caps (hostapd 11k/11v capability) and SteeringStats.
const ProtocolVersion = 7

// Firmware sync states reported in Hello. They mirror the config sync states
// but are a separate axis: a device can be running the right config on the
// wrong firmware.
const (
	FirmwareDownloading = "downloading" // image is being fetched before verification
	FirmwareUpgrading   = "upgrading"   // downloaded, verified, sysupgrade scheduled
	FirmwareApplied     = "applied"     // booted into the requested version
	FirmwareFailed      = "failed"      // sysupgrade did not land (still on old build)
)

// DiscoverPort is the UDP port the controller listens on. It deliberately
// avoids 29810 (TP-Link Omada) so a real Omada controller can coexist on the
// same segment while debugging.
const DiscoverPort = 29820

// Discover is broadcast by an AP on boot and whenever it loses its controller.
type Discover struct {
	Type     string `json:"type"`
	V        int    `json:"v"`
	MAC      string `json:"mac"`
	Model    string `json:"model"`
	Firmware string `json:"firmware"`
	Hostname string `json:"hostname"`
}

// Offer is the controller's reply: where to register, and which controller it is.
type Offer struct {
	Type           string `json:"type"`
	V              int    `json:"v"`
	ACID           string `json:"ac_id"`
	ACIP           string `json:"ac_ip"`
	HTTPPort       int    `json:"http_port"`
	SecretRequired bool   `json:"secret_required"`
}

// AP-facing sync states reported in Hello.
const (
	SyncIdle     = "idle"     // nothing to report
	SyncApplied  = "applied"  // revision applied and healthy
	SyncRollback = "rollback" // revision rejected, previous config restored
	SyncFailed   = "failed"   // apply failed and rollback also failed
)

// Hello is sent by the agent on every cycle. It doubles as registration and as
// the report channel, which keeps the AP's outbound traffic to one request per
// interval and makes the exchange idempotent.
type Hello struct {
	MAC        string `json:"mac"`
	Model      string `json:"model"`
	Firmware   string `json:"firmware"`
	Hostname   string `json:"hostname"`
	AppliedRev int    `json:"applied_rev"`
	// SyncState reports the outcome of the last apply attempt.
	SyncState   string `json:"sync_state"`
	SyncMessage string `json:"sync_message,omitempty"`
	// Steering is the client-steering daemon the AP finds enabled, as measured
	// on the device (not what the controller asked for). Empty means unknown.
	Steering string `json:"steering,omitempty"`
	// Caps is what this AP's hostapd actually exposes at runtime. Nil means the
	// agent does not probe (or the probe could not answer), which the controller
	// must not read as "no 11k/11v".
	Caps *WifiCaps `json:"caps,omitempty"`
	// SteeringStats is the running count of BTMs and their outcomes since the
	// agent started. Nil means this agent does not track them.
	SteeringStats *SteeringStat `json:"steering_stats,omitempty"`
	// Clients is the full associated-station list for this AP. Sending the
	// whole set each cycle (rather than deltas) means a lost message or an
	// agent restart self-heals on the next hello.
	// No omitempty: an empty array means "this AP has no clients", while an
	// absent field means "this agent does not collect client data".
	Clients []ClientStat `json:"clients"`
	// Neighbors is this AP's own 802.11k Neighbor Report, one entry per BSS.
	// hostapd encodes its operating class, channel and PHY type into the report
	// body, so the controller never has to know the radio layout of a peer AP:
	// it collects these and hands each BSS the entries of its siblings.
	// No omitempty for the same reason as Clients.
	Neighbors []NeighborBSS `json:"neighbors"`
	// Radios is the operating point of each radio: the channel it actually
	// landed on plus its airtime utilisation. No omitempty: nil means "this
	// agent does not report radio telemetry".
	Radios []RadioStat `json:"radios"`
	// Ports is the physical switch/ethernet port state read from sysfs. No
	// omitempty for the same reason as Radios: nil means this agent does not
	// collect port data, while an empty list means it has none to report.
	Ports []PortStat `json:"ports"`
	// FirmwareState reports the outcome of the last firmware upgrade attempt
	// (downloading/upgrading/applied/failed). Empty means nothing to report,
	// which is the steady state for the vast majority of hellos.
	FirmwareState   string `json:"firmware_state,omitempty"`
	FirmwareMessage string `json:"firmware_message,omitempty"`
	// Uptime is seconds since the AP booted, read from /proc/uptime. Zero means
	// the agent could not read it (or does not report it), which the controller
	// must not confuse with "just booted".
	Uptime int64 `json:"uptime,omitempty"`
	// Actions carries the outcome of commands the controller handed down in a
	// previous HelloReply. Nil means this agent does not support actions; an
	// empty list means it supports them and has nothing new to report.
	Actions []ActionResult `json:"actions"`
}

// Action types the controller may hand down. Kept as a closed set on both sides
// so a buggy or compromised controller cannot turn an AP into a remote shell.
const (
	ActionReboot       = "reboot"
	ActionWifiReload   = "wifi_reload"
	ActionFactoryReset = "factory_reset"
	// ActionKick deauthenticates one client from one BSS. Args: mac, ifname.
	ActionKick = "kick_client"
	// ActionDiagnose runs a fixed, read-only check on the AP and returns its
	// output as the action result message. Args: kind (one of Diagnose*).
	ActionDiagnose = "diagnose"
	// ActionLEDSet switches the AP's status LED. Args: state (on|off).
	ActionLEDSet = "led_set"
)

// Diagnose kinds. The set is closed on both sides: the agent never executes an
// arbitrary command, and the controller rejects anything it does not recognise.
const (
	DiagnoseNetwork = "network"
	DiagnoseSystem  = "system"
	DiagnoseKernel  = "kernel"
)

// Outcome values an AP reports for an action.
const (
	ActionResultDone   = "done"
	ActionResultFailed = "failed"
)

// HelloAction is one command the controller wants executed on an AP.
//
// ID is the idempotency key: the controller keeps re-sending an action until a
// result comes back, and the agent persists executed IDs so a retry (including
// one that arrives after a reboot) never runs the command twice.
type HelloAction struct {
	ID   string            `json:"id"`
	Type string            `json:"type"`
	Args map[string]string `json:"args,omitempty"`
}

// ActionResult is the AP's report for one HelloAction.
type ActionResult struct {
	ID      string `json:"id"`
	Type    string `json:"type,omitempty"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
	At      int64  `json:"at,omitempty"`
}

// WifiCaps is what the AP's hostapd actually exposes at runtime.
//
// K and V are only true when every BSS supports the corresponding methods:
// hostapd builds can expose a mixed surface while a radio is still coming up,
// and a partial "supported" would make the controller stop looking.
type WifiCaps struct {
	// BSS is the number of hostapd objects enumerated on the AP.
	BSS int  `json:"bss"`
	K   bool `json:"k"`
	V   bool `json:"v"`
	// R reports 802.11r (FT) fast roaming. Unlike 11k/11v, FT has no ubus
	// method to probe, so the agent reads the hostapd config files netifd
	// generated for the running daemon: R is true only when every BSS's
	// section sets ieee80211r=1. Unreadable config means false — "not
	// verified", matching the philosophy of K/V.
	R bool `json:"r"`
	// MD is the mobility domain (MDID) shared by the FT-enabled BSSs, shown
	// so operators can compare it across APs: FT only works when every AP
	// in the roaming set uses the same MDID. Empty when 11r is off.
	MD string `json:"md,omitempty"`
}

// SteeringStat is the agent's running count of BTM attempts and outcomes.
//
// The counters are per process, not per client, and reset when the agent
// restarts: they describe "what is steering doing right now", not a durable
// history. Accepted/Rejected come from hostapd's BTM response events when the
// build emits them; without those events the agent's association-based
// inference still feeds the same counters.
type SteeringStat struct {
	Sent        int `json:"sent"`
	Accepted    int `json:"accepted"`
	Rejected    int `json:"rejected"`
	Suppressed  int `json:"suppressed"`
	Unsupported int `json:"unsupported"`
}

// PortStat is one physical wired port as measured on the AP.
//
// These values come from /sys/class/net, not from the controller's desired
// configuration: Speed and Duplex are what the PHY negotiated, so the ports
// page can show whether a 2.5G port actually linked at 2.5G.
type PortStat struct {
	Name      string `json:"name"`
	MAC       string `json:"mac,omitempty"`
	Up        bool   `json:"up"`
	SpeedMbps int    `json:"speed_mbps,omitempty"`
	Duplex    string `json:"duplex,omitempty"`
	MTU       int    `json:"mtu,omitempty"`
}

// RadioStat is one radio's operating point as measured on the AP.
//
// Two things here are deliberately *measured* rather than configured:
//
//   - Channel is what the radio actually uses. With channel=auto the configured
//     value stays "auto" forever and only ACS knows the answer, so a controller
//     reading /etc/config would draw a flat line while the AP hops around.
//   - Utilization is hostapd's airtime busy percentage, the only interference
//     signal available without putting the controller on the data path.
//
// Radio is the UCI section name (radio0…), which is stable across reboots and is
// what the renderer produces, so it is safe to key history on.
type RadioStat struct {
	Radio string `json:"radio"`
	// Phy is the kernel wiphy name (phy0.1). Useful for cross-checking, not
	// for identity: it can shift when the device is reconfigured.
	Phy     string `json:"phy,omitempty"`
	Band    string `json:"band,omitempty"`
	Up      bool   `json:"up"`
	Channel int    `json:"channel"`
	FreqMHz int    `json:"freq_mhz,omitempty"`
	// HTMode is the configured width string (EHT160); WidthMHz is the same
	// thing as a number so it can be plotted.
	HTMode   string `json:"htmode,omitempty"`
	WidthMHz int    `json:"width_mhz,omitempty"`
	TxPower  int    `json:"txpower,omitempty"`
	// Country is the regulatory domain the radio was configured with (UCI
	// `option country`). It is a configured value, not a measured one, but the
	// radio page shows it next to the measured channel so the operator can see
	// both halves of the operating point at once. Empty = not set / old agent.
	Country string `json:"country,omitempty"`
	// Utilization is 0-100. -1 means hostapd did not report airtime.
	Utilization int `json:"utilization"`
	Clients     int `json:"clients,omitempty"`
}

// NeighborBSS is one BSS described the way 802.11k wants it advertised.
//
// Report is hostapd's own encoding (rrm_nr_get_own), kept opaque on purpose.
// Recomputing it from the template would mean reimplementing operating-class
// selection for three bands and every channel width; echoing it back through
// rrm_nr_set is what nrsyncd does and what keeps this correct across firmware.
type NeighborBSS struct {
	// Ifname is the netdev the entry belongs to (e.g. phy0.1-ap0). It is also
	// the key the controller uses to route peer entries back to a BSS.
	Ifname string `json:"ifname"`
	BSSID  string `json:"bssid"`
	SSID   string `json:"ssid"`
	Report string `json:"report"`
}

// ClientStat is one associated station as the AP sees it. The controller is not
// on the data path, so this is the only source of client signal information.
type ClientStat struct {
	MAC string `json:"mac"`
	// SSID/Band/Ifname identify where the client is associated; together with
	// the reporting AP's MAC they answer "which AP is this client on".
	SSID   string `json:"ssid,omitempty"`
	Band   string `json:"band,omitempty"`
	Ifname string `json:"ifname,omitempty"`
	Signal int    `json:"signal_dbm"`
	// Rates are in Mbps; 0 means unknown.
	RxRateMbps int `json:"rx_rate_mbps,omitempty"`
	TxRateMbps int `json:"tx_rate_mbps,omitempty"`
	// ConnectedSeconds is how long the station has been associated.
	ConnectedSeconds int `json:"connected_seconds,omitempty"`
	// InactiveSeconds is time since the last frame the station sent. mac80211
	// keeps an entry after a roam, so a large value marks a stale sample; the
	// steering paths skip those instead of chasing a ghost.
	InactiveSeconds int `json:"inactive_seconds,omitempty"`
	// IP is best effort. In a bridged AP the device never talks L3 with its
	// clients, so the neighbour table is usually empty in a bridged AP.
	IP string `json:"ip,omitempty"`
}

// Device states the controller reports back in HelloReply.State. They mirror
// the controller's own vocabulary; the agent only needs to tell "a template has
// been assigned to me" from "the controller has never been told what I am".
const (
	APStatePending = "pending"
	APStateAdopted = "adopted"
)

// HelloReply tells the agent what the controller wants it to be running.
type HelloReply struct {
	ACID  string `json:"ac_id"`
	State string `json:"state"` // APStatePending | APStateAdopted
	// DesiredRev is 0 when the controller has nothing to push. When it differs
	// from the agent's applied revision, the agent must fetch — including a
	// lower value, which is how a controller restored from an older database
	// rolls an AP back.
	DesiredRev int `json:"desired_rev"`
	// PackagePath is a path relative to the controller base URL.
	PackagePath string `json:"package_path,omitempty"`
	// PubKey is the Ed25519 verification key in hex. The agent pins the first
	// value it sees and refuses any later change (trust on first use).
	PubKey         string `json:"pubkey"`
	SecretRequired bool   `json:"secret_required"`
	// Firmware, when set, is an image the controller wants this AP to flash.
	// The agent verifies SHA-256 before touching sysupgrade; a controller that
	// omitted the digest would be asking for blind trust.
	Firmware *FirmwareRef `json:"firmware,omitempty"`
	// Actions are commands to execute before the next hello. Only actions that
	// have not produced a result yet are included; the agent dedupes by ID as
	// well, so a lost result costs a duplicate command at most once.
	Actions []HelloAction `json:"actions,omitempty"`
	// Bindings is the current set of client→SSID preferences ("smart magnet").
	// A nil list means the controller does not support bindings, so the agent
	// keeps whatever it has; an empty list is an authoritative "none".
	// No omitempty: the agent distinguishes "controller cleared the bindings"
	// (empty array) from "controller does not support bindings" (absent field).
	Bindings []ClientBinding `json:"bindings"`
	// NTP is the controller-managed site time-source list. Absent means the
	// controller does not manage NTP and the AP keeps its local configuration;
	// a non-nil list is authoritative and survives an AC outage once applied.
	NTP []string `json:"ntp,omitempty"`
}

// ClientBinding asks an AP's local steering loop to keep a station on one
// SSID, optionally on one AP. The controller resolves BSSID from the AP's own
// 802.11k report so the agent can match candidate entries directly.
type ClientBinding struct {
	MAC   string `json:"mac"`
	SSID  string `json:"ssid"`
	APMAC string `json:"ap_mac,omitempty"`
	BSSID string `json:"bssid,omitempty"`
}

// FirmwareRef locates one image in the controller's repository. Path is
// relative to the controller base URL, like PackagePath, so an AP never has to
// know how the repository is stored.
type FirmwareRef struct {
	Model   string `json:"model"`
	Version string `json:"version"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}
