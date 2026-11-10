package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/naoki66/ac-controller/shared/bundle"
	"github.com/naoki66/ac-controller/shared/render"
	"github.com/naoki66/ac-controller/shared/ucimerge"
)

// Applier performs the atomic part of provisioning: stage, back up, write,
// reload, verify, and roll back if verification fails (design §5 D3 step 1-6).
type Applier struct {
	ConfigDir string
	StateDir  string
	Runner    Runner
	Log       *slog.Logger
	// Sleep, when set, replaces time.Sleep. Tests use it to skip retry backoff.
	Sleep func(time.Duration)

	HealthTimeout time.Duration
	PollInterval  time.Duration
	// NeighbourSettle bounds how long the agent keeps re-asserting the 802.11k
	// lists after an apply, waiting for BSSes that are still coming up. Zero
	// means the default window. See settleNeighbors.
	NeighbourSettle time.Duration

	// WirelessRetryEvery bounds how often the agent may bring the radios back
	// up. A reboot that leaves wireless down must be recovered at once; a radio
	// that is genuinely broken must not be restarted every cycle.
	WirelessRetryEvery time.Duration
	// wirelessRetry is the next time a recovery attempt is allowed.
	wirelessRetry time.Time

	// DryRun renders and stages the merged files under StateDir/staging without
	// touching ConfigDir and without running any command. This is how the
	// pipeline is rehearsed before it ever runs on a live AP.
	DryRun bool

	// DisableDHCP stops and disables dnsmasq/odhcpd after a successful apply.
	// An AP in bridged mode must never hand out addresses: a rogue DHCP server
	// on the segment breaks the whole site, not just this AP (design §2.3).
	DisableDHCP bool

	// ACAddr is the controller address as host:port. When set, a network apply
	// that moves the management address may still pass if one of the new static
	// addresses can reach the controller; see waitForAddrsAfterNetwork.
	ACAddr string
	// DialAC proves that a candidate source address can reach the controller.
	// Tests replace it; nil means a real TCP dial from src to ACAddr.
	DialAC func(ctx context.Context, src net.IP, target string) error

	// SteeringMode gates the steering file. In autonomous mode the local
	// daemons belong to the operator, so the controller's directive is reported
	// but never enforced. Empty means ac_priority.
	SteeringMode string

	Now func() time.Time

	// bssMu guards the cached hostapd BSS set, which is valid for exactly one
	// sweep: reading it once per batch turns eleven `ubus list` calls into one,
	// while never letting an answer outlive the check that asked for it.
	bssMu  sync.Mutex
	bssSet map[string]bool
}

// ApplyResult reports what the applier actually did.
type ApplyResult struct {
	Rev int
	// Files lists the config files that changed. Empty means the device was
	// already running the desired state.
	Files     []string
	BackupDir string
	Staged    []string
	// NewAddr is the static address that replaced a lost management address.
	// Empty means no address moved.
	NewAddr string
	// DHCPRestoreNeeded records that this apply turned DHCP/DNS off although it
	// was running before. A watchdog revert must put it back, or the restored
	// routed-mode config would come up without a DHCP server.
	DHCPRestoreNeeded bool
}

// Apply writes one revision. It is safe to call with a revision that matches
// the current on-disk configuration: nothing is written and no reload happens.
func (a *Applier) Apply(ctx context.Context, m bundle.Manifest, files map[string][]byte) (ApplyResult, error) {
	// hostapd is reloaded somewhere below, so any cached BSS set now describes
	// an instance that is about to disappear.
	a.invalidateBSSSet()

	wireless, ok := files["wireless"]
	if !ok {
		return ApplyResult{}, errors.New("apply: package has no wireless file")
	}
	network := files["network"] // optional

	curWireless, err := readFileOrEmpty(filepath.Join(a.ConfigDir, "wireless"))
	if err != nil {
		return ApplyResult{}, err
	}
	curNetwork, err := readFileOrEmpty(filepath.Join(a.ConfigDir, "network"))
	if err != nil {
		return ApplyResult{}, err
	}

	// Local sections are disabled rather than deleted so a fat-mode rollback
	// stays a one-line change (design §8.1).
	mergedWireless := ucimerge.Merge(curWireless, string(wireless), ucimerge.Options{
		DisableLocal: true,
		DisableTypes: []string{"wifi-device", "wifi-iface"},
	})

	// An empty network file means the controller does not manage layer 2 for
	// this device: leave the file untouched rather than rewriting it identical.
	//
	// When it does manage it, local sections have to go rather than be disabled:
	// `config device` and `config bridge-vlan` have no `disabled` option, and the
	// deployed APs define their bridge in an anonymous `config device` section.
	// Keeping it alongside the controller's own bridge would give netifd two
	// devices called br-lan.
	//
	// `config interface` is not in that list on purpose. The controller renders
	// it under the device's own name and the merge replaces it in place; listing
	// the type here instead would also retire interfaces the controller knows
	// nothing about, including an uplink.
	//
	// The one exception is an interface bound to a port the incoming bridge
	// enslaves: a factory `wan` (proto dhcp) cannot survive next to a br-lan that
	// owns the wan port, and leaving it behind keeps a dead routing-mode uplink
	// (observed on 10.0.10.250 after the first adoption). Such sections are
	// dropped by name-of-device, not by type, so every unrelated interface
	// survives.
	mergedNetwork := curNetwork
	if len(network) > 0 {
		mergedNetwork = ucimerge.Merge(curNetwork, string(network), ucimerge.Options{
			ReplaceTypes: []string{"device", "bridge-vlan"},
			// loopback is a config interface too; dropping it would take the
			// device's own stack down.
			ProtectNames:                []string{"loopback"},
			DropInterfacesOnBridgePorts: true,
		})
	}

	// Refuse to write anything that does not look like genuine controller
	// output: a malformed file is indistinguishable from an outage.
	if err := ucimerge.Validate(mergedWireless); err != nil {
		return ApplyResult{}, fmt.Errorf("apply: %w", err)
	}
	if len(network) > 0 {
		if err := ucimerge.Validate(mergedNetwork); err != nil {
			return ApplyResult{}, fmt.Errorf("apply: %w", err)
		}
	}

	pending := map[string]string{}
	if mergedWireless != curWireless {
		pending[filepath.Join(a.ConfigDir, "wireless")] = mergedWireless
	}
	networkChanged := len(network) > 0 && mergedNetwork != curNetwork
	if networkChanged {
		pending[filepath.Join(a.ConfigDir, "network")] = mergedNetwork
	}

	res := ApplyResult{Rev: m.Rev}
	if len(pending) == 0 {
		// UCI is already current, but the 11k peer lists live in hostapd memory
		// and may have been lost to a reload since the last apply.
		if nb, ok := files[render.FileNeighbors]; ok {
			a.cacheNeighbors(nb)
			a.EnsureNeighbors(ctx, nb)
		}
		if len(network) > 0 {
			a.ensureManagementAccess(ctx, mergedNetwork)
		}
		a.Log.Info("config already up to date, nothing to write", "rev", m.Rev)
		return res, nil
	}
	for path := range pending {
		res.Files = append(res.Files, filepath.Base(path))
	}
	sort.Strings(res.Files)

	if a.DryRun {
		return a.stage(m, pending, res)
	}

	backupDir, err := a.backup(m, []string{
		filepath.Join(a.ConfigDir, "wireless"),
		filepath.Join(a.ConfigDir, "network"),
	})
	if err != nil {
		return res, err
	}
	res.BackupDir = backupDir

	// Snapshot the addresses before touching network, so the health check can
	// prove the box is still reachable afterwards.
	var addrsBefore []string
	if networkChanged {
		addrsBefore = a.localIPv4Addrs(ctx)
	}

	if err := writeAll(pending); err != nil {
		return res, err
	}

	if err := a.reload(ctx, pending); err != nil {
		a.rollback(ctx, backupDir, pending)
		return res, fmt.Errorf("apply: reload failed: %w", err)
	}

	// After a network change, the bridge VLAN devices are rebuilt asynchronously.
	// hostapd must not be asked to start until they are back.
	if networkChanged {
		if err := a.waitForNetdevs(ctx, ucimerge.Netdevs(mergedNetwork)); err != nil {
			a.rollback(ctx, backupDir, pending)
			return res, fmt.Errorf("apply: %w", err)
		}
	}

	// The steering file drives daemons only when the controller is allowed to:
	// in autonomous mode the operator owns usteer/dawn and the directive is
	// reported to the AC but not enforced here.
	if steering, ok := files[render.FileSteering]; ok && a.SteeringMode != SteeringModeAutonomous {
		a.applySteering(ctx, strings.TrimSpace(string(steering)))
	}

	expected := ucimerge.ACWifiInterfaces(mergedWireless)
	if err := a.waitForBSS(ctx, expected); err != nil {
		a.rollback(ctx, backupDir, pending)
		return res, fmt.Errorf("apply: health check failed: %w", err)
	}

	// Mesh 节点即使保留本机地址和 BSS，也可能已经丢失上联，因此需额外探测控制器。
	if hasMeshBackhaul(curWireless) || hasMeshBackhaul(mergedWireless) {
		if err := a.waitForMeshController(ctx); err != nil {
			a.rollback(ctx, backupDir, pending)
			return res, fmt.Errorf("apply: mesh health check failed: %w", err)
		}
	}

	// A network change must not cost an address. BSSes can be up while the
	// management IP is gone, which would pass the check above and lock the
	// operator out of the device. A deliberate renumbering passes only when a
	// new static address from this package can reach the controller.
	if networkChanged {
		newAddr, err := a.waitForAddrsAfterNetwork(ctx, addrsBefore, []byte(mergedNetwork))
		if err != nil {
			a.rollback(ctx, backupDir, pending)
			return res, fmt.Errorf("apply: network health check failed: %w", err)
		}
		res.NewAddr = newAddr
	}

	// Only after every fallible check: disabling DHCP commits config and stops
	// services that rollback does not restore. Stopping them before the checks
	// left a rolled-back AP on its factory routed network with no DHCP server,
	// so clients connected but never got an address — the outage observed after
	// a failed takeover on a factory-reset XR1710G.
	dhcpWasOff := a.dhcpWasDisabled(ctx)
	a.disableDHCPServices(ctx)
	res.DHCPRestoreNeeded = a.DisableDHCP && !dhcpWasOff

	// The template puts a management address on lan2..lan6, but the stock
	// firewall only trusts br-lan.1 and dropbear is bound to the `lan` interface.
	// Without this the AP comes up answering ARP on 10.10.20.250 while every
	// connection to it is refused; see ensureManagementAccess.
	if len(network) > 0 {
		a.ensureManagementAccess(ctx, mergedNetwork)
	}

	// After the health check, not merely after the reload.
	//
	// Reloading wireless tears down and rebuilds hostapd for each radio, and a
	// BSS that comes up later starts with an empty neighbour list. Setting the
	// lists right after `wifi reload` returns therefore loses whichever radios
	// finished last — observed on a real XR1710G, where the 2.4 GHz BSS ended up
	// empty while 5 and 6 GHz kept theirs. Once every expected BSS is up, the
	// lists set here stick.
	if nb, ok := files[render.FileNeighbors]; ok {
		a.cacheNeighbors(nb)
		a.EnsureNeighbors(ctx, nb)
		// Not every BSS is serviced yet even though the health check passed:
		// a radio doing ACS reports up well before its BSS is enabled. Come
		// back for those instead of waiting for the periodic refresh.
		a.settleNeighbors(ctx, nb)
		// Only after that window: `wifi reload` cannot bring up radios that are
		// not running at all, and an apply must not report success while
		// hostapd serves nothing. EnsureWireless only acts when every expected
		// BSS is still missing, so a normal apply never reaches it.
		if a.EnsureWireless(ctx, nb) {
			a.settleNeighbors(ctx, nb)
		}
	}

	a.Log.Info("revision applied", "rev", m.Rev, "files", res.Files, "bss", len(expected), "backup", backupDir)
	return res, nil
}

// stage writes the merged output to a scratch directory for inspection.
func (a *Applier) stage(m bundle.Manifest, pending map[string]string, res ApplyResult) (ApplyResult, error) {
	dir := filepath.Join(a.StateDir, "staging", fmt.Sprintf("rev%d", m.Rev))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, fmt.Errorf("apply: create staging dir: %w", err)
	}
	write := map[string]string{}
	for path, content := range pending {
		write[filepath.Join(dir, filepath.Base(path))] = content
	}
	if err := writeAll(write); err != nil {
		return res, err
	}
	for path := range write {
		res.Staged = append(res.Staged, path)
	}
	sort.Strings(res.Staged)
	a.Log.Info("dry run: staged merged config", "rev", m.Rev, "dir", dir)
	return res, nil
}

// backup copies the current config files to a revision-tagged directory. Missing
// files are recorded as missing so rollback knows to delete rather than restore.
func (a *Applier) backup(m bundle.Manifest, paths []string) (string, error) {
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	dir := filepath.Join(a.StateDir, "backup", fmt.Sprintf("rev%d-%d", m.Rev, now().Unix()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("apply: create backup dir: %w", err)
	}

	var manifestLines []string
	for _, path := range paths {
		body, err := os.ReadFile(path)
		name := filepath.Base(path)
		if os.IsNotExist(err) {
			manifestLines = append(manifestLines, name+"\tmissing")
			continue
		}
		if err != nil {
			return "", fmt.Errorf("apply: read %s for backup: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			return "", fmt.Errorf("apply: write backup %s: %w", name, err)
		}
		manifestLines = append(manifestLines, name+"\tpresent")
	}
	if err := os.WriteFile(filepath.Join(dir, "backup.txt"), []byte(strings.Join(manifestLines, "\n")+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("apply: write backup manifest: %w", err)
	}
	return dir, nil
}

// reload triggers exactly one wifi reload. Design §7.1 discipline: the bridge is
// never torn down here, because rebuilding it flushes the PPE table and shows up
// as several seconds of throughput collapse.
func (a *Applier) reload(ctx context.Context, pending map[string]string) error {
	networkChanged := false
	for path := range pending {
		if filepath.Base(path) == "network" {
			networkChanged = true
		}
	}
	if networkChanged {
		if _, err := a.Runner.Run(ctx, "ubus", "call", "network", "reload"); err != nil {
			return err
		}
	}
	_, err := a.Runner.Run(ctx, "wifi", "reload")
	return err
}

// wirelessRetryEvery is the minimum gap between two recovery attempts. Five
// minutes: long enough that a broken radio is not restarted every cycle, short
// enough that a reboot which left wireless down is recovered on the first cycle.
const wirelessRetryEvery = 5 * time.Minute

// EnsureWireless brings the radios back when hostapd is serving no BSS at all.
// It reports whether it ran `wifi up`.
//
// Why this exists: a reboot can finish with hostapd running but serving nothing.
// Measured twice on an XR1710G managed by this controller — every BSS reached
// AP-ENABLED during boot, then all eleven were torn down again and never came
// back. Nothing else recovers it: a reboot does not change the revision, so the
// agent never reloads wireless, and the periodic refresh only writes 11k lists
// into BSSes it assumes are there. The AP looks healthy to the controller and
// serves no clients until someone notices.
//
// `wifi reload` does not fix that state — it leaves hostapd without BSSes;
// `wifi up` does, in roughly fifteen seconds.
//
// The trigger is deliberately narrow: only when *every* expected BSS is absent.
// One slow radio, or one broken BSS out of eleven, must never cause a reload
// that would disconnect every client on the AP.
func (a *Applier) EnsureWireless(ctx context.Context, body []byte) bool {
	if a.DryRun {
		// Bringing the radios up disconnects every client on the AP. A dry run
		// is a rehearsal and must never do that, however broken wireless looks.
		return false
	}
	want, err := decodeNeighbors(body)
	if err != nil || len(want) == 0 {
		return false
	}
	// This decision runs `wifi up`, which disconnects every client on the AP, so
	// it must rest on the live set rather than on whatever the neighbour sweep
	// happened to cache a moment ago.
	a.invalidateBSSSet()
	for ifname := range want {
		if a.hasInterface(ctx, ifname) {
			return false
		}
	}

	every := a.WirelessRetryEvery
	if every == 0 {
		every = wirelessRetryEvery
	}
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	if !a.wirelessRetry.IsZero() && now.Before(a.wirelessRetry) {
		a.Log.Debug("wireless recovery throttled", "next", a.wirelessRetry)
		return false
	}
	a.wirelessRetry = now.Add(every)

	a.Log.Warn("no wireless BSS present, bringing the radios up", "expected", len(want))
	if _, err := a.Runner.Run(ctx, "wifi", "up"); err != nil {
		a.Log.Warn("wifi up failed", "err", err)
		return false
	}
	return true
}

// waitForNetdevs blocks until every device an interface is built on exists.
//
// `ubus call network reload` returns as soon as netifd has accepted the
// configuration, not when the bridge VLAN devices are back. Asking wifi to
// reload inside that window makes hostapd attach its BSSes to devices that do
// not exist yet; they come up `down` and never recover. That is exactly how a
// real takeover failed on an XR1710G: 10 of 11 BSSes stayed down and the apply
// rolled back.
func (a *Applier) waitForNetdevs(ctx context.Context, devs []string) error {
	if len(devs) == 0 {
		return nil
	}
	timeout := a.HealthTimeout
	if timeout == 0 {
		timeout = defaultHealthTimeout
	}
	poll := a.PollInterval
	if poll == 0 {
		poll = 500 * time.Millisecond
	}

	deadline := time.Now().Add(timeout)
	for {
		var missing []string
		for _, d := range devs {
			if _, err := a.Runner.Run(ctx, "ip", "link", "show", d); err != nil {
				missing = append(missing, d)
			}
		}
		if len(missing) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("netdev(s) not present after network reload: %s", strings.Join(missing, ", "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// waitForBSS polls `ubus call network.wireless status` until every expected
// managed BSS reports up, or the timeout expires.
func (a *Applier) waitForBSS(ctx context.Context, expected []string) error {
	if len(expected) == 0 {
		return errors.New("no managed BSS in rendered config")
	}

	timeout := a.HealthTimeout
	if timeout == 0 {
		timeout = defaultHealthTimeout
	}
	poll := a.PollInterval
	if poll == 0 {
		poll = 500 * time.Millisecond
	}

	deadline := time.Now().Add(timeout)
	var missing []string
	for {
		out, err := a.Runner.Run(ctx, "ubus", "call", "network.wireless", "status")
		if err == nil {
			up, perr := parseWirelessStatus(out)
			if perr == nil {
				missing = missingBSS(expected, up)
				if len(missing) == 0 {
					return nil
				}
			} else {
				missing = []string{"unparseable ubus status: " + perr.Error()}
			}
		} else {
			missing = []string{"ubus call failed: " + err.Error()}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("BSS not up within %s: %s", timeout, strings.Join(missing, ", "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// dhcpWasDisabled reports whether the DHCP server was already switched off
// before this apply. The watchdog uses it to avoid turning DHCP back on for a
// managed bridge AP that never had it.
func (a *Applier) dhcpWasDisabled(ctx context.Context) bool {
	if !a.DisableDHCP {
		return true
	}
	out, err := a.Runner.Run(ctx, "uci", "-q", "get", "dhcp.@dnsmasq[0].disabled")
	return err == nil && strings.TrimSpace(out) == "1"
}

// disableDHCPServices makes sure this AP is a pure layer-2 device: no DHCP
// server, no DNS service. The deployed baseline already runs this way (design
// §2.3); enforcing it here means a factory-reset AP cannot come back up handing
// out 192.168.1.x addresses in the middle of the site.
//
// Failures are logged but never fail the apply: a wifi config that works must not
// be rolled back because odhcpd happened to be absent.
func (a *Applier) disableDHCPServices(ctx context.Context) {
	if !a.DisableDHCP {
		return
	}

	steps := [][]string{
		// Config-level, so the setting survives a reboot even if the service is
		// later re-enabled by an upgrade.
		{"uci", "-q", "set", "dhcp.@dnsmasq[0].disabled=1"},
		{"uci", "-q", "set", "dhcp.odhcpd.disabled=1"},
		{"uci", "-q", "commit", "dhcp"},
		// Service-level, so it stops right now rather than at the next boot.
		{"/etc/init.d/dnsmasq", "stop"},
		{"/etc/init.d/dnsmasq", "disable"},
		{"/etc/init.d/odhcpd", "stop"},
		{"/etc/init.d/odhcpd", "disable"},
	}

	disabled := 0
	for _, step := range steps {
		if _, err := a.Runner.Run(ctx, step[0], step[1:]...); err != nil {
			// Absent service or missing package: log at debug, keep going.
			a.Log.Debug("dhcp shutdown step skipped", "cmd", strings.Join(step, " "), "err", err)
			continue
		}
		if len(step) > 1 && (step[1] == "disable") {
			disabled++
		}
	}
	a.Log.Info("dhcp/dns services disabled on this AP", "services_disabled", disabled)
}

// enableDHCPServices undoes disableDHCPServices. The address-move watchdog needs
// it: restoring a routed-mode config without its DHCP server would trade an
// unreachable AP for clients that associate and never get an address.
func (a *Applier) enableDHCPServices(ctx context.Context) {
	steps := [][]string{
		{"uci", "-q", "set", "dhcp.@dnsmasq[0].disabled=0"},
		{"uci", "-q", "set", "dhcp.odhcpd.disabled=0"},
		{"uci", "-q", "commit", "dhcp"},
		{"/etc/init.d/dnsmasq", "enable"},
		{"/etc/init.d/dnsmasq", "start"},
		{"/etc/init.d/odhcpd", "enable"},
		{"/etc/init.d/odhcpd", "start"},
	}
	for _, step := range steps {
		if _, err := a.Runner.Run(ctx, step[0], step[1:]...); err != nil {
			a.Log.Debug("dhcp restore step skipped", "cmd", strings.Join(step, " "), "err", err)
		}
	}
	a.Log.Info("dhcp/dns services restored for the pre-apply network")
}

// ensureManagementAccess makes the AP's own management addresses usable. Two
// device defaults defeat a multi-VLAN template even after the addresses are
// applied correctly:
//
//   - the stock firewall zone only lists `lan` (br-lan.1). lan2..lan6 belong to
//     no zone and fall through to the default REJECT policy, so the AP answers
//     ARP on 10.10.20.250 while ICMP/SSH/HTTP are refused.
//   - dropbear is started with `DirectInterface 'lan'` and therefore listens on
//     br-lan.1 only; SSH is refused on every other management address.
//
// Both are device-local settings the controller does not render, so they are
// repaired after a successful apply. Failures are logged, never fatal: a stale
// firewall rule must not roll back an otherwise correct wifi configuration.
//
// Observed on a factory-reset XR1710G: 10.10.20.250 answered ARP but nothing
// else until these two settings were fixed by hand.
func (a *Applier) ensureManagementAccess(ctx context.Context, networkConfig string) {
	if a.DryRun {
		return
	}
	mgmt := ucimerge.ManagementInterfaces(networkConfig)
	if len(mgmt) == 0 {
		return
	}
	a.ensureFirewallZone(ctx, mgmt)
	a.ensureDropbearBinding(ctx, mgmt)
}

// ensureFirewallZone adds every management interface to the zone that already
// carries `lan`. Adding the uplink is deliberately impossible here: only
// br-lan based static interfaces reach this list.
func (a *Applier) ensureFirewallZone(ctx context.Context, mgmt []string) {
	out, err := a.Runner.Run(ctx, "uci", "-q", "show", "firewall")
	if err != nil {
		a.Log.Debug("firewall: cannot read config", "err", err)
		return
	}

	var zone *uciShowSection
	sections := parseUCIShow(out)
	for i := range sections {
		s := &sections[i]
		if s.typ != "zone" {
			continue
		}
		if valueIn(s.opts["network"], "lan") {
			zone = s
			break
		}
		if zone == nil && valueIn(s.opts["name"], "lan") {
			zone = s
		}
	}
	if zone == nil {
		a.Log.Warn("firewall: no lan zone found, management access left untouched")
		return
	}

	var missing []string
	for _, name := range mgmt {
		if !valueIn(zone.opts["network"], name) {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return
	}
	for _, name := range missing {
		if _, err := a.Runner.Run(ctx, "uci", "-q", "add_list", zone.key+".network="+name); err != nil {
			a.Log.Debug("firewall: add_list failed", "interface", name, "err", err)
			return
		}
	}
	if _, err := a.Runner.Run(ctx, "uci", "-q", "commit", "firewall"); err != nil {
		a.Log.Debug("firewall: commit failed", "err", err)
		return
	}
	if _, err := a.Runner.Run(ctx, "/etc/init.d/firewall", "reload"); err != nil {
		a.Log.Debug("firewall: reload failed", "err", err)
	}
	a.Log.Info("management interfaces added to the lan firewall zone", "interfaces", missing)
}

// ensureDropbearBinding removes a single-interface binding (the stock
// `DirectInterface 'lan'`) when it does not cover every management interface.
// Without an Interface/DirectInterface option dropbear listens on all addresses,
// and the firewall still decides what the uplink may reach.
func (a *Applier) ensureDropbearBinding(ctx context.Context, mgmt []string) {
	out, err := a.Runner.Run(ctx, "uci", "-q", "show", "dropbear")
	if err != nil {
		a.Log.Debug("dropbear: cannot read config", "err", err)
		return
	}

	var changed []string
	for _, s := range parseUCIShow(out) {
		if s.typ != "dropbear" {
			continue
		}
		for _, opt := range []string{"DirectInterface", "Interface"} {
			vals := s.opts[opt]
			if len(vals) == 0 || coversAll(vals, mgmt) {
				continue
			}
			key := s.key + "." + opt
			if _, err := a.Runner.Run(ctx, "uci", "-q", "delete", key); err != nil {
				a.Log.Debug("dropbear: delete failed", "option", key, "err", err)
				continue
			}
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		return
	}
	if _, err := a.Runner.Run(ctx, "uci", "-q", "commit", "dropbear"); err != nil {
		a.Log.Debug("dropbear: commit failed", "err", err)
		return
	}
	if _, err := a.Runner.Run(ctx, "/etc/init.d/dropbear", "restart"); err != nil {
		a.Log.Debug("dropbear: restart failed", "err", err)
	}
	a.Log.Info("dropbear interface binding removed for management access", "options", changed)
}

// uciShowSection is one section of `uci show <package>` output. key is the
// section identifier as uci wants it back (firewall.@zone[0], dropbear.main).
type uciShowSection struct {
	key  string
	typ  string
	opts map[string][]string
}

// parseUCIShow understands the flattened form uci prints, where a list option
// arrives as `key.option='value' 'value'` and a section as `key=type`.
func parseUCIShow(out string) []uciShowSection {
	var sections []uciShowSection
	index := map[string]int{}
	section := func(key string) *uciShowSection {
		if i, ok := index[key]; ok {
			return &sections[i]
		}
		sections = append(sections, uciShowSection{key: key, opts: map[string][]string{}})
		index[key] = len(sections) - 1
		return &sections[len(sections)-1]
	}

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key, raw := line[:eq], strings.TrimSpace(line[eq+1:])
		if !strings.HasPrefix(raw, "'") && !strings.HasPrefix(raw, `"`) {
			// Section header: pkg.name=type
			s := section(key)
			s.typ = strings.Trim(raw, `'"`)
			continue
		}
		dot := strings.LastIndexByte(key, '.')
		if dot <= 0 {
			continue
		}
		s := section(key[:dot])
		s.opts[key[dot+1:]] = append(s.opts[key[dot+1:]], splitUCIValues(raw)...)
	}
	return sections
}

// splitUCIValues turns `'lan' 'lan2'` into [lan lan2]. Bare values are split on
// whitespace so output from older uci builds still parses.
func splitUCIValues(raw string) []string {
	raw = strings.TrimSpace(raw)
	var vals []string
	for len(raw) > 0 {
		switch raw[0] {
		case '\'', '"':
			end := strings.IndexByte(raw[1:], raw[0])
			if end < 0 {
				return append(vals, strings.Trim(raw, `'"`))
			}
			vals = append(vals, raw[1:1+end])
			raw = strings.TrimSpace(raw[2+end:])
		default:
			return append(vals, strings.Fields(raw)...)
		}
	}
	return vals
}

func valueIn(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func coversAll(values, want []string) bool {
	for _, w := range want {
		if !valueIn(values, w) {
			return false
		}
	}
	return true
}

// steeringDaemons is the set the agent knows how to start. Exactly one may run:
// two steering daemons fight over the same client and produce the disconnect
// storms the design warns about (design D4).
var steeringDaemons = []string{"nrsyncd", "usteer", "dawn"}

// steeringTakesOver reports whether the enabled daemon makes its own client
// steering decisions. nrsyncd only distributes 11k neighbour lists, so the
// agent's own BTM suggestions can safely run alongside it; usteer and dawn
// steer clients themselves and must be left alone.
func steeringTakesOver(daemon string) bool {
	return daemon == SteeringFallbackUsteer || daemon == SteeringFallbackDawn
}

// applySteering makes the AP run the daemon named in the package, and only that
// one. A daemon that is not installed is logged, not fatal: the package may not
// be available for this firmware, and refusing to apply would leave the AP on an
// older revision for a cosmetic reason.
func (a *Applier) applySteering(ctx context.Context, wanted string) {
	if wanted == "" {
		return
	}
	for _, daemon := range steeringDaemons {
		script := "/etc/init.d/" + daemon
		if daemon == wanted {
			if wanted == "none" {
				continue
			}
			if _, err := a.Runner.Run(ctx, script, "enable"); err != nil {
				a.Log.Warn("steering daemon unavailable, steering stays off", "daemon", daemon, "err", err)
				continue
			}
			_, _ = a.Runner.Run(ctx, script, "restart")
			a.Log.Info("steering daemon enabled", "daemon", daemon)
			continue
		}
		// Stop and disable everything else so only one daemon can act.
		_, _ = a.Runner.Run(ctx, script, "stop")
		_, _ = a.Runner.Run(ctx, script, "disable")
	}
}

// probeSteering reports which daemon the device actually considers enabled.
// Measured rather than echoed: a package can name nrsyncd on a firmware that
// does not have it, and the controller must see that difference.
func (a *Applier) probeSteering(ctx context.Context) string {
	for _, daemon := range steeringDaemons {
		if _, err := a.Runner.Run(ctx, "/etc/init.d/"+daemon, "enabled"); err == nil {
			return daemon
		}
	}
	return "none"
}

// restoreFiles copies the backed-up config back into place. Files that did not
// exist before the apply are removed. The returned map is what has to reload.
func (a *Applier) restoreFiles(backupDir string) (map[string]string, error) {
	manifest, err := os.ReadFile(filepath.Join(backupDir, "backup.txt"))
	if err != nil {
		return nil, fmt.Errorf("backup manifest unreadable: %w", err)
	}

	restored := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(manifest)), "\n") {
		name, status, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		target := filepath.Join(a.ConfigDir, name)
		if status == "missing" {
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				a.Log.Error("restore: remove failed", "file", name, "err", err)
			}
			continue
		}
		body, err := os.ReadFile(filepath.Join(backupDir, name))
		if err != nil {
			a.Log.Error("restore: backup copy unreadable", "file", name, "err", err)
			continue
		}
		restored[target] = string(body)
	}
	if err := writeAll(restored); err != nil {
		return nil, fmt.Errorf("write restored config: %w", err)
	}
	return restored, nil
}

// RestoreBackup is the address-move watchdog's undo. Unlike rollback it runs
// long after the apply (possibly after a reboot), so it restores every file in
// the backup and reloads them, then puts DHCP back when the successful apply had
// switched it off.
func (a *Applier) RestoreBackup(ctx context.Context, backupDir string, restoreDHCP bool) error {
	restored, err := a.restoreFiles(backupDir)
	if err != nil {
		return err
	}
	if len(restored) > 0 {
		if err := a.reload(ctx, restored); err != nil {
			return fmt.Errorf("reload restored config: %w", err)
		}
	}
	if restoreDHCP {
		a.enableDHCPServices(ctx)
	}
	return nil
}

// rollback restores the backed-up files and reloads again. Its own failures are
// logged but never mask the original error, which is what the operator needs.
func (a *Applier) rollback(ctx context.Context, backupDir string, pending map[string]string) {
	a.Log.Warn("rolling back to previous configuration", "backup", backupDir)

	if _, err := a.restoreFiles(backupDir); err != nil {
		a.Log.Error("rollback: restore failed", "err", err)
		return
	}
	if err := a.reload(ctx, pending); err != nil {
		a.Log.Error("rollback: reload failed", "err", err)
		return
	}
	a.Log.Info("rollback complete")
}

// wirelessStatus mirrors the parts of `ubus call network.wireless status` the
// agent relies on. Verified against ImmortalWrt 20261026 on an XR1710G:
//
//	{"radio1": {"up": true, "config": {...},
//	             "interfaces": [{"section": "wifinet10", "ifname": "phy0.1-ap0",
//	                             "config": {...}, "stations": []}]}}
//
// The "up" flag lives on the radio, not on each interface. An interface-level
// flag is accepted when present so that both older and newer netifd builds work,
// but its absence must not be read as "down" — doing so would fail every health
// check on real hardware and roll back a perfectly good config.
//
// RadioConfig also carries channel/htmode/txpower for the telemetry collector,
// which reads the same blob. They are typed flexibly because netifd dumps a UCI
// option as a JSON string on one build and a number on another.
type wirelessStatus map[string]radioStatus

type radioStatus struct {
	Up       bool `json:"up"`
	Disabled bool `json:"disabled"`
	Config   struct {
		Band    flexString `json:"band"`
		Channel flexString `json:"channel"`
		HTMode  flexString `json:"htmode"`
		Country flexString `json:"country"`
		TxPower flexInt    `json:"txpower"`
		Radio   flexInt    `json:"radio"`
	} `json:"config"`
	Interfaces []struct {
		Section string `json:"section"`
		Ifname  string `json:"ifname"`
		Up      *bool  `json:"up"`
		Config  struct {
			SSID string `json:"ssid"`
			Mode string `json:"mode"`
		} `json:"config"`
	} `json:"interfaces"`
}

func parseWirelessStatus(out string) (map[string]bool, error) {
	var status wirelessStatus
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		return nil, err
	}
	up := map[string]bool{}
	for _, radio := range status {
		for _, iface := range radio.Interfaces {
			if iface.Section == "" {
				continue
			}
			if iface.Up != nil {
				up[iface.Section] = *iface.Up
				continue
			}
			// A BSS exists only if its radio is up; a down radio reports
			// interfaces that are not serviced.
			up[iface.Section] = radio.Up
		}
	}
	if len(up) == 0 {
		return nil, errors.New("no interfaces reported")
	}
	return up, nil
}

func missingBSS(expected []string, up map[string]bool) []string {
	var missing []string
	for _, name := range expected {
		state, seen := up[name]
		if !seen {
			missing = append(missing, name+"(absent)")
			continue
		}
		if !state {
			missing = append(missing, name+"(down)")
		}
	}
	return missing
}

func readFileOrEmpty(path string) (string, error) {
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(body), nil
}

// writeAll writes files via a temporary sibling plus rename, so a crash mid-write
// cannot leave a truncated config that netifd would refuse to load.
func writeAll(files map[string]string) error {
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("create dir for %s: %w", path, err)
		}
		tmp := path + ".ac-tmp"
		if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", tmp, err)
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("replace %s: %w", path, err)
		}
	}
	return nil
}
