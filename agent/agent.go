package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/naoki66/ac-controller/shared/bundle"
	"github.com/naoki66/ac-controller/shared/proto"
	"github.com/naoki66/ac-controller/shared/render"
	"github.com/naoki66/ac-controller/shared/secret"
)

// Client talks to one controller.
type Client struct {
	BaseURL string
	Secret  string
	HTTP    *http.Client
}

// Controller HTTP timeouts.
//
// A single 30 s total timeout was wrong for the heartbeat. When the controller
// disappears behind a black hole — powered off, route withdrawn, firewall
// dropping — a connect attempt waits out the full limit, and the agent's cycle
// is serial: the peer announcement and the next sample sit behind that wait. A
// 20 s heartbeat then effectively becomes a 30 s one, and every AP on the site
// slows down at exactly the moment the AP-to-AP channel is all they have left.
//
// Splitting the budget puts the failure where it belongs. Establishing a
// connection to a controller on the management LAN is a local operation: 3 s
// covers a loaded AP, and anything slower means it is not there. The same
// reasoning applies to the response header, which the controller writes
// immediately after reading a small JSON body. The total limit stays generous
// because the same client also fetches a configuration package, and a few
// hundred kilobytes over a constrained uplink is not a fault.
const (
	controllerDialTimeout   = 3 * time.Second
	controllerHeaderTimeout = 6 * time.Second
	controllerTotalTimeout  = 15 * time.Second
)

// newControllerHTTPClient builds the transport used for every controller call.
// Keep-alive is left on: one hello every 20 s reuses the connection, so most
// cycles pay neither a TCP nor a TLS handshake.
func newControllerHTTPClient() *http.Client {
	return &http.Client{
		Timeout: controllerTotalTimeout,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   controllerDialTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ResponseHeaderTimeout: controllerHeaderTimeout,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          4,
		},
	}
}

// NewClient builds a client for "http://host:port".
func NewClient(baseURL, secret string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Secret:  secret,
		HTTP:    newControllerHTTPClient(),
	}
}

// Hello registers, reports and polls in a single round trip.
func (c *Client) Hello(ctx context.Context, h proto.Hello) (proto.HelloReply, error) {
	body, err := json.Marshal(h)
	if err != nil {
		return proto.HelloReply{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/ap/hello", bytes.NewReader(body))
	if err != nil {
		return proto.HelloReply{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret.AuthToken(c.Secret))
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return proto.HelloReply{}, err
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return proto.HelloReply{}, fmt.Errorf("hello: controller returned %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}

	var reply proto.HelloReply
	if err := json.Unmarshal(payload, &reply); err != nil {
		return proto.HelloReply{}, fmt.Errorf("hello: decode reply: %w", err)
	}
	return reply, nil
}

// FetchPackage downloads a package by its controller-relative path.
func (c *Client) FetchPackage(ctx context.Context, path string) ([]byte, error) {
	url := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret.AuthToken(c.Secret))
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("download %s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// A configuration package is tens of KB; anything near this bound means
	// something other than a package answered.
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// Discover broadcasts AP_DISCOVER and waits for one AC_OFFER.
func Discover(ctx context.Context, cfg Config, id Identity, log *slog.Logger) (string, proto.Offer, error) {
	msg, err := json.Marshal(proto.Discover{
		Type:     proto.MsgDiscover,
		V:        proto.ProtocolVersion,
		MAC:      id.MAC,
		Model:    id.Model,
		Firmware: id.Firmware,
		Hostname: id.Hostname,
	})
	if err != nil {
		return "", proto.Offer{}, err
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return "", proto.Offer{}, err
	}
	defer conn.Close()

	port := cfg.DiscoverPort
	if port == 0 {
		port = proto.DiscoverPort
	}
	targets := broadcastTargets(port)
	deadline := time.Now().Add(3 * time.Second)
	for _, target := range targets {
		if _, err := conn.WriteToUDP(msg, target); err != nil {
			log.Debug("discovery send failed", "target", target.String(), "err", err)
		}
	}
	_ = conn.SetReadDeadline(deadline)

	buf := make([]byte, 2048)
	for {
		if ctx.Err() != nil {
			return "", proto.Offer{}, ctx.Err()
		}
		n, peer, err := conn.ReadFromUDP(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return "", proto.Offer{}, fmt.Errorf("no controller answered on %d target(s)", len(targets))
			}
			return "", proto.Offer{}, err
		}

		var offer proto.Offer
		if err := json.Unmarshal(buf[:n], &offer); err != nil || offer.Type != proto.MsgOffer {
			continue
		}

		host := offer.ACIP
		if host == "" {
			host = peer.IP.String()
		}
		if host == "" || offer.HTTPPort == 0 {
			continue
		}
		addr := fmt.Sprintf("%s:%d", host, offer.HTTPPort)
		log.Info("controller discovered", "addr", addr, "ac_id", offer.ACID)
		return addr, offer, nil
	}
}

// broadcastTargets returns the limited broadcast address plus every interface's
// directed broadcast, because a switch or firewall may drop 255.255.255.255
// while still forwarding a subnet broadcast.
func broadcastTargets(port int) []*net.UDPAddr {
	targets := []*net.UDPAddr{{IP: net.IPv4bcast, Port: port}}

	ifaces, err := net.Interfaces()
	if err != nil {
		return targets
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil {
				continue
			}
			mask := net.IP(ipnet.Mask).To4()
			if mask == nil {
				continue
			}
			bcast := make(net.IP, 4)
			for i := 0; i < 4; i++ {
				bcast[i] = ip4[i] | ^mask[i]
			}
			targets = append(targets, &net.UDPAddr{IP: bcast, Port: port})
		}
	}
	return targets
}

// Agent is the long-running AP-side process.
type Agent struct {
	Config    Config
	Identity  Identity
	Applier   *Applier
	Clients   *ClientCollector
	Neighbors *NeighborCollector
	Radios    *RadioCollector
	// caps probes what hostapd exposes for 11k/11v. Cached inside the collector:
	// the probe costs one ubus fork per BSS.
	caps *capsCollector
	// Runner executes the one command that is not part of the apply pipeline:
	// the detached sysupgrade used by a firmware upgrade.
	Runner Runner
	Log    *slog.Logger

	// ConfirmWindow is how long an address move may stay unacknowledged by the
	// controller before the agent restores the previous configuration. Zero
	// means defaultConfirmWindow.
	ConfirmWindow time.Duration

	// ubus is the shared ubusd connection used by the collectors. Nil when
	// AC_UBUS=off, which makes every caller fall back to the CLI.
	ubus *ubusClient

	state  State
	acAddr string
	client *Client

	// peerBus is the AP-to-AP coordination channel. Nil when disabled.
	peerBus *PeerBus
	// peerVersion is the peer BSS-set version this AP has already reacted to.
	// peerVersionSeen separates "not observed yet" from version zero: an AP that
	// boots with no peers alive produces version 0, and treating that as
	// "unobserved" would swallow the first peer that ever appears.
	peerVersion     uint64
	peerVersionSeen bool
	// peerNTP is the time-source list learned from a peer this cycle. It is used
	// only when this AP has never received a controller list, so a new AP during
	// an AC outage can still find the site time source.
	peerNTP []string
	// pinnedAC is true when the controller address came from configuration
	// rather than discovery; only a discovered address may be forgotten and
	// re-resolved, so an operator-provided address is never overridden.
	pinnedAC bool
	// helloFailures and nextDiscover pace re-discovery. A discovered controller
	// that stops answering must be re-resolved, but broadcasting every 20s
	// would be pointless wake-ups on a healthy LAN.
	helloFailures int
	nextDiscover  time.Time
	// lastOwnNeighbors is the freshest 802.11k self-report, used to rebuild
	// neighbour lists from peers without another ubus read.
	lastOwnNeighbors []proto.NeighborBSS

	// steeringCache remembers which steering daemon the device reports as
	// enabled. Probing it runs three init scripts, so probing every heartbeat
	// was pure process churn.
	steeringCache string
	steeringAt    time.Time
	steeringKnown bool

	// steer is the policy-driven local steering loop.
	// Always present; it only acts when a package carried a steer-policy file.
	steer *SteerLoop

	// btm records whether steering suggestions actually work, shared by the
	// policy loop and the ad-hoc suggestion path. See btmguard.go.
	btm *btmGuard
	// btmResp consumes hostapd's BTM response events. Nil until Run, and nil
	// forever on a build that never starts the listener; the guard's inference
	// keeps working either way.
	btmResp *btmListener
	// adhocBTMs counts suggestions from the ad-hoc path, which runs outside the
	// policy loop. Only the cycle goroutine touches it, and reflectStatus reads
	// it from the same goroutine.
	adhocBTMs int

	// Cached across cycles: the 802.11k self-report only changes when a radio
	// is reconfigured, and re-reading 18 BSSes every 20s would be waste.
	neighborCache []proto.NeighborBSS
	// radioCache holds the last radio sample between collections. Airtime and
	// channel do not move at heartbeat speed, and each sample costs one ubus
	// call per radio.
	radioCache []proto.RadioStat
	// radioOffline is the control state the radio cache was last sampled under.
	// It exists only to detect the transition, which forces one fresh sample.
	radioOffline bool
	cycles       int

	// rt is the live self-description published to /var/run/ac-agent/status.json
	// for local consumers (LuCI). It is rebuilt from memory plus the persisted
	// state on every cycle, so it can never report a revision this AP did not
	// apply. See status.go.
	rt Status

	// applyFailures and nextApply implement backoff after a failed apply.
	// Without it a bad revision is retried every cycle, and each retry rewrites
	// network and reloads it — measurably worse than the bad config itself: a
	// real takeover failure here produced minutes of intermittent connectivity.
	applyFailures int
	nextApply     time.Time
}

// neighborRefreshCycles is how often the agent re-reads its own 11k reports and
// re-asserts the peer lists. At the default 20s interval that is five minutes:
// fast enough to recover from a reload, slow enough to be invisible.
const neighborRefreshCycles = 15

// neighborRefreshCyclesOffline stretches that while the controller is
// unreachable. The periodic pass exists for "hostapd forgot the lists after a
// reload", and an offline AP does not reload anything: no revision can arrive.
// A peer appearing or disappearing is handled separately, within one heartbeat,
// by restoreNeighborsOnPeerChange — so this slower pass changes no behaviour.
const neighborRefreshCyclesOffline = 30

// rediscoverAfterFailures and rediscoverDelay control re-resolution of a
// discovered controller. Three missed heartbeats is about a minute; re-reading
// the network faster than that only adds broadcast wake-ups.
const (
	rediscoverAfterFailures = 3
	rediscoverDelay         = 60 * time.Second
	// steeringProbeInterval caches the init-script probe. The enabled state of a
	// steering daemon changes only when an apply changes it, so probing three
	// init scripts every heartbeat was avoidable process churn.
	steeringProbeInterval = 5 * time.Minute
)

// radioSampleCycles is how often a fresh radio sample is taken. Radio state
// moves on the order of minutes, not seconds, and each sample costs one ubus
// call per radio.
const radioSampleCycles = 3

// radioSampleCyclesOffline stretches that while the controller is unreachable.
// Nothing consumes the sample then: it cannot be reported, and peer
// announcements carry only the BSS set (see PeerBus.Announce). The cache still
// refreshes on this slower cadence, so a local UI reading status.json never
// sees it frozen for good.
const radioSampleCyclesOffline = 15

// helloClientReuse is how old a client sample the heartbeat may reuse. It is
// deliberately longer than the steering loop's slowest cadence: the loop is the
// primary sampler, and the heartbeat rides along instead of duplicating it.
const helloClientReuse = 12 * time.Second

// New prepares an agent: loads state, resolves the identity and builds the
// applier pointing at this AP's /etc tree.
func New(cfg Config, log *slog.Logger) (*Agent, error) {
	cfg = Load(cfg)

	id := cfg.Identity
	detected := DetectIdentity(cfg.Root)
	if id.MAC == "" {
		id.MAC = detected.MAC
	}
	if id.Model == "" {
		id.Model = detected.Model
	}
	if id.Firmware == "" {
		id.Firmware = detected.Firmware
	}
	if id.Hostname == "" {
		id.Hostname = detected.Hostname
	}
	// A missing MAC is tolerated at construction so that `ac-agent status` works
	// against a copied /etc tree. Step() refuses to run without one, because the
	// controller identifies APs by MAC.

	st, err := LoadState(cfg.statePath())
	if err != nil {
		return nil, err
	}

	runner := cfg.Runner
	if runner == nil {
		runner = ExecRunner{}
	}

	a := &Agent{
		Config:   cfg,
		Identity: id,
		Log:      log,
		state:    st,
		rt: Status{
			PID:       os.Getpid(),
			StartedAt: time.Now().UTC().Format(time.RFC3339),
			Interval:  int(cfg.Interval / time.Second),
		},
		acAddr:   cfg.ACAddr,
		pinnedAC: cfg.ACAddr != "",
		Runner:   runner,
		Applier: &Applier{
			ConfigDir:     cfg.configDir(),
			StateDir:      cfg.stateDir(),
			Runner:        runner,
			Log:           log,
			HealthTimeout: cfg.HealthTimeout,
			DryRun:        cfg.DryRun,
			DisableDHCP:   !cfg.KeepDHCP,
			SteeringMode:  cfg.SteeringMode,
			ACAddr:        cfg.ACAddr,
		},
		Clients: &ClientCollector{
			Runner: runner, Log: log,
			Source: cfg.StationSource, Netlink: cfg.Netlink,
		},
		Neighbors: &NeighborCollector{Runner: runner, Log: log},
		Radios:    &RadioCollector{Runner: runner, Log: log},
		caps:      newCapsCollector(runner, log),
	}
	if cfg.Ubus {
		// One client for the whole process: the id cache and the breaker are
		// per-daemon state, and a second connection would double the lookups
		// and hide the first one's failures.
		a.ubus = newUbusClient(log)
		a.Radios.Ubus = a.ubus
		a.Neighbors.Ubus = a.ubus
	}
	if cfg.PeerPort > 0 {
		a.peerBus = NewPeerBusWithOptions(PeerOptions{
			Port:     cfg.PeerPort,
			Interval: cfg.PeerInterval,
			SelfMAC:  id.MAC,
			Log:      log,
			Secret:   cfg.Secret,
			Network:  cfg.PeerNetwork,
			Runner:   runner,
		})
	}
	a.btm = newBTMGuard()
	a.steer = NewSteerLoop()
	a.steer.Agent = a
	if st.SteerPolicyRaw != "" {
		if p, err := parseSteerPolicy([]byte(st.SteerPolicyRaw)); err == nil {
			a.steer.SetPolicy(p)
		} else {
			log.Warn("persisted steer-policy ignored", "err", err)
		}
	}
	if st.ClientBindingsRaw != "" {
		var bindings []proto.ClientBinding
		if err := json.Unmarshal([]byte(st.ClientBindingsRaw), &bindings); err == nil {
			a.steer.SetBindings(bindings)
		} else {
			log.Warn("persisted client bindings ignored", "err", err)
		}
	}
	if a.acAddr != "" {
		a.client = NewClient(withScheme(a.acAddr), cfg.Secret)
	}
	return a, nil
}

// State exposes a copy of the agent's persisted state for CLI status output.
func (a *Agent) State() State { return a.state }

// Run polls the controller until the context is cancelled. Transport failures
// are logged and retried: the whole design assumes the controller may vanish
// (design C3, §8.1 thin-standalone).
func (a *Agent) Run(ctx context.Context) error {
	interval := a.Config.Interval
	if interval <= 0 {
		interval = defaultInterval
	}

	a.Log.Info("agent starting",
		"mac", a.Identity.MAC,
		"model", a.Identity.Model,
		"hostname", a.Identity.Hostname,
		"ac", a.acAddr,
		"applied_rev", a.state.AppliedRev,
		"dry_run", a.Config.DryRun,
		"interval", interval.String(),
	)
	// Publish before the first cycle so a local UI shows "starting" instead of
	// mistaking a missing file for a dead agent.
	a.writeStatus()

	// Peer coordination is best effort: a port already in use must not stop
	// provisioning. The loud case is a second agent on the same host.
	if a.peerBus != nil {
		if err := a.peerBus.Start(ctx); err != nil {
			a.Log.Warn("peer coordination disabled, port unavailable", "port", a.peerBus.Port, "err", err)
			a.peerBus = nil
		} else {
			a.Log.Info("peer coordination listening", "port", a.peerBus.Port, "interval", a.peerBus.Interval.String())
			defer a.peerBus.Stop()
		}
	}

	// Real BTM responses turn the guard's inference into a measured outcome.
	// The listener is best effort: a missing socket or a hostapd build that does
	// not emit the event just leaves the inference in charge.
	a.btmResp = &btmListener{
		Dir:    filepath.Join(a.Config.Root, "var", "run", "hostapd"),
		Runner: a.Runner,
		Log:    a.Log,
		OnResponse: func(mac string, status int, target string) {
			a.btm.report(mac, status, target)
		},
	}
	a.btmResp.Start(ctx)
	defer a.btmResp.Close()

	// The steering loop blocks until a policy exists, so an AP without one
	// pays nothing but a goroutine.
	go a.steer.run(ctx)

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			a.Log.Info("agent stopping")
			return nil
		case <-timer.C:
		}

		if err := a.Step(ctx); err != nil && ctx.Err() == nil {
			a.Log.Warn("sync cycle failed", "err", err)
		}
		next := interval
		if d := a.helloBackoff(interval); d > next {
			next = d
		}
		timer.Reset(next)
	}
}

// helloBackoff returns how long to wait before the next heartbeat.
//
// A controller that is known to be gone does not get polled at the nominal rate
// forever: during a long outage every attempt is a wasted connection setup, and
// the AP-to-AP channel is what actually carries roaming in that window.
//
// The first rediscoverAfterFailures attempts deliberately keep the nominal
// cadence. Both the autonomous-steering fallback and re-discovery count
// consecutive failures, so stretching the early ones would delay the decision to
// go autonomous by exactly the same amount — trading a working offline mode for a
// few saved packets.
func (a *Agent) helloBackoff(base time.Duration) time.Duration {
	if base <= 0 {
		return base
	}
	excess := a.helloFailures - rediscoverAfterFailures
	if excess <= 0 {
		return base
	}
	limit := a.Config.HelloBackoffMax
	if limit <= 0 {
		limit = defaultHelloBackoffMax
	}
	d := time.Duration(1+excess) * base
	if d > limit {
		d = limit
	}
	// A cap below the nominal interval must never tighten the loop: during an
	// outage that would poll faster than a healthy one.
	if d < base {
		d = base
	}
	return d
}

// restoreNeighborsIfDue re-asserts the cached 11k peer lists on a slow cadence.
//
// hostapd keeps them in memory only, so any reload or reboot silently empties
// them, and this is the only thing that puts them back without waiting for the
// controller to push a revision — which may be weeks away. cycles==1 also covers
// one-shot mode (`ac-agent once`), which never reaches the periodic path.
//
// Only the first cycle gets the settle window: at boot every radio is doing ACS,
// so a single pass can miss most BSSes and a plain restore would not land until
// the next periodic pass, fifteen cycles later. The periodic refresh deliberately
// stays a single cheap pass — a BSS that never comes up must not be able to
// stall the heartbeat loop.
func (a *Agent) restoreNeighborsIfDue(ctx context.Context) {
	if a.Applier == nil {
		return
	}
	if period := a.neighborRefreshPeriod(); a.cycles != 1 && a.cycles%period != 0 {
		return
	}
	// The body is the controller's cached list merged with this AP's own
	// reports and every live peer. Without the peer merge, an AP that joins
	// while the controller is down would stay absent from every list until the
	// next revision, which may never come.
	body := a.neighborBody()
	if body == nil {
		return
	}
	// A reboot does not change the revision, so nothing else would ever notice
	// that hostapd came up serving no BSS at all. See Applier.EnsureWireless.
	a.Applier.EnsureNeighbors(ctx, body)
	if a.cycles == 1 {
		a.Applier.settleNeighbors(ctx, body)
	}
	// After the settle window, never before it: at boot the agent starts before
	// hostapd has built every BSS, and bringing the radios up inside that window
	// is exactly what interrupts the bring-up.
	if a.Applier.EnsureWireless(ctx, body) {
		a.Log.Info("no wireless BSS running, radios brought up")
		a.Applier.settleNeighbors(ctx, body)
	}
}

// reconcilePeerCadence tightens the AP-to-AP announcement while the controller
// is known to be unreachable.
//
// With the AC gone, peer announcements are the only thing keeping 11k lists and
// client awareness current across the site, so they run at the heartbeat's
// cadence instead of the relaxed 30 s. The traffic is a few KB per minute on the
// management LAN — far cheaper than the controller polls this replaces.
//
// Exactly one transition, decided from helloFailures, so a flapping controller
// cannot make the cadence oscillate every cycle: it moves only when the failure
// count crosses the same threshold that declares control lost.
func (a *Agent) reconcilePeerCadence() {
	if a.peerBus == nil {
		return
	}
	target := a.Config.PeerInterval
	if a.helloFailures >= rediscoverAfterFailures {
		if off := a.Config.PeerIntervalOffline; off > 0 {
			target = off
		}
	}
	if target <= 0 {
		return
	}
	a.peerBus.SetInterval(target)
}

// restoreNeighborsOnPeerChange reasserts the 11k lists as soon as a peer's BSS
// set changes.
//
// The periodic refresh runs every neighborRefreshCycles (five minutes). That is
// the right pace for "hostapd forgot the lists after a reload", but it is the
// wrong pace for "a peer just appeared": every existing AP would keep
// advertising a neighbour list that does not contain the new BSS for up to five
// minutes, so clients are not told about the only AP that could serve them.
// A rebooted peer has the same problem in reverse — its BSSID disappears from
// every list only when the refresh catches up.
//
// Only the BSS signature moves the version. A peer's client list changes
// constantly and feeds nothing that gets written to this AP.
func (a *Agent) restoreNeighborsOnPeerChange(ctx context.Context) {
	if a.peerBus == nil || a.Applier == nil {
		return
	}
	v := a.peerBus.Version()
	if !a.peerVersionSeen {
		// Opening snapshot: cycle 1 already established the lists through the
		// settle window, so reacting here would duplicate that work on every
		// boot. Recorded even when it is zero, so the next real change counts.
		a.peerVersionSeen = true
		a.peerVersion = v
		return
	}
	if a.peerVersion == v {
		return
	}
	a.peerVersion = v
	body := a.neighborBody()
	if body == nil {
		return
	}
	a.Log.Info("peer BSS set changed, reasserting 11k lists")
	a.Applier.EnsureNeighbors(ctx, body)
}

// Step performs one hello/apply cycle.
func (a *Agent) Step(ctx context.Context) error {
	if a.Identity.MAC == "" {
		return errors.New("agent: cannot determine local MAC address; pass --mac")
	}
	a.cycles++
	// One write per cycle even when nothing else changes: UpdatedAt is how a
	// local UI tells "agent running and idle" from "agent gone, file stale".
	defer a.writeStatus()
	// A management-address move that was never confirmed must be undone before
	// this cycle builds its hello, so the hello reports the restored address.
	a.checkPendingConfirm(ctx)
	a.reconcileFirmware()
	// Steering transitions use the previous cycle's outcome, so this sits
	// before the hello of the current cycle. See steering.go.
	a.reconcileSteering(ctx)
	// Same reasoning for the peer cadence: it follows the previous cycle's
	// outcome so the announcement below already uses it.
	a.reconcilePeerCadence()

	// Build the hello before contacting the controller: it also collects
	// telemetry, announces this AP to peers and refreshes the local 11k view.
	// All of that must keep happening while the controller is unreachable —
	// an AP that stops serving roaming data during an AC outage is exactly the
	// failure this design is meant to avoid.
	hello := a.buildHello(ctx)
	if len(a.state.NTPServers) == 0 && len(a.peerNTP) > 0 {
		if err := a.applyNTP(ctx, a.peerNTP); err != nil {
			a.Log.Warn("peer NTP list not applied", "err", err)
		}
	}
	a.restoreNeighborsIfDue(ctx)
	a.restoreNeighborsOnPeerChange(ctx)

	if a.client == nil {
		if time.Now().Before(a.nextDiscover) {
			a.Log.Debug("controller discovery deferred", "retry_at", a.nextDiscover)
			return nil
		}
		addr, offer, err := Discover(ctx, a.Config, a.Identity, a.Log)
		if err != nil {
			a.nextDiscover = time.Now().Add(rediscoverDelay)
			// Recorded like a failed hello: from an operator's point of view
			// "the broadcast got no answer" and "the address does not answer"
			// are the same question with different answers.
			a.rt.LastHelloOK = false
			a.rt.LastHelloError = err.Error()
			return err
		}
		a.acAddr = addr
		a.client = NewClient(withScheme(addr), a.Config.Secret)
		if a.Config.Secret == "" && offer.SecretRequired {
			secretErr := errors.New("controller requires a shared secret but AC_SECRET is empty")
			a.rt.LastHelloOK = false
			a.rt.LastHelloError = secretErr.Error()
			return secretErr
		}
	}

	reply, err := a.client.Hello(ctx, hello)
	if err != nil {
		// Record the failure before any early return so the status file and the
		// controller-facing view agree on why this AP is not under control.
		a.rt.LastHelloOK = false
		a.rt.LastHelloError = err.Error()

		// A rejected token will not fix itself; surface it instead of looping.
		if strings.Contains(err.Error(), "401") {
			return fmt.Errorf("%w (check AC_SECRET)", err)
		}
		// A discovered controller that vanished may have moved (DHCP, reinstall,
		// a different controller taking over). Forget its address and re-resolve
		// it, but at a pace that does not broadcast every heartbeat. A pinned
		// address is operator intent and is never forgotten.
		a.helloFailures++
		if !a.pinnedAC && a.helloFailures >= rediscoverAfterFailures {
			a.Log.Warn("controller unreachable, rediscovering", "failures", a.helloFailures,
				"retry_in", rediscoverDelay.String())
			a.client = nil
			a.acAddr = ""
			a.nextDiscover = time.Now().Add(rediscoverDelay)
		}
		return err
	}
	a.helloFailures = 0
	// A successful hello is the controller's acknowledgement that it can still
	// reach this AP after an address move; the watchdog can stand down.
	if a.state.PendingConfirm != nil {
		a.Log.Info("controller confirmed the new management address", "rev", a.state.PendingConfirm.Rev)
		a.state.PendingConfirm = nil
		if err := a.saveState(); err != nil {
			a.Log.Error("state save failed", "err", err)
		}
	}
	a.rt.LastHelloOK = true
	a.rt.LastHelloAt = time.Now().UTC().Format(time.RFC3339)
	a.rt.LastHelloError = ""
	a.rt.DesiredRev = reply.DesiredRev
	// Adoption follows the controller's latest answer, in both directions: a
	// device released on the controller must go back to "awaiting adoption"
	// instead of claiming to be managed forever. The one exception lives in
	// reflectStatus (a revision already applied on this AP is proof enough),
	// which covers a controller that lost its database.
	a.rt.Adopted = (reply.State == proto.APStateAdopted)

	// The report, if any, was delivered; clear it before doing more work.
	if a.state.PendingSync != "" {
		a.state.PendingSync = ""
		a.state.PendingSyncMsg = ""
		if err := a.saveState(); err != nil {
			return err
		}
	}
	// Same for action results: this hello carried them, and the reply below
	// already excludes the actions they belong to.
	a.clearDeliveredActionResults()

	// Binding intent is authoritative when present. An empty list clears the
	// previous set; nil means the controller is too old to know about bindings
	// and the AP keeps its persisted set.
	if reply.Bindings != nil {
		raw, err := json.Marshal(reply.Bindings)
		if err != nil {
			return err
		}
		if string(raw) != a.state.ClientBindingsRaw {
			a.state.ClientBindingsRaw = string(raw)
			a.steer.SetBindings(reply.Bindings)
			if err := a.saveState(); err != nil {
				return err
			}
		}
	}

	if err := a.pinController(reply); err != nil {
		// The controller answered, so this is not a reachability problem: it is
		// the trust-on-first-use pin refusing a changed key. Surfaced as an
		// error on the status card, because nothing will proceed until an
		// operator resolves it by hand.
		a.rt.LastHelloError = err.Error()
		return err
	}

	// Time sources are site-wide intent from the controller. Applying them is
	// best effort and independent of config revisions.
	a.applyNTPFromReply(ctx, reply)

	// Commands are independent of configuration revisions. A reboot stops the
	// cycle: nothing else should run on a box that is going down.
	reboot, err := a.runActions(ctx, reply.Actions)
	if err != nil {
		return err
	}
	if reboot {
		return nil
	}

	// A revision the controller offers is fetched whenever it differs from what
	// this AP applied. The older `>` rule assumed monotonic revisions, but a
	// controller restored from an older database advertises a lower rev, and an
	// AP that never downgrades then stays on stale config forever (observed on
	// 10.0.30.251: applied 9, controller desired 7, so the LAN4-6 BSS never
	// came back). Applying stops next cycle because state records the manifest
	// rev, so the loop converges instead of re-downloading.
	if reply.DesiredRev != a.state.AppliedRev && reply.PackagePath != "" {
		if wait := time.Until(a.nextApply); wait > 0 {
			a.Log.Warn("apply backing off after previous failure", "retry_in", wait.Round(time.Second).String())
		} else {
			a.Log.Info("pulling revision", "rev", reply.DesiredRev, "path", reply.PackagePath)
			if err := a.applyRevision(ctx, reply); err != nil {
				a.applyFailures++
				a.nextApply = time.Now().Add(applyBackoff(a.applyFailures))
				a.Log.Error("apply failed; backing off", "failures", a.applyFailures,
					"retry_in", applyBackoff(a.applyFailures).String())
				return err
			}
			a.applyFailures = 0
			a.nextApply = time.Time{}
		}
	} else {
		a.Log.Debug("nothing to do", "desired_rev", reply.DesiredRev, "applied_rev", a.state.AppliedRev)
	}

	// Firmware is a separate axis from configuration and is checked even when
	// there is no revision to apply.
	if reply.Firmware != nil {
		return a.upgradeFirmware(ctx, *reply.Firmware)
	}
	return nil
}

// applyBackoff grows 1m, 2m, 4m ... capped at one hour. A provisioning agent
// that hammers a broken configuration is more dangerous than one that waits.
func applyBackoff(failures int) time.Duration {
	const base = time.Minute
	const max = time.Hour
	d := base
	for i := 1; i < failures && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}

// buildHello assembles the payload sent on every cycle and after an apply.
// One builder for both paths: a report that omitted Steering once blanked the
// controller's view of it.
func (a *Agent) buildHello(ctx context.Context) proto.Hello {
	// Clients first: the radio sample attributes stations to the radio they are
	// on, and doing that from the same snapshot keeps the two consistent.
	clients := a.collectClients(ctx)
	hello := proto.Hello{
		MAC:           a.Identity.MAC,
		Model:         a.Identity.Model,
		Firmware:      a.Identity.Firmware,
		Hostname:      a.Identity.Hostname,
		AppliedRev:    a.state.AppliedRev,
		SyncState:     a.state.PendingSync,
		SyncMessage:   a.state.PendingSyncMsg,
		Steering:      a.steeringState(ctx),
		Caps:          a.caps.Collect(ctx),
		SteeringStats: a.steeringStats(),
		Clients:       clients,
		Neighbors:     a.collectNeighbors(ctx),
		Radios:        a.collectRadios(ctx, clients),
		Ports:         collectPorts(a.Config.Root),

		FirmwareState:   a.state.FirmwareState,
		FirmwareMessage: a.state.FirmwareMessage,
		Uptime:          systemUptime(a.Config.Root),
		Actions:         a.state.ActionResults,
	}
	// Announce to peers and, when enabled, make a local roaming suggestion.
	// Doing this here rather than after the HTTP call keeps peer coordination
	// alive on an offline cycle.
	a.coordinatePeers(ctx, hello)
	return hello
}

// steeringState returns the steering daemon the device has enabled, probing at
// most once per steeringProbeInterval. applyRevision invalidates the cache so a
// successful apply is reflected on the very next hello.
func (a *Agent) steeringState(ctx context.Context) string {
	if a.Applier == nil {
		return ""
	}
	if !a.steeringKnown || time.Since(a.steeringAt) >= steeringProbeInterval {
		a.steeringCache = a.Applier.probeSteering(ctx)
		a.steeringAt = time.Now()
		a.steeringKnown = true
	}
	return a.steeringCache
}

// steeringStats returns the guard's counters, or nil when this agent has none.
// Nil keeps the hello field absent, which the controller reads as "not
// reported" rather than as a real zero.
func (a *Agent) steeringStats() *proto.SteeringStat {
	if a.btm == nil {
		return nil
	}
	st := a.btm.stats()
	return &st
}

// collectRadios returns the radio sample, refreshing it every third cycle.
//
// At the default 20s heartbeat that is one sample per minute, which is what the
// controller stores anyway (AC_SAMPLE_INTERVAL); sampling faster would only make
// the AP spend ubus calls on data the controller throws away. cycles==1 covers
// one-shot mode, which never reaches a second cycle.
func (a *Agent) collectRadios(ctx context.Context, clients []proto.ClientStat) []proto.RadioStat {
	if a.Radios == nil {
		return nil
	}
	// Dropping the cache on every change of control state forces exactly one
	// fresh sample at the transition. Without it the period alone would leave
	// the first online report carrying an offline-era sample for up to a whole
	// offline period.
	period := radioSampleCycles
	if a.controlLost() {
		period = radioSampleCyclesOffline
		if !a.radioOffline {
			a.radioOffline = true
			a.radioCache = nil
		}
	} else if a.radioOffline {
		a.radioOffline = false
		a.radioCache = nil
	}
	if a.radioCache == nil || a.cycles%period == 0 {
		a.radioCache = a.Radios.Collect(ctx, clients)
	}
	return a.radioCache
}

// collectNeighbors returns this AP's own 802.11k self-description, cached between
// refreshes. The controller needs it to build every peer's list; without it a
// second AP would have nothing to advertise and 11k would stay silent.
func (a *Agent) collectNeighbors(ctx context.Context) []proto.NeighborBSS {
	if a.Neighbors == nil {
		return nil
	}
	if a.neighborCache == nil || a.cycles%a.neighborRefreshPeriod() == 0 {
		a.neighborCache = a.Neighbors.Collect(ctx)
	}
	return a.neighborCache
}

// neighborRefreshPeriod returns the cadence for the 11k self-report and the
// periodic list restore. Both are "hostapd forgot" repairs, so an offline AP —
// which cannot receive a revision and therefore never reloads — pays for them
// half as often.
func (a *Agent) neighborRefreshPeriod() int {
	if a.controlLost() {
		return neighborRefreshCyclesOffline
	}
	return neighborRefreshCycles
}

// collectClients returns the station list, or nil when telemetry is off or
// unreadable. nil is deliberate: the controller treats an absent list as "not
// reported" and leaves its client table alone.
func (a *Agent) collectClients(ctx context.Context) []proto.ClientStat {
	if a.Clients == nil {
		return nil
	}
	// The steering loop already samples the same stations every few seconds;
	// taking its sample keeps the telemetry and the steering decision
	// describing the same instant, at no extra process cost. 12 s covers the
	// loop's idle cadence with margin while staying well inside one heartbeat,
	// so a stopped loop can never serve a client list older than a cycle.
	return a.Clients.Snapshot(ctx, helloClientReuse)
}

// pinController trusts the first public key it sees and refuses any change.
// Swapping the key is exactly what an attacker on the path would do, so the
// agent treats it as a fatal condition rather than a warning.
func (a *Agent) pinController(reply proto.HelloReply) error {
	if reply.PubKey == "" {
		return errors.New("controller did not provide a public key")
	}
	if a.state.PubKey == "" {
		a.state.PubKey = reply.PubKey
		a.state.ACID = reply.ACID
		a.Log.Info("pinned controller key", "ac_id", reply.ACID, "pubkey", reply.PubKey)
		return a.saveState()
	}
	if a.state.PubKey != reply.PubKey {
		return fmt.Errorf("controller public key changed (pinned %s..., got %s...); refusing to apply",
			prefix(a.state.PubKey), prefix(reply.PubKey))
	}
	if reply.ACID != "" && a.state.ACID != "" && reply.ACID != a.state.ACID {
		return fmt.Errorf("controller ac_id changed (pinned %s, got %s)", a.state.ACID, reply.ACID)
	}
	return nil
}

func (a *Agent) applyRevision(ctx context.Context, reply proto.HelloReply) error {
	raw, err := a.client.FetchPackage(ctx, reply.PackagePath)
	if err != nil {
		return a.reportFailure(fmt.Errorf("download: %w", err))
	}

	pub, err := hex.DecodeString(a.state.PubKey)
	if err != nil {
		return a.reportFailure(fmt.Errorf("pinned public key is not hex: %w", err))
	}

	manifest, files, err := bundle.Extract(raw, pub)
	if err != nil {
		return a.reportFailure(fmt.Errorf("package rejected: %w", err))
	}

	// The package names its target explicitly; a mismatch means the controller
	// handed over the wrong file and must not be applied on trust.
	if manifest.DeviceMAC != "" && a.Identity.MAC != "" && manifest.DeviceMAC != a.Identity.MAC {
		return a.reportFailure(fmt.Errorf("package targets %s, this AP is %s", manifest.DeviceMAC, a.Identity.MAC))
	}
	if !manifest.AllowsModel(a.Identity.Model) {
		return a.reportFailure(fmt.Errorf("package does not allow model %q (whitelist %v)", a.Identity.Model, manifest.ModelWhitelist))
	}

	// The address may have come from discovery rather than the config, so hand
	// the live value to the applier for the renumbering reachability check.
	a.Applier.ACAddr = a.acAddr
	res, err := a.Applier.Apply(ctx, manifest, files)
	if err != nil {
		// The applier already rolled the device back to its previous config.
		a.Log.Error("apply failed and was rolled back", "rev", manifest.Rev, "err", err)
		a.state.LastError = err.Error()
		return a.reportFailure(err)
	}

	if a.Config.DryRun {
		a.Log.Info("dry run complete; device was not modified", "rev", manifest.Rev, "staged", res.Staged)
		return nil
	}

	a.state.AppliedRev = manifest.Rev
	// An address move is committed only when the controller acknowledges it.
	// Arm the watchdog before the immediate hello below: if that hello fails,
	// the window keeps running and only a later successful hello clears it.
	if res.NewAddr != "" {
		window := a.ConfirmWindow
		if window <= 0 {
			window = defaultConfirmWindow
		}
		a.state.PendingConfirm = &PendingConfirm{
			Rev:         manifest.Rev,
			BackupDir:   res.BackupDir,
			Deadline:    time.Now().Add(window).UTC().Format(time.RFC3339),
			RestoreDHCP: res.DHCPRestoreNeeded,
		}
		a.Log.Info("management address moved; waiting for the controller to confirm",
			"new_addr", res.NewAddr, "window", window.String())
	} else {
		a.state.PendingConfirm = nil
	}
	// The package may have changed the steering daemon; do not report the cached
	// answer on the hello sent right below.
	a.steeringKnown = false
	// Remember what the controller wants steering to be. This is the directive
	// the steering policy restores after an autonomous-steering outage, so it
	// must be the package's content, not the daemons actually found on disk.
	if raw, ok := files[render.FileSteering]; ok {
		if wanted := strings.TrimSpace(string(raw)); wanted != "" {
			a.state.Steering = wanted
		}
	}
	// The steering-loop policy rides in the same package. A malformed policy
	// must not fail the apply — the wireless config is still good — so it is
	// logged and dropped, and the controller sees the device running without
	// local steering on the next hello.
	if raw, ok := files[render.FileSteerPolicy]; ok {
		if p, err := parseSteerPolicy(raw); err != nil {
			a.Log.Warn("steer-policy rejected", "err", err)
		} else {
			a.steer.SetPolicy(p)
			a.state.SteerPolicyRaw = strings.TrimSpace(string(raw))
		}
	}
	// Record the archive digest, not a manifest field: it is what the controller
	// stored and what an operator can compare against on either side.
	sum := sha256.Sum256(raw)
	a.state.SHA256 = hex.EncodeToString(sum[:])
	a.state.AppliedAt = time.Now().UTC().Format(time.RFC3339)
	a.state.LastError = ""
	a.state.PendingSync = proto.SyncApplied
	a.state.PendingSyncMsg = fmt.Sprintf("rev %d, files %v", manifest.Rev, res.Files)
	if err := a.saveState(); err != nil {
		return err
	}

	// Report immediately; waiting a full interval would leave the controller
	// showing a stale state for no reason. buildHello is reused so the report
	// cannot silently drop the steering state and client list the first hello
	// carried — that is exactly the bug this helper was introduced to prevent.
	if _, err := a.client.Hello(ctx, a.buildHello(ctx)); err != nil {
		a.Log.Warn("could not report applied revision yet, will retry", "err", err)
		return nil // the pending report survives on disk and goes out next cycle
	}

	a.state.PendingSync = ""
	a.state.PendingSyncMsg = ""
	if a.state.PendingConfirm != nil {
		a.Log.Info("controller confirmed the new management address", "rev", a.state.PendingConfirm.Rev)
		a.state.PendingConfirm = nil
	}
	return a.saveState()
}

// defaultConfirmWindow is how long a management-address move may stay
// unacknowledged before the watchdog restores the previous configuration.
const defaultConfirmWindow = 5 * time.Minute

// checkPendingConfirm is the commit-confirm half of a management-address move.
// The apply already proved once that the new address can reach the controller;
// this requires the real proof, a hello the controller answered. If that never
// happens — asymmetric routing, a VLAN that only the controller can reach, a
// dropped route after the apply — the old configuration comes back and the
// operator keeps an AP instead of losing one.
func (a *Agent) checkPendingConfirm(ctx context.Context) {
	pc := a.state.PendingConfirm
	if pc == nil || a.Config.DryRun {
		return
	}
	deadline, err := time.Parse(time.RFC3339, pc.Deadline)
	if err != nil {
		a.Log.Warn("pending confirm has an invalid deadline; dropping it", "deadline", pc.Deadline, "err", err)
		a.state.PendingConfirm = nil
		if err := a.saveState(); err != nil {
			a.Log.Error("state save failed", "err", err)
		}
		return
	}
	if time.Now().Before(deadline) {
		return
	}

	a.Log.Warn("management address was never confirmed by the controller; restoring the previous config",
		"rev", pc.Rev, "deadline", pc.Deadline, "backup", pc.BackupDir)
	if err := a.Applier.RestoreBackup(ctx, pc.BackupDir, pc.RestoreDHCP); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			a.Log.Error("pending confirm cannot be restored: backup is gone", "backup", pc.BackupDir, "err", err)
			a.state.PendingConfirm = nil
			a.state.LastError = "management address was not confirmed and its backup is gone"
			a.state.PendingSync = proto.SyncRollback
			a.state.PendingSyncMsg = truncate(a.state.LastError, 500)
			if err := a.saveState(); err != nil {
				a.Log.Error("state save failed", "err", err)
			}
			return
		}
		// Transient failure (write, reload): keep the pending record and try
		// again on the next cycle rather than leaving the AP half-restored.
		a.Log.Error("watchdog restore failed; will retry next cycle", "err", err)
		return
	}

	a.state.PendingConfirm = nil
	a.state.LastError = fmt.Sprintf("rev %d moved the management address and was never confirmed; rolled back", pc.Rev)
	a.state.PendingSync = proto.SyncRollback
	a.state.PendingSyncMsg = truncate(a.state.LastError, 500)
	if err := a.saveState(); err != nil {
		a.Log.Error("state save failed after watchdog revert", "err", err)
	}
}

// reportFailure records the outcome so the controller learns about it even if
// the connection drops right now.
func (a *Agent) reportFailure(cause error) error {
	a.state.LastError = cause.Error()
	state := proto.SyncRollback
	if a.Config.DryRun {
		state = proto.SyncIdle
	}
	a.state.PendingSync = state
	a.state.PendingSyncMsg = truncate(cause.Error(), 500)
	if err := a.saveState(); err != nil {
		return err
	}
	return cause
}

func (a *Agent) saveState() error {
	return SaveState(a.Config.statePath(), a.state)
}

func withScheme(addr string) string {
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return addr
	}
	return "http://" + addr
}

func prefix(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
