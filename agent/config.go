package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/naoki66/ac-controller/shared/proto"
)

// Steering policy modes (internal default only; the LuCI/UCI option was
// removed).
//
// The controller is the decision maker whenever it is reachable: applySteering
// stops every other daemon, so the AC's directive always outranks a locally
// installed dawn/usteer. The default is ac_priority; when the controller is
// lost, the AP-to-AP channel and the built-in 11v suggestion keep working
// instead of switching to a different daemon.
const (
	// SteeringModeACPriority keeps the controller in charge while it answers.
	// This is the default and the only mode the shipped configuration uses.
	SteeringModeACPriority = "ac_priority"
	// SteeringModeACOnly never deviates from the controller's last directive.
	// When the controller goes away the last state simply keeps running.
	SteeringModeACOnly = "ac_only"
	// SteeringModeAutonomous makes the agent ignore the steering file entirely:
	// the local daemons are managed by hand (or by nothing), and the AC's
	// steering directive is reported but never enforced.
	SteeringModeAutonomous = "autonomous"
)

// Legacy fallback daemons for SteeringModeACPriority. "none" (and an empty
// value) means no daemon switch on an outage, which is the shipped default;
// local AP-to-AP coordination and the built-in BTM fill the gap instead.
const (
	SteeringFallbackNone    = "none"
	SteeringFallbackUsteer  = "usteer"
	SteeringFallbackDawn    = "dawn"
	SteeringFallbackNRSyncd = "nrsyncd"
)

// Config is everything the agent needs to run. Every path is expressed relative
// to Root so the same code can be exercised on a workstation against a copy of
// an AP's /etc tree instead of only on the device.
type Config struct {
	// Root prefixes all filesystem paths. Empty means "/" on the device.
	Root string
	// ACAddr is "host:port" of a known controller. Empty means "discover first".
	ACAddr string
	// Secret derives separate credentials for controller and peer authentication.
	Secret string
	// TrustedControllerKey is a public key provisioned by the firmware build.
	TrustedControllerKey string
	overrideTrustKey     string
	// Interval is the hello/poll period.
	Interval time.Duration
	// HealthTimeout bounds how long the agent waits for BSSes to come up.
	HealthTimeout time.Duration
	// DiscoverPort is the UDP port used for AP_DISCOVER.
	DiscoverPort int
	// DryRun renders and stages files but never touches /etc/config and never
	// runs reload commands.
	DryRun bool
	// KeepDHCP opts out of stopping dnsmasq/odhcpd. Only useful when the AP is
	// deliberately kept as a router; an AP in bridged mode must not serve DHCP.
	KeepDHCP bool
	// DisableDHCP explicitly opts a bridge AP into DHCP service takeover.
	DisableDHCP bool
	// Runner overrides command execution. Nil means the real system, which is
	// what the device uses; tests inject a fake.
	Runner Runner
	// Identity, when set, overrides what would be read from the system.
	Identity Identity
	// PeerPort is the UDP port for AP-to-AP coordination. Zero means the
	// default; a negative value disables peer coordination entirely.
	PeerPort int
	// PeerInterval is how often this AP announces its BSS/client state to peers.
	PeerInterval time.Duration
	// PeerIntervalOffline is the announcement cadence used while the controller
	// is known to be unreachable. Zero means the default (20 s).
	PeerIntervalOffline time.Duration
	// PeerSteer allows conservative local 11v suggestions when a client is
	// already very weak and a peer advertises the same SSID. On by default in
	// the shipped UCI file: after the controller is lost this is what keeps
	// roaming working across the APs.
	PeerSteer bool
	// PeerNetwork is the OpenWrt logical network (not a device name) that carries
	// AP-to-AP traffic. It is resolved to l3_device for IPv6 link-local multicast.
	// Empty means "try the default network name lan", and a failed resolution
	// falls back to IPv4 broadcast.
	PeerNetwork string
	// StationSource selects how the associated-station table is read:
	// "auto" (netlink, falling back to iw), "nl80211", or "iw". Empty means
	// auto. iw costs a fork+exec per BSS, netlink a syscall pair; the option
	// exists so a site can fall back without rebuilding the binary.
	StationSource string
	// Netlink enables reading the BSS list and the neighbour table straight
	// from the kernel instead of spawning ubus and ip. On by default: the two
	// commands cost ~12 ms per sample, almost entirely fork+exec. AC_NETLINK=off
	// restores them, which is the escape hatch for a kernel whose netlink
	// replies do not match what this agent decodes.
	Netlink bool
	// Ubus enables talking to ubusd over its own socket instead of spawning the
	// ubus binary for every status read. On by default: each of those calls costs
	// 3.5-4 ms almost entirely in fork+exec. AC_UBUS=off restores the CLI, which
	// is both the escape hatch and the way to measure the difference.
	Ubus bool
	// StatusPath overrides where the live status file is written. Empty means
	// /var/run/ac-agent/status.json under Root. It lives in tmpfs because it is
	// a snapshot of a running process, not a fact worth surviving a reboot.
	StatusPath string
	// SteeringMode selects how the agent resolves conflicts between the
	// controller's steering directive and locally installed daemons. See the
	// SteeringMode* constants. Empty means ac_priority.
	SteeringMode string
	// SteeringFallback names the daemon to run when the controller is lost in
	// SteeringModeACPriority. Empty or "none" disables the fallback.
	SteeringFallback string
	// HelloBackoffMax caps the heartbeat backoff applied once the controller is
	// known to be gone. Zero means the default. See Agent.helloBackoff.
	HelloBackoffMax time.Duration
}

// Identity describes the AP to the controller.
type Identity struct {
	MAC      string
	Model    string
	Firmware string
	Hostname string
}

// Defaults for a real device.
const (
	defaultInterval = 20 * time.Second
	// 45s, not 15: a layer-2 takeover rebuilds every bridge VLAN device and
	// then restarts hostapd for three radios. 15s was measured too short on an
	// XR1710G — the check timed out while BSSes were still coming up.
	defaultHealthTimeout = 45 * time.Second
	// defaultHelloBackoffMax bounds the heartbeat backoff during a long outage.
	// Three minutes would save more attempts, but a controller that comes back
	// would then stay invisible for minutes while every AP is already talking to
	// its peers; one minute keeps recovery prompt at a third of the attempts.
	defaultHelloBackoffMax = 60 * time.Second
)

// Load builds a Config from AC_* environment variables, then fills the gaps by
// reading the system. Explicit values in c (typically from flags) win over the
// environment.
func Load(c Config) Config {
	if c.Root == "" {
		if v := os.Getenv("AC_ROOT"); v != "" {
			c.Root = v
		}
	}
	if c.Root == "" {
		c.Root = "/"
	}
	if c.ACAddr == "" {
		c.ACAddr = os.Getenv("AC_ADDR")
	}
	if c.Secret == "" {
		c.Secret = os.Getenv("AC_SECRET")
	}
	c.Secret = strings.TrimSpace(c.Secret)
	if c.Interval == 0 {
		c.Interval = defaultInterval
		if v := os.Getenv("AC_INTERVAL"); v != "" {
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				c.Interval = d
			}
		}
	}
	if c.HealthTimeout == 0 {
		c.HealthTimeout = defaultHealthTimeout
		if v, err := time.ParseDuration(os.Getenv("AC_HEALTH_TIMEOUT")); err == nil && v >= 10*time.Second && v <= 15*time.Minute {
			c.HealthTimeout = v
		}
	}
	if c.DiscoverPort == 0 {
		c.DiscoverPort = 29820
	}
	if v := os.Getenv("AC_DRY_RUN"); v == "1" || strings.EqualFold(v, "true") {
		c.DryRun = true
	}
	if v := os.Getenv("AC_KEEP_DHCP"); v == "1" || strings.EqualFold(v, "true") {
		c.KeepDHCP = true
	}
	if v := os.Getenv("AC_DISABLE_DHCP"); v == "1" || strings.EqualFold(v, "true") {
		c.DisableDHCP = true
	}
	if c.PeerPort == 0 {
		c.PeerPort = proto.PeerPort
		if v := os.Getenv("AC_PEER_PORT"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				if n < 0 {
					n = 0
				}
				c.PeerPort = n
			}
		}
	}
	if c.PeerInterval == 0 {
		c.PeerInterval = defaultPeerInterval
		if v := os.Getenv("AC_PEER_INTERVAL"); v != "" {
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				c.PeerInterval = d
			}
		}
	}
	if c.PeerIntervalOffline == 0 {
		c.PeerIntervalOffline = defaultPeerIntervalOffline
		if v := os.Getenv("AC_PEER_INTERVAL_OFFLINE"); v != "" {
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				c.PeerIntervalOffline = d
			}
		}
	}
	if c.HelloBackoffMax == 0 {
		c.HelloBackoffMax = defaultHelloBackoffMax
		if v := os.Getenv("AC_HELLO_BACKOFF_MAX"); v != "" {
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				c.HelloBackoffMax = d
			}
		}
	}
	if v := os.Getenv("AC_PEER_STEER"); v == "1" || strings.EqualFold(v, "true") {
		c.PeerSteer = true
	}
	if c.PeerNetwork == "" {
		c.PeerNetwork = os.Getenv("AC_PEER_NETWORK")
	}
	if c.PeerNetwork == "" {
		c.PeerNetwork = "lan"
	}
	if c.StationSource == "" {
		c.StationSource = strings.TrimSpace(os.Getenv("AC_STATION_SOURCE"))
	}
	c.Netlink = strings.ToLower(strings.TrimSpace(os.Getenv("AC_NETLINK"))) != "off"
	c.Ubus = strings.ToLower(strings.TrimSpace(os.Getenv("AC_UBUS"))) != "off"
	if c.SteeringMode == "" {
		c.SteeringMode = os.Getenv("AC_STEERING_MODE")
	}
	switch c.SteeringMode {
	case "":
		c.SteeringMode = SteeringModeACPriority
	case SteeringModeACPriority, SteeringModeACOnly, SteeringModeAutonomous:
	default:
		// An unknown mode must not disable the controller silently: keep the
		// safe default and let the operator notice the mismatch on the page.
		c.SteeringMode = SteeringModeACPriority
	}
	if c.SteeringFallback == "" {
		c.SteeringFallback = os.Getenv("AC_STEERING_FALLBACK")
	}
	switch c.SteeringFallback {
	case "", SteeringFallbackNone, SteeringFallbackUsteer, SteeringFallbackDawn, SteeringFallbackNRSyncd:
	default:
		c.SteeringFallback = SteeringFallbackNone
	}
	if c.StatusPath == "" {
		c.StatusPath = os.Getenv("AC_STATUS_PATH")
	}
	return c
}

func (c Config) configDir() string { return filepath.Join(c.Root, "etc", "config") }
func (c Config) stateDir() string  { return filepath.Join(c.Root, "etc", "ac") }

func (c Config) wirelessPath() string { return filepath.Join(c.configDir(), "wireless") }
func (c Config) networkPath() string  { return filepath.Join(c.configDir(), "network") }
func (c Config) statePath() string    { return filepath.Join(c.stateDir(), "state.json") }

// statusPath is the live self-description a local UI reads. /var/run is tmpfs on
// OpenWrt, which is what we want: the status describes this process, and a file
// left behind by a previous boot must not be mistakable for a running agent.
func (c Config) statusPath() string {
	if c.StatusPath != "" {
		return c.StatusPath
	}
	return filepath.Join(c.Root, "var", "run", "ac-agent", "status.json")
}

// DetectIdentity reads what the controller needs to identify this AP. Every
// lookup has a fallback: a wrong model string only weakens the model whitelist,
// whereas failing here would leave the AP unprovisionable.
func DetectIdentity(root string) Identity {
	id := Identity{
		MAC:      detectMAC(root),
		Model:    firstLine(filepath.Join(root, "tmp", "sysinfo", "model")),
		Firmware: openwrtRelease(filepath.Join(root, "etc", "openwrt_release")),
	}
	if h, err := os.Hostname(); err == nil {
		id.Hostname = h
	}
	if id.Hostname == "" {
		id.Hostname = "unknown"
	}
	if id.Model == "" {
		id.Model = "unknown"
	}
	return id
}

// detectMAC walks the usual OpenWrt candidates: the bridge first (that is what
// the controller sees on the wire), then the wired uplink.
func detectMAC(root string) string {
	for _, name := range []string{"br-lan", "eth0", "lan1", "wan"} {
		if v := strings.TrimSpace(firstLine(filepath.Join(root, "sys", "class", "net", name, "address"))); v != "" {
			return strings.ToLower(v)
		}
	}
	return ""
}

func openwrtRelease(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(body), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(key) == "DISTRIB_DESCRIPTION" {
			return strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return ""
}

func firstLine(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(body), "\n") {
		if v := strings.TrimSpace(line); v != "" {
			return v
		}
	}
	return ""
}

// systemUptime reads seconds since boot. It returns 0 when the file is
// unavailable, which is distinct from a freshly booted AP: the controller shows
// "unknown" rather than "0 秒".
func systemUptime(root string) int64 {
	body, err := os.ReadFile(filepath.Join(root, "proc", "uptime"))
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return 0
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || secs <= 0 {
		return 0
	}
	return int64(secs)
}

// State is the agent's own memory. It is the source of truth for "what has this
// AP actually applied", which the controller can only ever learn second hand.
type State struct {
	EnrollmentID         string `json:"enrollment_id,omitempty"`
	ResetGeneration      int64  `json:"reset_generation,omitempty"`
	ManagementKey        string `json:"management_key,omitempty"`
	ManagementTrafficKey string `json:"management_traffic_key,omitempty"`
	ManagementAddr       string `json:"management_addr,omitempty"`
	ManagementReset      bool   `json:"management_reset,omitempty"`
	ControllerOverrideID string `json:"controller_override_id,omitempty"`
	ACID                 string `json:"ac_id,omitempty"`
	PubKey               string `json:"pubkey,omitempty"`
	AppliedRev           int    `json:"applied_rev"`
	SHA256               string `json:"sha256,omitempty"`
	AppliedAt            string `json:"applied_at,omitempty"`
	LastError            string `json:"last_error,omitempty"`
	// PendingSync is a report that has not reached the controller yet. Keeping
	// it on disk means a reboot does not swallow the outcome of an apply that
	// already happened.
	PendingSync    string `json:"pending_sync,omitempty"`
	PendingSyncMsg string `json:"pending_sync_message,omitempty"`
	// FirmwareTarget/State/Message/At track one upgrade. They are separate from
	// the config sync fields because the two axes are independent: an AP can be
	// on the right revision and the wrong build. At is when the state was last
	// written, and is what turns a scheduled upgrade into a failure when no
	// reboot ever follows.
	FirmwareTarget  string `json:"firmware_target,omitempty"`
	FirmwareState   string `json:"firmware_state,omitempty"`
	FirmwareMessage string `json:"firmware_message,omitempty"`
	FirmwareAt      string `json:"firmware_at,omitempty"`
	// Steering is the last directive the controller actually handed down in a
	// package (the steering file's content, not what the device runs). It is
	// what the agent re-asserts when the controller comes back after an
	// autonomous-steering outage. Empty means "never seen a steering file".
	Steering string `json:"steering,omitempty"`
	// SteeringFallbackActive is true while the device is running the locally
	// chosen fallback daemon instead of the controller's directive. Persisted
	// so a reboot during an outage does not reset the decision, and so the
	// recovery path fires exactly once.
	SteeringFallbackActive bool `json:"steering_fallback_active,omitempty"`
	// SteerPolicyRaw is the steer-policy file's content from the last package,
	// kept verbatim so the local steering loop survives an agent restart
	// without waiting for a new revision. Empty means no policy.
	SteerPolicyRaw string `json:"steer_policy,omitempty"`
	// ClientBindingsRaw is the last binding set received in a hello reply,
	// stored as JSON so a controller outage does not stop local magnet
	// steering. Empty means the controller has never sent bindings.
	ClientBindingsRaw string `json:"client_bindings,omitempty"`
	// NTPServers is the last controller-managed time-source list applied to
	// /etc/config/system. It is persisted so a reboot during an AC outage does
	// not lose the time source; an empty list means the controller never sent one.
	NTPServers []string `json:"ntp_servers,omitempty"`
	// ActionResults are finished controller actions that have not been
	// acknowledged yet. Kept on disk so a reboot in the middle of reporting a
	// command does not lose the outcome.
	ActionResults []proto.ActionResult `json:"action_results,omitempty"`
	// ExecutedActions is a bounded history of action IDs. It is written before
	// the command runs, which is what stops a reboot from being executed twice
	// when the controller retries an action whose result never arrived.
	ExecutedActions []string `json:"executed_actions,omitempty"`
	// PendingConfirm tracks a network revision and its post-commit work.
	// If no signed acknowledgement arrives before Deadline, restore BackupDir:
	// a push that makes the AP unreachable from the operator's side must not be
	// permanent.
	PendingConfirm *PendingConfirm `json:"pending_confirm,omitempty"`
	// PendingApply is written before any live configuration file is replaced.
	PendingApply            string `json:"pending_apply,omitempty"`
	PendingApplyRestoreDHCP bool   `json:"pending_apply_restore_dhcp,omitempty"`
}

// PendingConfirm is the commit-confirm window for a network change.
type PendingConfirm struct {
	Rev       int    `json:"rev"`
	BackupDir string `json:"backup_dir"`
	// Deadline is RFC3339 in UTC.
	Deadline string `json:"deadline"`
	// RestoreDHCP records that the apply switched DHCP off although it ran
	// before, so the revert has to switch it back on.
	RestoreDHCP       bool   `json:"restore_dhcp,omitempty"`
	PreviousRev       int    `json:"previous_rev,omitempty"`
	PreviousSHA256    string `json:"previous_sha256,omitempty"`
	PreviousAppliedAt string `json:"previous_applied_at,omitempty"`
	Confirmed         bool   `json:"confirmed,omitempty"`
	DeferredAux       bool   `json:"deferred_aux,omitempty"`
	Steering          string `json:"steering,omitempty"`
	SteerPolicy       string `json:"steer_policy,omitempty"`
	Neighbors         []byte `json:"neighbors,omitempty"`
}

// LoadState reads the state file. A missing file is not an error: it simply
// means "nothing applied yet".
func LoadState(path string) (State, error) {
	var st State
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return st, fmt.Errorf("decode state %s: %w", path, err)
	}
	return st, nil
}

// SaveState writes the state atomically (0600: it holds management credentials, and a
// half-written file must never be readable as valid).
func SaveState(path string, st State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	body, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeAll(map[string]string{path: string(body) + "\n"})
}

// itoa is a tiny helper kept for readable error messages.
func itoa(n int) string { return strconv.Itoa(n) }
