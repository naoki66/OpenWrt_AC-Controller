package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naoki66/ac-controller/shared/bundle"
	"github.com/naoki66/ac-controller/shared/render"
)

// newSteeringAgent builds an agent wired to a fake runner, with the persisted
// state in a temp dir so saveState can be exercised for real.
func newSteeringAgent(t *testing.T, cfg Config) (*Agent, *fakeRunner) {
	t.Helper()
	root := t.TempDir()
	cfg.Root = root
	cfg.DryRun = false
	runner := &fakeRunner{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := LoadState(filepath.Join(root, "etc", "ac", "state.json"))
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	a := &Agent{
		Config: cfg,
		Log:    log,
		state:  st,
		Runner: runner,
		Applier: &Applier{
			ConfigDir:    filepath.Join(root, "etc", "config"),
			StateDir:     filepath.Join(root, "etc", "ac"),
			Runner:       runner,
			Log:          log,
			SteeringMode: cfg.SteeringMode,
		},
	}
	return a, runner
}

func callsOf(runner *fakeRunner) string { return strings.Join(runner.Calls(), "\n") }

// Losing the controller after having been managed starts the locally chosen
// fallback daemon and stops everything else, exactly once.
func TestFallbackStartsWhenControllerLost(t *testing.T) {
	a, runner := newSteeringAgent(t, Config{
		SteeringMode:     SteeringModeACPriority,
		SteeringFallback: SteeringFallbackUsteer,
	})
	a.state.AppliedRev = 3
	a.helloFailures = steeringFallbackAfterFailures
	ctx := context.Background()

	a.reconcileSteering(ctx)

	calls := callsOf(runner)
	for _, want := range []string{
		"/etc/init.d/usteer enable",
		"/etc/init.d/usteer restart",
		"/etc/init.d/dawn stop",
		"/etc/init.d/dawn disable",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing %q in\n%s", want, calls)
		}
	}
	if !a.state.SteeringFallbackActive {
		t.Error("fallback state was not recorded")
	}
	if _, err := os.Stat(a.Config.statePath()); err != nil {
		t.Errorf("state not persisted: %v", err)
	}

	// A second pass in the same condition must be a no-op: the transition, not
	// the condition, drives the daemon commands.
	before := len(runner.Calls())
	a.reconcileSteering(ctx)
	if got := len(runner.Calls()); got != before {
		t.Errorf("reconcile ran again (%d -> %d calls):\n%s", before, got, callsOf(runner))
	}
}

// A device that was never adopted must not start a daemon on its own: there is
// nothing to fall back from.
func TestNoFallbackWithoutPriorAdoption(t *testing.T) {
	a, runner := newSteeringAgent(t, Config{
		SteeringMode:     SteeringModeACPriority,
		SteeringFallback: SteeringFallbackUsteer,
	})
	a.cycles = 10 // no address at all, long past the grace period

	a.reconcileSteering(context.Background())

	if calls := callsOf(runner); calls != "" {
		t.Errorf("daemons touched on a never-adopted device:\n%s", calls)
	}
}

// When the controller comes back, its last directive is re-asserted and the
// fallback flag clears.
func TestRecoveryRestoresACDirective(t *testing.T) {
	a, runner := newSteeringAgent(t, Config{
		SteeringMode:     SteeringModeACPriority,
		SteeringFallback: SteeringFallbackUsteer,
	})
	a.state.AppliedRev = 3
	a.state.Steering = SteeringFallbackDawn // the AC's package named dawn
	a.state.SteeringFallbackActive = true
	ctx := context.Background()

	a.reconcileSteering(ctx)

	calls := callsOf(runner)
	for _, want := range []string{
		"/etc/init.d/dawn enable",
		"/etc/init.d/dawn restart",
		"/etc/init.d/usteer stop",
		"/etc/init.d/usteer disable",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing %q in\n%s", want, calls)
		}
	}
	if a.state.SteeringFallbackActive {
		t.Error("fallback flag survived recovery")
	}
}

// With no directive on record (adopted before this feature existed) recovery
// stops only the fallback daemon instead of guessing.
func TestRecoveryWithoutDirectiveStopsFallbackOnly(t *testing.T) {
	a, runner := newSteeringAgent(t, Config{
		SteeringMode:     SteeringModeACPriority,
		SteeringFallback: SteeringFallbackUsteer,
	})
	a.state.AppliedRev = 3
	a.state.SteeringFallbackActive = true

	a.reconcileSteering(context.Background())

	calls := callsOf(runner)
	if !strings.Contains(calls, "/etc/init.d/usteer stop") ||
		!strings.Contains(calls, "/etc/init.d/usteer disable") {
		t.Errorf("fallback daemon was not stopped:\n%s", calls)
	}
	if strings.Contains(calls, "enable") || strings.Contains(calls, "restart") {
		t.Errorf("recovery must not start anything without a directive:\n%s", calls)
	}
}

// Autonomous mode hands steering to the operator entirely: even a lost
// controller never touches a daemon.
func TestAutonomousModeNeverTouchesDaemons(t *testing.T) {
	a, runner := newSteeringAgent(t, Config{
		SteeringMode:     SteeringModeAutonomous,
		SteeringFallback: SteeringFallbackUsteer,
	})
	a.state.AppliedRev = 3
	a.helloFailures = steeringFallbackAfterFailures

	a.reconcileSteering(context.Background())

	if calls := callsOf(runner); calls != "" {
		t.Errorf("autonomous mode touched daemons:\n%s", calls)
	}
}

// ac_only keeps the controller's last state through an outage: no fallback, no
// stopping either.
func TestACOnlyHasNoFallback(t *testing.T) {
	a, runner := newSteeringAgent(t, Config{
		SteeringMode:     SteeringModeACOnly,
		SteeringFallback: SteeringFallbackUsteer,
	})
	a.state.AppliedRev = 3
	a.helloFailures = steeringFallbackAfterFailures

	a.reconcileSteering(context.Background())

	if calls := callsOf(runner); calls != "" {
		t.Errorf("ac_only mode touched daemons:\n%s", calls)
	}
}

// The apply pipeline must skip the steering file in autonomous mode.
func TestApplySkipsSteeringWhenAutonomous(t *testing.T) {
	f := newApplyFixture(t, statusWith(true, "ac_ssid_corp_2g"))
	f.applier.SteeringMode = SteeringModeAutonomous
	files := packageFiles()
	files[render.FileSteering] = []byte("nrsyncd\n")

	if _, err := f.applier.Apply(context.Background(), bundle.Manifest{Rev: 1}, files); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if calls := callsOf(f.runner); strings.Contains(calls, "/etc/init.d/") {
		t.Errorf("autonomous apply touched steering daemons:\n%s", calls)
	}
}

// Environment parsing: unknown values fall back to the safe defaults rather
// than silently changing the policy.
func TestSteeringConfigFromEnv(t *testing.T) {
	t.Setenv("AC_STEERING_MODE", "bogus")
	t.Setenv("AC_STEERING_FALLBACK", "dawn")
	cfg := Load(Config{})
	if cfg.SteeringMode != SteeringModeACPriority {
		t.Errorf("unknown mode = %q, want default ac_priority", cfg.SteeringMode)
	}
	if cfg.SteeringFallback != SteeringFallbackDawn {
		t.Errorf("fallback = %q, want dawn", cfg.SteeringFallback)
	}

	t.Setenv("AC_STEERING_MODE", "autonomous")
	cfg = Load(Config{})
	if cfg.SteeringMode != SteeringModeAutonomous {
		t.Errorf("mode = %q, want autonomous", cfg.SteeringMode)
	}
}
