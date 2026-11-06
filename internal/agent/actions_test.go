package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naoki66/ac-controller/internal/proto"
)

func newActionsAgent(t *testing.T, dryRun bool) (*Agent, *fakeRunner) {
	t.Helper()
	root := t.TempDir()
	runner := &fakeRunner{}
	a := &Agent{
		Config: Config{Root: root, DryRun: dryRun},
		Runner: runner,
		Log:    testLog(),
	}
	return a, runner
}

// A command must run once even though the controller keeps re-sending it until
// the result arrives.
func TestRunActionsExecutesOnce(t *testing.T) {
	a, runner := newActionsAgent(t, false)
	act := proto.HelloAction{ID: "a1", Type: proto.ActionWifiReload}

	if _, err := a.runActions(context.Background(), []proto.HelloAction{act}); err != nil {
		t.Fatalf("run actions: %v", err)
	}
	if len(runner.calls) != 1 || runner.calls[0] != "wifi reload" {
		t.Fatalf("calls = %v, want [wifi reload]", runner.calls)
	}
	if len(a.state.ActionResults) != 1 || a.state.ActionResults[0].State != proto.ActionResultDone {
		t.Fatalf("results = %+v, want one done", a.state.ActionResults)
	}

	// Same ID again: no second command, no second result.
	if _, err := a.runActions(context.Background(), []proto.HelloAction{act}); err != nil {
		t.Fatalf("repeat run actions: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Errorf("calls after repeat = %v, want one", runner.calls)
	}
	if len(a.state.ActionResults) != 1 {
		t.Errorf("results after repeat = %+v, want one", a.state.ActionResults)
	}
}

// The executed-ID memory is written before the command runs, so an action whose
// result was lost cannot be replayed after the agent restarts.
func TestExecutedActionSurvivesRestart(t *testing.T) {
	a, runner := newActionsAgent(t, false)
	act := proto.HelloAction{ID: "r1", Type: proto.ActionReboot}
	if _, err := a.runActions(context.Background(), []proto.HelloAction{act}); err != nil {
		t.Fatalf("run actions: %v", err)
	}

	st, err := LoadState(filepath.Join(a.Config.Root, "etc", "ac", "state.json"))
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	a.state = st
	callsBefore := len(runner.calls)
	if _, err := a.runActions(context.Background(), []proto.HelloAction{act}); err != nil {
		t.Fatalf("run actions after reload: %v", err)
	}
	if len(runner.calls) != callsBefore {
		t.Errorf("calls after reload = %v, want no new command", runner.calls)
	}
}

// A reboot ends the cycle: nothing else may run on an AP that is going down.
func TestRunActionsRebootStopsCycle(t *testing.T) {
	a, runner := newActionsAgent(t, false)
	reboot, err := a.runActions(context.Background(), []proto.HelloAction{
		{ID: "r2", Type: proto.ActionReboot},
	})
	if err != nil {
		t.Fatalf("run actions: %v", err)
	}
	if !reboot {
		t.Fatal("reboot flag = false, want true")
	}
	if len(runner.calls) != 1 || runner.calls[0] != "sh -c sleep 2; reboot &" {
		t.Fatalf("calls = %v, want the detached reboot", runner.calls)
	}
	if a.state.ActionResults[0].State != proto.ActionResultDone {
		t.Errorf("result = %+v, want done", a.state.ActionResults[0])
	}
}

func TestRunActionsRejectsUnknownAndDryRun(t *testing.T) {
	a, runner := newActionsAgent(t, false)
	if _, err := a.runActions(context.Background(), []proto.HelloAction{{ID: "x1", Type: "shell"}}); err != nil {
		t.Fatalf("run actions: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("calls = %v, want none", runner.calls)
	}
	if got := a.state.ActionResults[0].State; got != proto.ActionResultFailed {
		t.Errorf("state = %q, want failed", got)
	}

	dry, dryRunner := newActionsAgent(t, true)
	if _, err := dry.runActions(context.Background(), []proto.HelloAction{{ID: "x2", Type: proto.ActionWifiReload}}); err != nil {
		t.Fatalf("dry run actions: %v", err)
	}
	if len(dryRunner.calls) != 0 {
		t.Errorf("dry run calls = %v, want none", dryRunner.calls)
	}
}

// A factory reset must restore the snapshot taken before the controller ever
// managed the AP, not the backup of our own last revision, and must forget the
// adoption while keeping the action idempotency memory.
func TestFactoryResetRestoresOldestBackup(t *testing.T) {
	a, runner := newActionsAgent(t, false)
	cfgDir := filepath.Join(a.Config.Root, "etc", "config")
	stateDir := filepath.Join(a.Config.Root, "etc", "ac")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "wireless"), []byte("controller-wireless"), 0o644); err != nil {
		t.Fatalf("write wireless: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "network"), []byte("controller-network"), 0o644); err != nil {
		t.Fatalf("write network: %v", err)
	}
	writeBackup(t, filepath.Join(stateDir, "backup", "rev2-2000"), "newer-wireless", "newer-network")
	writeBackup(t, filepath.Join(stateDir, "backup", "rev1-1000"), "factory-wireless", "factory-network")

	a.state.AppliedRev = 2
	a.state.ACID = "feedfacefeedfacefeedfacefeedface"
	a.state.SHA256 = "deadbeef"

	reboot, err := a.runActions(context.Background(), []proto.HelloAction{
		{ID: "f1", Type: proto.ActionFactoryReset},
	})
	if err != nil {
		t.Fatalf("run actions: %v", err)
	}
	if !reboot {
		t.Fatal("factory reset must end the cycle with a reboot")
	}

	want := []string{"ubus call network reload", "wifi reload", "sh -c sleep 2; reboot &"}
	if len(runner.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", runner.calls, want)
	}
	for i := range want {
		if runner.calls[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, runner.calls[i], want[i])
		}
	}

	for name, wantBody := range map[string]string{
		"wireless": "factory-wireless",
		"network":  "factory-network",
	} {
		body, err := os.ReadFile(filepath.Join(cfgDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(body) != wantBody {
			t.Errorf("%s = %q, want %q", name, body, wantBody)
		}
	}

	if a.state.AppliedRev != 0 || a.state.ACID != "" || a.state.SHA256 != "" {
		t.Errorf("state not cleared: %+v", a.state)
	}
	if !a.actionExecuted("f1") {
		t.Error("action ID lost while clearing state; a retried reset would loop the AP")
	}
	if len(a.state.ActionResults) != 1 || a.state.ActionResults[0].State != proto.ActionResultDone {
		t.Errorf("results = %+v, want one done", a.state.ActionResults)
	}
}

func TestFactoryResetWithoutBackupFails(t *testing.T) {
	a, runner := newActionsAgent(t, false)
	if _, err := a.runActions(context.Background(), []proto.HelloAction{
		{ID: "f2", Type: proto.ActionFactoryReset},
	}); err != nil {
		t.Fatalf("run actions: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("calls = %v, want none", runner.calls)
	}
	if got := a.state.ActionResults[0].State; got != proto.ActionResultFailed {
		t.Errorf("state = %q, want failed", got)
	}
}

func writeBackup(t *testing.T, dir, wireless, network string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir backup: %v", err)
	}
	for name, body := range map[string]string{"wireless": wireless, "network": network} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write backup %s: %v", name, err)
		}
	}
	manifest := "wireless\tpresent\nnetwork\tpresent\n"
	if err := os.WriteFile(filepath.Join(dir, "backup.txt"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write backup manifest: %v", err)
	}
}

// Diagnostics are a closed set of read-only commands. The agent must never run
// an arbitrary command supplied through args, and the output has to survive the
// hello reply so an operator can read it on the system page.
func TestDiagnoseActionFixedChecks(t *testing.T) {
	a, runner := newActionsAgent(t, false)
	runner.replies = map[string]string{
		"ip -br addr":   "eth0 UP 10.0.30.251/24",
		"ip route":      "default via 10.0.30.1",
		"logread -l 80": "system line 1\nsystem line 2",
		"dmesg ":        strings.Join(lineList(100), "\n"),
	}

	if _, err := a.runActions(context.Background(), []proto.HelloAction{{
		ID: "d1", Type: proto.ActionDiagnose, Args: map[string]string{"kind": proto.DiagnoseNetwork},
	}}); err != nil {
		t.Fatalf("network diagnose: %v", err)
	}
	if len(runner.calls) != 2 || runner.calls[0] != "ip -br addr" || runner.calls[1] != "ip route" {
		t.Fatalf("network calls = %v", runner.calls)
	}
	res := a.state.ActionResults[0]
	if res.State != proto.ActionResultDone || !strings.Contains(res.Message, "eth0 UP") {
		t.Errorf("network result = %+v", res)
	}

	if _, err := a.runActions(context.Background(), []proto.HelloAction{{
		ID: "d2", Type: proto.ActionDiagnose, Args: map[string]string{"kind": proto.DiagnoseSystem},
	}}); err != nil {
		t.Fatalf("system diagnose: %v", err)
	}
	if runner.calls[len(runner.calls)-1] != "logread -l 80" {
		t.Errorf("system call = %v", runner.calls)
	}
	if got := a.state.ActionResults[1].Message; !strings.Contains(got, "system line 2") {
		t.Errorf("system result = %q", got)
	}

	if _, err := a.runActions(context.Background(), []proto.HelloAction{{
		ID: "d3", Type: proto.ActionDiagnose, Args: map[string]string{"kind": proto.DiagnoseKernel},
	}}); err != nil {
		t.Fatalf("kernel diagnose: %v", err)
	}
	// 100 lines in, 80 come back: the tail starts at l20.
	kernel := a.state.ActionResults[2].Message
	if !strings.HasPrefix(kernel, "l20\n") || !strings.HasSuffix(kernel, "l99") {
		t.Errorf("kernel result starts %q ends %q", head(kernel, 20), tail(kernel, 10))
	}

	if _, err := a.runActions(context.Background(), []proto.HelloAction{{
		ID: "d4", Type: proto.ActionDiagnose, Args: map[string]string{"kind": "shell"},
	}}); err != nil {
		t.Fatalf("invalid diagnose: %v", err)
	}
	if got := a.state.ActionResults[3]; got.State != proto.ActionResultFailed {
		t.Errorf("invalid kind state = %q, want failed", got.State)
	}
}

func lineList(n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("l%d", i)
	}
	return lines
}

func head(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// Kicking one client is a single hostapd deauthenticate call with a
// normalized MAC; the agent must not accept anything else as an argument.
func TestKickClientAction(t *testing.T) {
	a, runner := newActionsAgent(t, false)
	if _, err := a.runActions(context.Background(), []proto.HelloAction{{
		ID:   "k1",
		Type: proto.ActionKick,
		Args: map[string]string{"mac": "AA:BB:CC:DD:EE:FF", "ifname": "phy1-ap0"},
	}}); err != nil {
		t.Fatalf("run actions: %v", err)
	}

	want := `ubus call hostapd.phy1-ap0 deauthenticate {"addr":"aa:bb:cc:dd:ee:ff"}`
	if len(runner.calls) != 1 || runner.calls[0] != want {
		t.Fatalf("calls = %v, want [%s]", runner.calls, want)
	}
	res := a.state.ActionResults[0]
	if res.State != proto.ActionResultDone || res.Message != "client deauthenticated" {
		t.Errorf("result = %+v", res)
	}
}

func TestKickClientActionRejectsUnsafeArgs(t *testing.T) {
	cases := []map[string]string{
		{"mac": "nope", "ifname": "phy1-ap0"},
		{"mac": "aa:bb:cc:dd:ee:ff", "ifname": "phy1 ap0"},
		{"mac": "aa:bb:cc:dd:ee:ff"},
	}
	for i, args := range cases {
		a, runner := newActionsAgent(t, false)
		if _, err := a.runActions(context.Background(), []proto.HelloAction{{
			ID:   fmt.Sprintf("bad%d", i),
			Type: proto.ActionKick,
			Args: args,
		}}); err != nil {
			t.Fatalf("args %v: run actions: %v", args, err)
		}
		if len(runner.calls) != 0 {
			t.Errorf("args %v: calls = %v, want none", args, runner.calls)
		}
		if got := a.state.ActionResults[0].State; got != proto.ActionResultFailed {
			t.Errorf("args %v: state = %q, want failed", args, got)
		}
	}
}
