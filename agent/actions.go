package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/naoki66/ac-controller/shared/proto"
)

// The agent re-checks both arguments even though the controller validated
// them: an AP must never pass an arbitrary string to ubus.
var (
	actionMACRe    = regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)
	actionIfnameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// executedActionHistory bounds the on-disk idempotency memory. The controller
// stops resending an action as soon as its result arrives, so only the last
// few dozen matter; keeping a cap stops state.json growing forever.
const executedActionHistory = 64

// runActions executes the commands from a hello reply and records their
// outcomes. It reports whether a reboot was scheduled: the caller must stop
// working once the AP is about to go down.
func (a *Agent) runActions(ctx context.Context, actions []proto.HelloAction) (rebootScheduled bool, err error) {
	for _, act := range actions {
		if act.ID == "" || a.actionExecuted(act.ID) {
			continue
		}
		// Persist the ID before running anything. A reboot that loses its report
		// is retried by the controller, and running it again would loop the AP.
		a.rememberAction(act.ID)
		if err := a.saveState(); err != nil {
			return false, fmt.Errorf("record action %s: %w", act.ID, err)
		}

		res := a.executeAction(ctx, act)
		a.state.ActionResults = append(a.state.ActionResults, res)
		if err := a.saveState(); err != nil {
			return false, fmt.Errorf("record action result %s: %w", act.ID, err)
		}
		a.Log.Info("controller action executed", "id", act.ID, "type", act.Type, "state", res.State)

		if act.Type == proto.ActionReboot && res.State == proto.ActionResultDone {
			// Report before the reboot actually happens (the command sleeps 2s),
			// then stop: no revision apply on a box that is going down.
			a.reportActionResults(ctx)
			return true, nil
		}
		// A factory reset also ends with a reboot; nothing else may run on a box
		// that is going down.
		if act.Type == proto.ActionFactoryReset && res.State == proto.ActionResultDone {
			a.reportActionResults(ctx)
			return true, nil
		}
	}
	// Non-reboot results ride the next scheduled hello; no extra round trip.
	return false, nil
}

func (a *Agent) executeAction(ctx context.Context, act proto.HelloAction) proto.ActionResult {
	res := proto.ActionResult{
		ID:    act.ID,
		Type:  act.Type,
		State: proto.ActionResultFailed,
		At:    time.Now().Unix(),
	}
	if a.Config.DryRun {
		res.Message = "dry run: not executed"
		return res
	}
	// LED control is a direct sysfs write, not a subprocess, so it works even
	// when no command runner is configured.
	if act.Type == proto.ActionLEDSet {
		state := strings.ToLower(strings.TrimSpace(act.Args["state"]))
		if err := validateLEDState(state); err != nil {
			res.Message = err.Error()
			return res
		}
		names, err := setStatusLED(defaultLEDRoot, state)
		if err != nil {
			res.Message = truncate(err.Error(), 300)
			return res
		}
		res.State = proto.ActionResultDone
		res.Message = "status LED " + state + ": " + strings.Join(names, ", ")
		return res
	}
	if a.Runner == nil {
		res.Message = "no command runner configured"
		return res
	}

	switch act.Type {
	case proto.ActionWifiReload:
		if _, err := a.Runner.Run(ctx, "wifi", "reload"); err != nil {
			res.Message = truncate(err.Error(), 300)
			return res
		}
		res.State = proto.ActionResultDone
		res.Message = "wireless reloaded"
	case proto.ActionReboot:
		// Detached like sysupgrade: the runner must return before the box goes
		// down, and the short sleep lets this result reach the controller first.
		if _, err := a.Runner.Run(ctx, "sh", "-c", "sleep 2; reboot &"); err != nil {
			res.Message = truncate(err.Error(), 300)
			return res
		}
		res.State = proto.ActionResultDone
		res.Message = "rebooting"
	case proto.ActionFactoryReset:
		if err := a.factoryReset(ctx); err != nil {
			res.Message = truncate(err.Error(), 300)
			return res
		}
		if _, err := a.Runner.Run(ctx, "sh", "-c", "sleep 2; reboot &"); err != nil {
			res.Message = "configuration restored but reboot failed: " + truncate(err.Error(), 240)
			return res
		}
		res.State = proto.ActionResultDone
		res.Message = "restored pre-controller configuration; rebooting"
	case proto.ActionDiagnose:
		out, err := a.diagnose(ctx, act.Args["kind"])
		if err != nil {
			res.Message = truncate(err.Error(), 300)
			return res
		}
		res.State = proto.ActionResultDone
		if strings.TrimSpace(out) == "" {
			out = "(no output)"
		}
		// The result rides the next hello. Logs are bounded to a screenful so
		// a pathological buffer cannot turn one heartbeat into a megabyte.
		res.Message = truncate(strings.TrimSpace(out), 4000)
	case proto.ActionKick:
		mac := strings.ToLower(strings.TrimSpace(act.Args["mac"]))
		ifname := strings.TrimSpace(act.Args["ifname"])
		if !actionMACRe.MatchString(mac) {
			res.Message = "invalid client MAC"
			return res
		}
		if !actionIfnameRe.MatchString(ifname) {
			res.Message = "invalid interface name"
			return res
		}
		arg, err := json.Marshal(map[string]string{"addr": mac})
		if err != nil {
			res.Message = truncate(err.Error(), 300)
			return res
		}
		if _, err := a.Runner.Run(ctx, "ubus", "call", "hostapd."+ifname, "deauthenticate", string(arg)); err != nil {
			res.Message = truncate(err.Error(), 300)
			return res
		}
		res.State = proto.ActionResultDone
		res.Message = "client deauthenticated"
	default:
		res.Message = "unsupported action type"
	}
	return res
}

// diagnose runs one of the fixed read-only checks. There is no free-form
// command path: the kind selects a command, and the controller validates the
// same closed set before queuing it.
func (a *Agent) diagnose(ctx context.Context, kind string) (string, error) {
	switch strings.TrimSpace(kind) {
	case proto.DiagnoseNetwork:
		addrs, err := a.Runner.Run(ctx, "ip", "-br", "addr")
		if err != nil {
			return "", fmt.Errorf("ip addr: %w", err)
		}
		routes, err := a.Runner.Run(ctx, "ip", "route")
		if err != nil {
			return "", fmt.Errorf("ip route: %w", err)
		}
		return "== addresses ==\n" + addrs + "\n== routes ==\n" + routes, nil
	case proto.DiagnoseSystem:
		return a.Runner.Run(ctx, "logread", "-l", "80")
	case proto.DiagnoseKernel:
		raw, err := a.Runner.Run(ctx, "dmesg")
		if err != nil {
			return "", err
		}
		return tailLines(raw, 80), nil
	default:
		return "", errors.New("invalid diagnose kind")
	}
}

// tailLines keeps the newest lines. dmesg has no portable "last N" flag across
// BusyBox builds, so the truncation happens here instead of in a shell pipeline.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// factoryReset restores the configuration snapshot taken before the controller
// ever applied a revision, then forgets the controller relationship. The
// oldest backup is the pre-adoption state: every later backup is one of our own
// revisions.
//
// It deliberately does NOT run something like firstboot: that wipes the overlay
// and would remove the agent itself, leaving a remote AP that can never be
// reached again.
func (a *Agent) factoryReset(ctx context.Context) error {
	dir := oldestBackupDir(filepath.Join(a.Config.stateDir(), "backup"))
	if dir == "" {
		return errors.New("no pre-controller backup available")
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "backup.txt"))
	if err != nil {
		return fmt.Errorf("read backup manifest: %w", err)
	}

	restore := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(manifest)), "\n") {
		name, status, ok := strings.Cut(line, "\t")
		if !ok || name == "" {
			continue
		}
		target := filepath.Join(a.Config.configDir(), name)
		if status == "missing" {
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove %s: %w", name, err)
			}
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("read backup %s: %w", name, err)
		}
		restore[target] = string(body)
	}
	if err := writeAll(restore); err != nil {
		return err
	}
	if _, err := a.Runner.Run(ctx, "ubus", "call", "network", "reload"); err != nil {
		return fmt.Errorf("reload network: %w", err)
	}
	if _, err := a.Runner.Run(ctx, "wifi", "reload"); err != nil {
		return fmt.Errorf("reload wireless: %w", err)
	}

	// Forget what the controller taught this AP, but keep the action memory:
	// if the result is lost on the way and the controller resends the reset,
	// running it again after the reboot would loop the box.
	previous := a.state
	a.state = State{
		EnrollmentID:         a.state.EnrollmentID,
		ResetGeneration:      a.state.ResetGeneration,
		ManagementKey:        a.state.ManagementKey,
		ManagementTrafficKey: a.state.ManagementTrafficKey,
		ManagementAddr:       a.state.ManagementAddr,
		ManagementReset:      a.state.ManagementReset,
		ControllerOverrideID: a.state.ControllerOverrideID,
		ExecutedActions:      a.state.ExecutedActions,
		ActionResults:        a.state.ActionResults,
	}
	if previous.ManagementKey != "" {
		a.state.ACID, a.state.PubKey = previous.ACID, previous.PubKey
	}
	if err := a.saveState(); err != nil {
		return fmt.Errorf("persist cleared state: %w", err)
	}
	return nil
}

// oldestBackupDir returns the earliest backup directory under root. Names are
// "rev<rev>-<unix>"; sorting by timestamp rather than by name avoids rev10
// sorting before rev2.
func oldestBackupDir(root string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	var best string
	var bestTS int64 = -1
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		i := strings.LastIndex(e.Name(), "-")
		if i < 0 || i == len(e.Name())-1 {
			continue
		}
		ts, err := strconv.ParseInt(e.Name()[i+1:], 10, 64)
		if err != nil {
			continue
		}
		if bestTS < 0 || ts < bestTS {
			bestTS = ts
			best = filepath.Join(root, e.Name())
		}
	}
	return best
}

// reportActionResults makes one extra hello so a reboot's outcome is not lost
// with the shutdown. A failure is fine: the result stays on disk and rides the
// hello after the AP comes back.
func (a *Agent) reportActionResults(ctx context.Context) {
	if a.client == nil {
		return
	}
	if _, err := a.client.Hello(ctx, a.buildHello(ctx)); err != nil {
		a.Log.Warn("could not report action result yet, will retry", "err", err)
		return
	}
	a.clearDeliveredActionResults()
}

// clearDeliveredActionResults drops results the controller has acknowledged.
func (a *Agent) clearDeliveredActionResults() {
	if len(a.state.ActionResults) == 0 {
		return
	}
	a.state.ActionResults = nil
	if err := a.saveState(); err != nil {
		a.Log.Warn("could not clear delivered action results", "err", err)
	}
}

func (a *Agent) actionExecuted(id string) bool {
	for _, seen := range a.state.ExecutedActions {
		if seen == id {
			return true
		}
	}
	return false
}

func (a *Agent) rememberAction(id string) {
	a.state.ExecutedActions = append(a.state.ExecutedActions, id)
	if len(a.state.ExecutedActions) > executedActionHistory {
		a.state.ExecutedActions = a.state.ExecutedActions[len(a.state.ExecutedActions)-executedActionHistory:]
	}
}
