package agent

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStatusAgent(t *testing.T) *Agent {
	t.Helper()
	dir := t.TempDir()
	return &Agent{
		Config: Config{
			Root:       dir,
			StatusPath: filepath.Join(dir, "run", "ac-agent", "status.json"),
			Interval:   20 * time.Second,
		},
		Identity: Identity{
			MAC:      "00:00:00:00:00:21",
			Model:    "Gemtek XR1710G",
			Firmware: "ImmortalWrt 20261026",
			Hostname: "ap251",
		},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		acAddr:   "10.0.30.5:8090",
		pinnedAC: true,
	}
}

// The state name is what the LuCI badge is chosen from, so each transition has
// to be pinned: "reachable but not adopted" and "adopted" are different answers
// to the operator's real question, "can this AP be controlled".
func TestStatusState(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Agent)
		want   string
	}{
		{
			name:   "before the first cycle",
			mutate: func(a *Agent) {},
			want:   StatusStarting,
		},
		{
			name:   "no controller address",
			mutate: func(a *Agent) { a.cycles = 1; a.acAddr = ""; a.client = nil },
			want:   StatusDiscovering,
		},
		{
			name:   "hello failed",
			mutate: func(a *Agent) { a.cycles = 1; a.rt.LastHelloOK = false },
			want:   StatusUnreachable,
		},
		{
			name:   "hello accepted, not adopted",
			mutate: func(a *Agent) { a.cycles = 1; a.rt.LastHelloOK = true },
			want:   StatusConnected,
		},
		{
			name: "adopted",
			mutate: func(a *Agent) {
				a.cycles = 1
				a.rt.LastHelloOK = true
				a.rt.Adopted = true // what Step() sets from reply.State
				a.state.AppliedRev = 3
			},
			want: StatusManaged,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newStatusAgent(t)
			c.mutate(a)
			if got := a.StatusSnapshot().State; got != c.want {
				t.Errorf("state = %q, want %q", got, c.want)
			}
		})
	}
}

// Adoption has to follow the controller in both directions. An AP that was
// released on the controller still has a revision on disk, so a sticky flag
// would keep the page claiming "managed" while the controller shows "pending".
func TestStatusAdoptedFollowsController(t *testing.T) {
	a := newStatusAgent(t)
	a.cycles = 1
	a.rt.LastHelloOK = true
	a.rt.Adopted = false // the controller just answered "pending"
	a.state.AppliedRev = 2

	snap := a.StatusSnapshot()
	if snap.Adopted {
		t.Error("adopted must follow the controller, not the local revision")
	}
	if snap.State != StatusConnected {
		t.Errorf("state = %q, want %q", snap.State, StatusConnected)
	}
	if snap.AppliedRev != 2 {
		t.Errorf("applied_rev = %d, want 2 (the AP still runs it)", snap.AppliedRev)
	}
}

func TestStatusAddrSource(t *testing.T) {
	pinned := newStatusAgent(t)
	if got := pinned.StatusSnapshot().ACAddrSource; got != "config" {
		t.Errorf("pinned source = %q, want config", got)
	}

	discovered := newStatusAgent(t)
	discovered.pinnedAC = false
	if got := discovered.StatusSnapshot().ACAddrSource; got != "discovered" {
		t.Errorf("discovered source = %q, want discovered", got)
	}
}

// The file must be readable by a local UI with no privileged helper, and must
// never carry the shared secret or the pinned key.
func TestWriteStatusFile(t *testing.T) {
	a := newStatusAgent(t)
	a.cycles = 7
	a.rt.LastHelloOK = true
	a.rt.LastHelloAt = "2026-11-01T14:00:00Z"
	a.rt.DesiredRev = 4
	a.rt.Adopted = true
	a.state.AppliedRev = 4
	a.state.SHA256 = "abc123"
	a.state.ACID = "feedface"

	a.writeStatus()

	path := a.Config.statusPath()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}

	var got Status
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("status is not valid JSON: %v\n%s", err, body)
	}
	if got.State != StatusManaged {
		t.Errorf("state = %q, want managed", got.State)
	}
	if got.AppliedRev != 4 || got.DesiredRev != 4 {
		t.Errorf("revs = %d/%d, want 4/4", got.AppliedRev, got.DesiredRev)
	}
	if got.ACAddr != "10.0.30.5:8090" || got.ACAddrSource != "config" {
		t.Errorf("ac = %q (%q)", got.ACAddr, got.ACAddrSource)
	}
	if got.Interval != 20 || got.Cycles != 7 {
		t.Errorf("interval/cycles = %d/%d, want 20/7", got.Interval, got.Cycles)
	}
	if got.PID == 0 {
		t.Error("pid missing")
	}

	for _, banned := range []string{"secret", "pubkey"} {
		if strings.Contains(strings.ToLower(string(body)), banned) {
			t.Errorf("status file leaks %q:\n%s", banned, body)
		}
	}
}

func TestLiveStatusUsesRecentDaemonSnapshot(t *testing.T) {
	daemon := newStatusAgent(t)
	daemon.cycles = 5
	daemon.rt.LastHelloOK = true
	daemon.rt.Adopted = true
	daemon.rt.PeerAPs = 2
	daemon.writeStatus()
	cli := &Agent{Config: daemon.Config, Identity: daemon.Identity}
	got, ok := cli.LiveStatus()
	if !ok || got.State != StatusManaged || got.PeerAPs != 2 {
		t.Fatalf("daemon status unavailable: %+v, %v", got, ok)
	}
	got.UpdatedAt = "2000-01-01T00:00:00Z"
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cli.Config.statusPath(), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := cli.LiveStatus(); ok {
		t.Fatal("stale daemon status was presented as live")
	}
}

// A failed write must not take the agent down: /var may be read-only. Making a
// directory unwritable is not portable across the CI platforms, so this covers
// the cheap half — a path whose parent is a file, which MkdirAll cannot create.
func TestWriteStatusSurvivesBadPath(t *testing.T) {
	a := newStatusAgent(t)
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("prepare blocker: %v", err)
	}
	a.Config.StatusPath = filepath.Join(blocker, "status.json")
	a.writeStatus() // must not panic
}

// Uptime is read relative to Root so the same code works on a workstation
// against a copied /proc tree. Missing file means "unknown", not zero seconds.
func TestSystemUptime(t *testing.T) {
	root := t.TempDir()
	proc := filepath.Join(root, "proc")
	if err := os.MkdirAll(proc, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(proc, "uptime"), []byte("12345.67 987.65\n"), 0o600); err != nil {
		t.Fatalf("write uptime: %v", err)
	}
	if got := systemUptime(root); got != 12345 {
		t.Errorf("uptime = %d, want 12345", got)
	}
	if got := systemUptime(t.TempDir()); got != 0 {
		t.Errorf("missing file uptime = %d, want 0", got)
	}
}
