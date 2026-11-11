package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naoki66/ac-controller/shared/proto"
)

// A management-address move is provisional until the controller answers a
// hello. When the window expires without one, the old network must come back —
// including the DHCP server the successful apply had switched off.
func TestPendingConfirmRestoresBackupAndDHCP(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "etc", "config")
	backupDir := filepath.Join(root, "etc", "ac", "backup", "rev-9")
	for _, dir := range []string{configDir, backupDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	const oldNetwork = "config interface 'wan'\n\toption proto 'dhcp'\n"
	const newNetwork = "config interface 'lan'\n\toption ipaddr '10.10.10.250/24'\n"
	files := []struct{ path, body string }{
		{filepath.Join(configDir, "network"), newNetwork},
		{filepath.Join(configDir, "wireless"), "config wifi-device 'radio0'\n"},
		{filepath.Join(backupDir, "network"), oldNetwork},
		{filepath.Join(backupDir, "wireless"), "config wifi-device 'radio0'\n"},
		{filepath.Join(backupDir, "backup.txt"), "network\tpresent\nwireless\tpresent\n"},
	}
	for _, f := range files {
		if err := os.WriteFile(f.path, []byte(f.body), 0o644); err != nil {
			t.Fatalf("seed %s: %v", f.path, err)
		}
	}

	runner := &fakeRunner{}
	a := &Agent{
		Config: Config{Root: root},
		Applier: &Applier{
			ConfigDir: configDir,
			Runner:    runner,
			Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		state: State{PendingConfirm: &PendingConfirm{
			Rev:         9,
			BackupDir:   backupDir,
			Deadline:    time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
			RestoreDHCP: true,
		}},
	}

	a.checkPendingConfirm(context.Background())

	body, err := os.ReadFile(filepath.Join(configDir, "network"))
	if err != nil {
		t.Fatalf("read restored network: %v", err)
	}
	if string(body) != oldNetwork {
		t.Errorf("network not restored:\n%s", body)
	}
	if a.state.PendingConfirm != nil {
		t.Error("pending confirm was not cleared")
	}
	if a.state.PendingSync != proto.SyncRollback {
		t.Errorf("pending sync = %q, want rollback", a.state.PendingSync)
	}
	if !strings.Contains(a.state.PendingSyncMsg, "never confirmed") {
		t.Errorf("message = %q", a.state.PendingSyncMsg)
	}
	if n := runner.countCalls("uci -q set dhcp.@dnsmasq[0].disabled=0"); n != 1 {
		t.Errorf("DHCP was not switched back on: %v", runner.Calls())
	}
	if n := runner.countCalls("/etc/init.d/dnsmasq start"); n != 1 {
		t.Errorf("dnsmasq was not restarted: %v", runner.Calls())
	}
}

// Before the deadline the watchdog must leave a healthy apply alone.
func TestPendingConfirmWaitsForTheWindow(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "etc", "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const current = "config interface 'lan'\n\toption ipaddr '10.10.10.250/24'\n"
	if err := os.WriteFile(filepath.Join(configDir, "network"), []byte(current), 0o644); err != nil {
		t.Fatalf("seed network: %v", err)
	}

	runner := &fakeRunner{}
	a := &Agent{
		Config: Config{Root: root},
		Applier: &Applier{
			ConfigDir: configDir,
			Runner:    runner,
			Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		state: State{PendingConfirm: &PendingConfirm{
			Rev:       9,
			BackupDir: filepath.Join(root, "missing"),
			Deadline:  time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
		}},
	}

	a.checkPendingConfirm(context.Background())

	body, err := os.ReadFile(filepath.Join(configDir, "network"))
	if err != nil {
		t.Fatalf("read network: %v", err)
	}
	if string(body) != current {
		t.Error("network changed before the deadline")
	}
	if a.state.PendingConfirm == nil {
		t.Error("pending confirm cleared before the deadline")
	}
	if len(runner.Calls()) != 0 {
		t.Errorf("commands ran before the deadline: %v", runner.Calls())
	}
}

func TestNetworkConfirmationMustMatchAppliedRevision(t *testing.T) {
	a := &Agent{
		Config: Config{ACPublicKey: "pinned-key"},
		state:  State{ACID: "controller", PendingConfirm: &PendingConfirm{Rev: 9}},
	}
	reply := proto.HelloReply{ACID: "controller", PubKey: "pinned-key", State: proto.APStateAdopted}
	for _, rev := range []int{0, 8, 10} {
		reply.AcceptedRev = rev
		if a.confirmedRevision(reply) {
			t.Fatalf("revision %d confirmed pending revision 9", rev)
		}
	}
	reply.AcceptedRev = 9
	if !a.confirmedRevision(reply) {
		t.Fatal("matching ACK did not confirm the network change")
	}
}

func TestInterruptedApplyKeepsJournalWhenRestoreFails(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "missing-backup")
	a := &Agent{
		Config:  Config{Root: root},
		Applier: &Applier{ConfigDir: filepath.Join(root, "etc", "config")},
		state:   State{AppliedRev: 2, PendingApply: dir},
	}
	if err := a.saveState(); err != nil {
		t.Fatal(err)
	}
	if err := a.recoverInterruptedApply(context.Background()); err == nil {
		t.Fatal("recovery reported success without a backup")
	}
	stored, err := LoadState(a.Config.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if a.state.PendingApply != dir || stored.PendingApply != dir || stored.AppliedRev != 2 {
		t.Fatalf("failed recovery lost the journal: in memory %+v; on disk %+v", a.state, stored)
	}
}
