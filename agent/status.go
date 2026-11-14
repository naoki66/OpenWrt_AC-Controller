package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/naoki66/ac-controller/shared/proto"
	"github.com/naoki66/ac-controller/shared/version"
)

// The control relationship as seen from the AP. The set is deliberately small
// and closed: a local UI shows one badge per value, and keeping the wording out
// of the agent means the translation lives where the display does.
//
// The distinction between connected and managed is the one an operator actually
// asks about ("can this AP be controlled?"): reachable but not adopted is a
// controller that has never been given a template for this device, while
// managed means a revision has been handed down.
const (
	StatusStarting    = "starting"    // just started, first cycle not finished
	StatusDiscovering = "discovering" // no controller address; broadcasting
	StatusUnreachable = "unreachable" // address known, last hello failed
	StatusConnected   = "connected"   // hello accepted, device not adopted yet
	StatusManaged     = "managed"     // adopted; a revision belongs to this AP
)

// Status is the agent's live self-description, published to
// /var/run/ac-agent/status.json on every cycle.
//
// Why a file and not a socket: it is read by LuCI through rpcd's file ACL,
// which needs no extra service, no listening port and no new privilege. The
// agent already rewrites a small file per cycle for its own state, so this adds
// no new failure mode — a stale file is detectable because UpdatedAt is always
// current while the process runs.
//
// It holds no secret: the shared secret and the pinned key are deliberately
// absent, because /var/run is world-readable by convention and a status screen
// has no business knowing the credential.
type Status struct {
	Version   string `json:"version"`
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
	UpdatedAt string `json:"updated_at"`

	MAC      string `json:"mac"`
	Model    string `json:"model"`
	Firmware string `json:"firmware"`
	Hostname string `json:"hostname"`

	Interval int  `json:"interval_seconds"`
	DryRun   bool `json:"dry_run"`
	Cycles   int  `json:"cycles"`

	State string `json:"state"`

	// ACAddr is the controller the agent is currently using; ACAddrSource says
	// whether it came from configuration (the WAN/uplink case, where broadcast
	// discovery cannot reach) or from a UDP answer on the local segment.
	ACAddr             string `json:"ac_addr,omitempty"`
	ACAddrSource       string `json:"ac_addr_source,omitempty"`
	ControllerOverride bool   `json:"controller_override,omitempty"`
	ACID               string `json:"ac_id,omitempty"`

	Adopted    bool   `json:"adopted"`
	AppliedRev int    `json:"applied_rev"`
	DesiredRev int    `json:"desired_rev"`
	SHA256     string `json:"sha256,omitempty"`
	AppliedAt  string `json:"applied_at,omitempty"`

	LastHelloAt    string `json:"last_hello_at,omitempty"`
	LastHelloOK    bool   `json:"last_hello_ok"`
	LastHelloError string `json:"last_hello_error,omitempty"`

	LastError     string `json:"last_error,omitempty"`
	BSS           int    `json:"bss"`
	PeerNetwork   string `json:"peer_network,omitempty"`
	PeerPort      int    `json:"peer_port,omitempty"`
	PeerSteer     bool   `json:"peer_steer"`
	PeerTransport string `json:"peer_transport,omitempty"`
	PeerDevice    string `json:"peer_device,omitempty"`
	PeerAPs       int    `json:"peer_aps"`
	PeerBSSes     int    `json:"peer_bss"`
	FirmwareState string `json:"firmware_state,omitempty"`

	// Steering policy as configured locally, plus what the device actually
	// runs. SteeringMode is one of ac_priority/ac_only/autonomous;
	// SteeringFallback is the daemon that takes over when the controller is
	// lost ("none" or empty disables the fallback). Steering is the daemon the
	// device currently has enabled, as measured on the init scripts — empty
	// until the first probe. SteeringFallbackActive is true while the device
	// runs the fallback instead of the controller's directive.
	SteeringMode           string `json:"steering_mode,omitempty"`
	SteeringFallback       string `json:"steering_fallback,omitempty"`
	SteeringFallbackActive bool   `json:"steering_fallback_active,omitempty"`
	Steering               string `json:"steering,omitempty"`

	// SteerPolicyActive is true while a package-borne steering policy is in
	// force; SteerBTM/SteerForced count the local actions since start, from both
	// the policy loop and the ad-hoc suggestion path. SteerSuppressed counts
	// suggestions withheld because the client already declined three of them —
	// the number an operator uses to tell "steering is quiet" from "steering is
	// not running". SteerUnsupported breaks out the subset of clients that were
	// never sent one at all because their association request did not declare
	// any 11k/11v capability: those are not declining, they cannot comply.
	SteerPolicyActive bool `json:"steer_policy_active"`
	SteerBTM          int  `json:"steer_btm_suggestions"`
	SteerForced       int  `json:"steer_forced_kicks"`
	SteerSuppressed   int  `json:"steer_suppressed"`
	SteerUnsupported  int  `json:"steer_unsupported"`

	// SteeringStats is the same snapshot sent to the controller, kept as one
	// object so a local UI and the AC show identical numbers. Nil when this
	// agent has no guard (the older flat fields above remain for compatibility).
	SteeringStats *proto.SteeringStat `json:"steering_stats,omitempty"`
}

// StatusSnapshot returns the current process's status without writing it.
func (a *Agent) StatusSnapshot() Status {
	a.reflectStatus()
	return a.rt
}

// LiveStatus reads the daemon's recent status for a separate CLI process.
func (a *Agent) LiveStatus() (Status, bool) {
	var status Status
	body, err := os.ReadFile(a.Config.statusPath())
	if err != nil || json.Unmarshal(body, &status) != nil || status.PID <= 0 || status.MAC != a.Identity.MAC {
		return Status{}, false
	}
	at, err := time.Parse(time.RFC3339, status.UpdatedAt)
	if err != nil || status.Interval < 1 || status.Interval > 86400 {
		return Status{}, false
	}
	age := time.Since(at)
	if age < -5*time.Second || age > max(15*time.Second, 3*time.Duration(status.Interval)*time.Second) {
		return Status{}, false
	}
	return status, true
}

// writeStatus publishes the status file. Failures are never fatal: the file is
// a convenience for local UIs, and a provisioning agent must keep working on a
// read-only /var.
func (a *Agent) writeStatus() {
	a.reflectStatus()

	body, err := json.MarshalIndent(a.rt, "", "  ")
	if err != nil {
		a.Log.Debug("status: encode failed", "err", err)
		return
	}

	path := a.Config.statusPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		a.Log.Debug("status: create dir failed", "path", path, "err", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o644); err != nil {
		a.Log.Debug("status: write failed", "path", path, "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		a.Log.Debug("status: replace failed", "path", path, "err", err)
	}
}

// reflectStatus rebuilds the derived fields from the live agent. Everything it
// copies is either already held in memory or read from the persisted state, so
// the file can never claim a revision the AP did not actually apply.
func (a *Agent) reflectStatus() {
	now := time.Now().UTC().Format(time.RFC3339)

	a.rt.Version = version.Version
	a.rt.UpdatedAt = now
	a.rt.MAC = a.Identity.MAC
	a.rt.Model = a.Identity.Model
	a.rt.Firmware = a.Identity.Firmware
	a.rt.Hostname = a.Identity.Hostname
	a.rt.DryRun = a.Config.DryRun
	a.rt.Cycles = a.cycles
	if a.rt.PID == 0 {
		a.rt.PID = os.Getpid()
	}
	if a.rt.StartedAt == "" {
		a.rt.StartedAt = now
	}
	if a.Config.Interval > 0 {
		a.rt.Interval = int(a.Config.Interval / time.Second)
	}

	a.rt.ACAddr = a.acAddr
	a.rt.ControllerOverride = a.state.ManagementKey != "" || a.Config.overrideTrustKey != ""
	switch {
	case a.acAddr == "":
		a.rt.ACAddrSource = ""
	case a.pinnedAC:
		a.rt.ACAddrSource = "config"
	default:
		a.rt.ACAddrSource = "discovered"
	}

	a.rt.AppliedRev = a.state.AppliedRev
	a.rt.SHA256 = a.state.SHA256
	a.rt.AppliedAt = a.state.AppliedAt
	a.rt.LastError = a.state.LastError
	a.rt.FirmwareState = a.state.FirmwareState
	a.rt.SteeringMode = steeringModeOrDefault(a.Config.SteeringMode)
	a.rt.SteeringFallback = a.Config.SteeringFallback
	a.rt.SteeringFallbackActive = a.state.SteeringFallbackActive
	a.steeringMu.Lock()
	if a.steeringKnown {
		a.rt.Steering = a.steeringCache
	}
	a.steeringMu.Unlock()
	if a.steer != nil {
		a.rt.SteerPolicyActive, a.rt.SteerBTM, a.rt.SteerForced = func() (bool, int, int) {
			active, counts := a.steer.snapshot()
			// The ad-hoc path shares the same intent, so its suggestions belong
			// in the same counter: an operator asking "is steering doing
			// anything" should not have to know which of two paths fired.
			return active, counts.BTM + a.adhocBTMs, counts.Forced
		}()
	}
	if a.btm != nil {
		a.rt.SteerSuppressed = a.btm.counts()
		a.rt.SteerUnsupported = a.btm.unsupportedCount()
		st := a.btm.stats()
		a.rt.SteeringStats = &st
	}
	if a.state.ACID != "" {
		a.rt.ACID = a.state.ACID
	}
	a.rt.BSS = a.bssCount()
	if a.peerBus != nil {
		a.rt.PeerNetwork = a.Config.PeerNetwork
		a.rt.PeerPort = a.Config.PeerPort
		a.rt.PeerSteer = a.Config.PeerSteer
	}

	// Adopted is whatever the controller said last, not a local memory. Making
	// it sticky looked safer but broke the case an operator actually hits:
	// releasing an AP on the controller has to be visible here too, otherwise
	// the page keeps claiming the device is managed after it was unbound.
	// (The AP still runs its last applied revision either way; applied_rev is
	// reported separately, so "connected" with a non-zero rev reads as
	// "the controller lost track of me".)

	switch {
	case a.cycles == 0:
		a.rt.State = StatusStarting
	case a.acAddr == "":
		a.rt.State = StatusDiscovering
	case !a.rt.LastHelloOK:
		a.rt.State = StatusUnreachable
	case !a.rt.Adopted:
		a.rt.State = StatusConnected
	default:
		a.rt.State = StatusManaged
	}
}

// bssCount is the number of BSSes this AP last reported to the controller. It
// is the same sample the 11k lists are built from, so it costs nothing extra.
func (a *Agent) bssCount() int {
	return len(a.neighborCache)
}
