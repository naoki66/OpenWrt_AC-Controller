package agent

import (
	"context"
)

// Steering policy: who decides which client-steering daemon runs on this AP.
//
// While the controller answers, its directive always wins — applySteering
// enables exactly the daemon the package names and stops every other one, so
// dawn/usteer can never fight the AC. What this file adds is the outage path:
// in SteeringModeACPriority a sustained loss of the controller hands steering
// to a locally chosen daemon (the fallback), and the first successful hello
// hands it straight back. Nothing here runs more than once per transition, and
// every transition is persisted, so a reboot during an outage does not flip the
// decision back and forth.

// steeringFallbackAfterFailures is how many consecutive failed heartbeats are
// needed before the agent concludes the controller is really gone. Three is the
// same judgement rediscovery makes (about one minute at the default interval):
// shorter would flap on a single lost packet, longer leaves the site unsteered
// through a controller restart.
const steeringFallbackAfterFailures = rediscoverAfterFailures

// controlLost reports whether the controller should currently be considered
// absent. Two shapes of absence exist and both count:
//   - the configured or discovered address stopped answering (helloFailures),
//   - there is no address at all, but this AP has run controller-issued
//     configuration before (a reboot, or the controller vanished between
//     cycles). A device that was never adopted must not start a daemon on its
//     own — that is what SteeringModeACOnly and the rev>0 guard are for.
func (a *Agent) controlLost() bool {
	if a.helloFailures >= steeringFallbackAfterFailures {
		return true
	}
	return a.acAddr == "" && a.state.AppliedRev > 0 && a.cycles >= steeringFallbackAfterFailures
}

// reconcileSteering moves the device between "under AC steering" and
// "autonomous fallback" when the two states disagree. Called once per cycle
// before the hello: decisions use the previous cycle's outcome, which keeps the
// behaviour deterministic and costs at most one interval of lag.
func (a *Agent) reconcileSteering(ctx context.Context) {
	if a.Applier == nil || a.Config.DryRun {
		return
	}
	// Only ac_priority moves daemons outside an apply: autonomous mode belongs
	// to the operator, and ac_only keeps the controller's last state through an
	// outage by definition.
	if a.Config.SteeringMode != SteeringModeACPriority {
		return
	}

	if a.controlLost() {
		fallback := a.Config.SteeringFallback
		if fallback == "" || fallback == SteeringFallbackNone {
			return
		}
		if a.state.SteeringFallbackActive {
			return
		}
		a.Log.Warn("controller lost, switching to autonomous steering",
			"daemon", fallback, "applied_rev", a.state.AppliedRev)
		// applySteering enables the fallback and stops everything else, which
		// is exactly the mutual exclusion the design requires. A daemon that
		// is not installed logs a warning and leaves steering off: refusing to
		// record the transition would retry every cycle for the same result.
		a.Applier.applySteering(ctx, fallback)
		a.state.SteeringFallbackActive = true
		a.invalidateSteeringState()
		if err := a.saveState(); err != nil {
			a.Log.Warn("could not persist fallback state", "err", err)
		}
		return
	}

	// The controller answers again: restore its last known directive. With an
	// empty directive (adopted before this feature existed) the safe move is to
	// stop only the fallback daemon — the next package will name the daemon it
	// wants, and "unknown" must not be read as "none of them".
	if a.state.SteeringFallbackActive {
		if a.state.Steering == "" {
			a.stopFallbackDaemon(ctx)
		} else {
			a.Log.Info("controller recovered, restoring AC steering", "daemon", a.state.Steering)
			a.Applier.applySteering(ctx, a.state.Steering)
		}
		a.state.SteeringFallbackActive = false
		a.invalidateSteeringState()
		if err := a.saveState(); err != nil {
			a.Log.Warn("could not persist steering recovery", "err", err)
		}
	}
}

// steeringModeOrDefault maps an empty mode to the default for readers of the
// field (the status snapshot) before Load has filled it in.
func steeringModeOrDefault(mode string) string {
	if mode == "" {
		return SteeringModeACPriority
	}
	return mode
}

// stopFallbackDaemon disables only the locally chosen daemon, leaving whatever
// the controller last arranged in place.
func (a *Agent) stopFallbackDaemon(ctx context.Context) {
	fallback := a.Config.SteeringFallback
	if fallback == "" || fallback == SteeringFallbackNone {
		return
	}
	a.Log.Info("controller recovered, stopping fallback steering", "daemon", fallback)
	for _, verb := range []string{"stop", "disable"} {
		if _, err := a.Runner.Run(ctx, "/etc/init.d/"+fallback, verb); err != nil {
			a.Log.Debug("fallback daemon command failed", "daemon", fallback, "verb", verb, "err", err)
		}
	}
}
