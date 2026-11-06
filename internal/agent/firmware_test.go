package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naoki66/ac-controller/internal/proto"
)

// firmwareFixture serves the two endpoints an upgrade touches: the image and
// the hello the agent uses to report progress.
type firmwareFixture struct {
	t      *testing.T
	server *httptest.Server
	blob   []byte

	mu     sync.Mutex
	hellos []proto.Hello
}

func newFirmwareFixture(t *testing.T, blob []byte) *firmwareFixture {
	t.Helper()
	f := &firmwareFixture{t: t, blob: blob}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/firmwares/image", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(f.blob)
	})
	mux.HandleFunc("POST /api/v1/ap/hello", func(w http.ResponseWriter, r *http.Request) {
		var h proto.Hello
		_ = json.NewDecoder(r.Body).Decode(&h)
		f.mu.Lock()
		f.hellos = append(f.hellos, h)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(proto.HelloReply{
			ACID:   "feedfacefeedfacefeedfacefeedface",
			State:  "adopted",
			PubKey: strings.Repeat("aa", 32),
		})
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *firmwareFixture) lastHello() proto.Hello {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hellos) == 0 {
		return proto.Hello{}
	}
	return f.hellos[len(f.hellos)-1]
}

// sysupgradeCalls filters the runner's command log down to the flash command.
// Other commands are expected: the "downloading" report collects telemetry.
func sysupgradeCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, "sh -c ") {
			out = append(out, c)
		}
	}
	return out
}

func firmwareRefFor(version string, blob []byte) proto.FirmwareRef {
	sum := sha256.Sum256(blob)
	return proto.FirmwareRef{
		Model:   "XR1710G",
		Version: version,
		Path:    "/api/v1/firmwares/image?model=XR1710G&version=" + version,
		SHA256:  hex.EncodeToString(sum[:]),
		Size:    int64(len(blob)),
	}
}

// newUpgradeAgent builds an agent rooted in a temp directory and pointed at the
// fixture controller. Identity.Firmware is the build the AP is currently on.
func newUpgradeAgent(t *testing.T, f *firmwareFixture, running string, dryRun bool) (*Agent, *fakeRunner, string) {
	t.Helper()
	root := t.TempDir()
	runner := &fakeRunner{}

	a, err := New(Config{
		Root:   root,
		ACAddr: strings.TrimPrefix(f.server.URL, "http://"),
		DryRun: dryRun,
		Runner: runner,
		Identity: Identity{
			MAC:      "8c:de:52:11:22:33",
			Model:    "XR1710G",
			Firmware: running,
			Hostname: "ap250",
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	return a, runner, root
}

// TestUpgradeFirmwareStagesAndSchedules walks the success path: the image is
// downloaded, verified, written to /tmp, reported as upgrading, and sysupgrade
// is launched detached.
func TestUpgradeFirmwareStagesAndSchedules(t *testing.T) {
	blob := []byte("firmware-image-bytes")
	f := newFirmwareFixture(t, blob)
	a, runner, root := newUpgradeAgent(t, f, "20260101", false)

	if err := a.upgradeFirmware(context.Background(), firmwareRefFor("20261026", blob)); err != nil {
		t.Fatalf("upgrade: %v", err)
	}

	st := a.State()
	if st.FirmwareState != proto.FirmwareUpgrading || st.FirmwareTarget != "20261026" {
		t.Errorf("state = %+v, want upgrading/20261026", st)
	}
	if h := f.lastHello(); h.FirmwareState != proto.FirmwareUpgrading {
		t.Errorf("reported state = %q, want upgrading before the reboot", h.FirmwareState)
	}
	// The controller must learn that work started *before* the download, otherwise
	// a multi-minute fetch looks like a lost instruction on the device row.
	f.mu.Lock()
	first := f.hellos[0]
	reports := len(f.hellos)
	f.mu.Unlock()
	if reports < 2 {
		t.Errorf("got %d hello reports, want at least downloading and upgrading", reports)
	}
	if first.FirmwareState != proto.FirmwareDownloading {
		t.Errorf("first report = %q, want downloading", first.FirmwareState)
	}
	if !strings.Contains(first.FirmwareMessage, "20261026") {
		t.Errorf("downloading message = %q, want the target version in it", first.FirmwareMessage)
	}

	staged, err := os.ReadFile(filepath.Join(root, "tmp", "ac-firmware.img"))
	if err != nil {
		t.Fatalf("image not staged: %v", err)
	}
	if !bytes.Equal(staged, blob) {
		t.Error("staged image differs from the downloaded bytes")
	}

	calls := runner.Calls()
	var sched string
	for _, c := range calls {
		if strings.HasPrefix(c, "sh -c ") {
			sched = c
		}
	}
	if sched == "" {
		t.Fatalf("sysupgrade was not scheduled: %v", calls)
	}
	if !strings.HasPrefix(sched, "sh -c sleep 2; sysupgrade -q ") {
		t.Errorf("unexpected sysupgrade command: %q", sched)
	}
	// Detached on purpose: a blocking sysupgrade would stall the heartbeat for
	// the whole flash.
	if !strings.Contains(sched, "&") {
		t.Errorf("sysupgrade must run detached: %q", sched)
	}
}

// A digest mismatch is the one failure that must never reach sysupgrade.
func TestUpgradeFirmwareRejectsDigestMismatch(t *testing.T) {
	blob := []byte("firmware-image-bytes")
	f := newFirmwareFixture(t, blob)
	a, runner, root := newUpgradeAgent(t, f, "20260101", false)

	ref := firmwareRefFor("20261026", blob)
	ref.SHA256 = strings.Repeat("00", 32)

	if err := a.upgradeFirmware(context.Background(), ref); err == nil {
		t.Fatal("expected an error for a bad digest")
	}
	if st := a.State(); st.FirmwareState != proto.FirmwareFailed || !strings.Contains(st.FirmwareMessage, "sha256") {
		t.Errorf("state = %+v, want failed with a digest message", st)
	}
	if calls := sysupgradeCalls(runner.Calls()); len(calls) != 0 {
		t.Errorf("sysupgrade ran despite the digest mismatch: %v", calls)
	}
	if _, err := os.Stat(filepath.Join(root, "tmp", "ac-firmware.img")); !os.IsNotExist(err) {
		t.Error("rejected image was staged")
	}
}

// A short read means the download was truncated; flashing it would brick the
// AP, so the size the controller published is enforced too.
func TestUpgradeFirmwareRejectsTruncatedImage(t *testing.T) {
	blob := []byte("firmware-image-bytes")
	f := newFirmwareFixture(t, blob)
	a, runner, _ := newUpgradeAgent(t, f, "20260101", false)

	ref := firmwareRefFor("20261026", blob)
	ref.Size = ref.Size * 2

	if err := a.upgradeFirmware(context.Background(), ref); err == nil {
		t.Fatal("expected an error for a truncated image")
	}
	if st := a.State(); st.FirmwareState != proto.FirmwareFailed {
		t.Errorf("state = %+v, want failed", st)
	}
	if calls := sysupgradeCalls(runner.Calls()); len(calls) != 0 {
		t.Errorf("sysupgrade ran on a truncated image: %v", calls)
	}
}

// After a successful flash the state file is gone (sysupgrade does not keep
// /etc/ac), so the running firmware string is what prevents a second flash.
func TestUpgradeFirmwareRecognisesRunningBuild(t *testing.T) {
	blob := []byte("firmware-image-bytes")
	f := newFirmwareFixture(t, blob)
	// DISTRIB_DESCRIPTION carries the build identifier among other text.
	a, runner, root := newUpgradeAgent(t, f, "XR1710G v1 build 20261026", false)

	if err := a.upgradeFirmware(context.Background(), firmwareRefFor("20261026", blob)); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if st := a.State(); st.FirmwareState != proto.FirmwareApplied {
		t.Errorf("state = %+v, want applied", st)
	}
	if h := f.lastHello(); h.FirmwareState != proto.FirmwareApplied {
		t.Errorf("reported state = %q, want applied", h.FirmwareState)
	}
	for _, c := range runner.Calls() {
		if strings.HasPrefix(c, "sh -c ") {
			t.Errorf("already-running build triggered sysupgrade: %v", runner.Calls())
		}
	}
	if _, err := os.Stat(filepath.Join(root, "tmp", "ac-firmware.img")); !os.IsNotExist(err) {
		t.Error("already-running build was staged")
	}
}

func TestUpgradeFirmwareDryRunDoesNothing(t *testing.T) {
	blob := []byte("firmware-image-bytes")
	f := newFirmwareFixture(t, blob)
	a, runner, root := newUpgradeAgent(t, f, "20260101", true)

	if err := a.upgradeFirmware(context.Background(), firmwareRefFor("20261026", blob)); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if calls := runner.Calls(); len(calls) != 0 {
		t.Errorf("dry run executed commands: %v", calls)
	}
	if f.lastHello().FirmwareState != "" {
		t.Error("dry run reported a firmware state")
	}
	if _, err := os.Stat(filepath.Join(root, "tmp", "ac-firmware.img")); !os.IsNotExist(err) {
		t.Error("dry run staged an image")
	}
}

// reconcileFirmware is what turns "upgrading" into a verdict after the reboot.
func TestReconcileFirmwareAppliedAfterReboot(t *testing.T) {
	f := newFirmwareFixture(t, nil)
	a, _, _ := newUpgradeAgent(t, f, "XR1710G build 20261026", false)
	a.state.FirmwareState = proto.FirmwareUpgrading
	a.state.FirmwareTarget = "20261026"
	a.state.FirmwareAt = time.Now().UTC().Format(time.RFC3339)

	a.reconcileFirmware()
	if st := a.State(); st.FirmwareState != proto.FirmwareApplied {
		t.Errorf("state = %+v, want applied after booting the target", st)
	}
}

func TestReconcileFirmwareTimesOut(t *testing.T) {
	f := newFirmwareFixture(t, nil)
	a, _, _ := newUpgradeAgent(t, f, "20260101", false)

	// No reboot within the timeout: the upgrade simply did not happen.
	a.state.FirmwareState = proto.FirmwareUpgrading
	a.state.FirmwareTarget = "20261026"
	a.state.FirmwareAt = time.Now().Add(-2 * firmwareBootTimeout).UTC().Format(time.RFC3339)
	a.reconcileFirmware()
	if st := a.State(); st.FirmwareState != proto.FirmwareFailed {
		t.Errorf("state = %+v, want failed after the timeout", st)
	}

	// A fresh schedule is not judged before its deadline.
	a.state.FirmwareState = proto.FirmwareUpgrading
	a.state.FirmwareAt = time.Now().UTC().Format(time.RFC3339)
	a.reconcileFirmware()
	if st := a.State(); st.FirmwareState != proto.FirmwareUpgrading {
		t.Errorf("state = %+v, want still upgrading", st)
	}
}
