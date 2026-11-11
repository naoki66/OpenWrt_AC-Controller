package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/naoki66/ac-controller/shared/proto"
)

// Firmware upgrade (P4).
//
// The controller owns the repository and the decision; the agent owns the flash.
// A hello reply that carries a FirmwareRef means "run this build", and the agent
// downloads it, verifies the digest the controller published, schedules
// sysupgrade and reports what happened. Nothing is re-sent by the controller after
// applied/failed, so an image that cannot boot cannot turn into a reboot loop.

// firmwareBootTimeout is how long "upgrading" may last without the running
// firmware string changing. A sysupgrade from download to reboot takes minutes;
// beyond this the upgrade simply did not happen (wrong image, failed flash,
// init script killed) and saying so is better than showing "upgrading" forever.
const firmwareBootTimeout = 10 * time.Minute

// firmwareDownloadTimeout bounds one image fetch. The shared client timeout is
// 30s, sized for a hello or a package; a sysupgrade image is tens of MB and
// takes minutes over WiFi, so using that limit would fail every real upgrade.
const firmwareDownloadTimeout = 20 * time.Minute

// reconcileFirmware resolves an upgrade that was scheduled before a reboot.
//
// It runs at the top of every cycle because the interesting transition — the
// AP coming back on the new build — is only observable from the identity read
// at startup. Note that sysupgrade does not preserve /etc/ac, so this state is
// often gone after a successful flash; upgradeFirmware() therefore also compares
// the controller's target with the running firmware.
func (a *Agent) reconcileFirmware() {
	if a.state.FirmwareState != proto.FirmwareUpgrading {
		return
	}
	target := a.state.FirmwareTarget
	if target != "" && firmwareMatches(a.Identity.Firmware, target) {
		a.setFirmwareState(proto.FirmwareApplied, "running "+a.Identity.Firmware)
		a.Log.Info("firmware upgrade landed", "version", target, "running", a.Identity.Firmware)
		return
	}

	at, err := time.Parse(time.RFC3339, a.state.FirmwareAt)
	if err != nil || time.Since(at) < firmwareBootTimeout {
		return
	}
	a.setFirmwareState(proto.FirmwareFailed,
		fmt.Sprintf("no reboot into %s within %s (still running %s)", target, firmwareBootTimeout, a.Identity.Firmware))
	a.Log.Error("firmware upgrade did not take effect", "version", target, "running", a.Identity.Firmware)
}

// upgradeFirmware acts on a FirmwareRef from the hello reply.
//
// Every rejection is reported back rather than retried silently: the operator
// needs to see "digest mismatch" on the device row, not a controller that keeps
// asking forever. The two exceptions are an upgrade already in flight and a
// build the AP is already running, which are no-ops.
func (a *Agent) upgradeFirmware(ctx context.Context, ref proto.FirmwareRef) error {
	if ref.Version == "" || ref.Path == "" {
		return nil
	}
	if a.state.FirmwareState == proto.FirmwareUpgrading && a.state.FirmwareTarget == ref.Version {
		return nil // sysupgrade already scheduled; reconcile decides the outcome
	}
	if a.Identity.Model != "" && ref.Model != "" && !strings.EqualFold(a.Identity.Model, ref.Model) {
		msg := fmt.Sprintf("image is for %s, this AP is %s", ref.Model, a.Identity.Model)
		return a.reportFirmwareFailure(msg, errors.New(msg))
	}
	if digest, err := hex.DecodeString(ref.SHA256); err != nil || len(digest) != sha256.Size ||
		ref.Size <= 0 || ref.Size > 300<<20 {
		err := errors.New("signed firmware reference has an invalid SHA-256 or size")
		return a.reportFirmwareFailure(err.Error(), err)
	}

	// Already running the requested build. This is the normal path right after a
	// successful flash: the local state file did not survive sysupgrade, but the
	// running firmware says everything the controller needs. Reporting applied
	// here is what stops the same image being sent again.
	if firmwareMatches(a.Identity.Firmware, ref.Version) {
		a.state.FirmwareTarget = ref.Version
		a.setFirmwareState(proto.FirmwareApplied, "already running "+a.Identity.Firmware)
		a.notifyFirmware(ctx)
		return nil
	}

	if a.Config.DryRun {
		a.Log.Info("dry run: firmware would be installed", "version", ref.Version, "size", ref.Size)
		return nil
	}

	// Say "downloading" before the fetch, not after it: a tens-of-MB image takes
	// minutes on WiFi, and until this report the controller could only show the
	// old build with no indication that work had started.
	a.state.FirmwareTarget = ref.Version
	a.setFirmwareState(proto.FirmwareDownloading,
		fmt.Sprintf("downloading %s %s (%d bytes)", ref.Model, ref.Version, ref.Size))
	a.notifyFirmware(ctx)

	image := a.firmwareImagePath()
	if err := os.MkdirAll(filepath.Dir(image), 0o755); err != nil {
		return a.reportFirmwareFailure("stage image: "+err.Error(), err)
	}

	a.Log.Info("downloading firmware", "model", ref.Model, "version", ref.Version, "size", ref.Size)
	// Streamed straight to the staging file: a tens-of-MB image buffered in RAM
	// is the largest allocation the agent ever makes, and it is avoidable.
	if err := a.client.DownloadFirmware(ctx, ref.Path, ref.Size, image, ref.SHA256); err != nil {
		return a.reportFirmwareFailure("download: "+err.Error(), err)
	}

	a.setFirmwareState(proto.FirmwareUpgrading,
		fmt.Sprintf("%s %s, sha256 ok", ref.Model, ref.Version))

	// Report before rebooting: the device may not get another hello out until it
	// is back up, and the controller should show "upgrading" meanwhile.
	a.notifyFirmware(ctx)

	a.Log.Info("scheduling sysupgrade", "image", image)
	if _, err := a.Runner.Run(ctx, "sh", "-c", sysupgradeCommand(image)); err != nil {
		return a.reportFirmwareFailure("schedule sysupgrade: "+err.Error(), err)
	}
	return nil
}

// firmwareImagePath is where the downloaded blob is staged. sysupgrade needs a
// real file, and /tmp is the one place guaranteed to be writable on OpenWrt.
func (a *Agent) firmwareImagePath() string {
	return filepath.Join(a.Config.Root, "tmp", "ac-firmware.img")
}

// sysupgradeCommand runs sysupgrade detached.
//
// Blocking would hold the heartbeat loop for the whole flash and make the AP
// look dead right before it reboots. The short sleep lets the "upgrading" report
// above reach the controller first.
func sysupgradeCommand(image string) string {
	return "sleep 2; sysupgrade -q " + shellQuote(image) + " >>/tmp/ac-sysupgrade.log 2>&1 &"
}

// shellQuote makes a path safe for the one place the agent builds a shell string.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// firmwareMatches reports whether the running firmware string names the target
// build. APs report DISTRIB_DESCRIPTION ("... build 20261026"), while the
// repository version is the build identifier an operator chooses, so containment
// is the only comparison available without inventing a second convention.
func firmwareMatches(running, version string) bool {
	running = strings.TrimSpace(running)
	version = strings.TrimSpace(version)
	if running == "" || version == "" {
		return false
	}
	return strings.Contains(strings.ToLower(running), strings.ToLower(version))
}

// setFirmwareState persists a state transition and retries no report: the hello
// loop already carries it on the next cycle.
func (a *Agent) setFirmwareState(state, message string) {
	a.state.FirmwareState = state
	a.state.FirmwareMessage = truncate(message, 500)
	a.state.FirmwareAt = time.Now().UTC().Format(time.RFC3339)
	if err := a.saveState(); err != nil {
		a.Log.Warn("could not persist firmware state", "err", err)
	}
}

// reportFirmwareFailure records a terminal failure and returns the cause so the
// cycle is logged once as a warning.
func (a *Agent) reportFirmwareFailure(message string, cause error) error {
	a.setFirmwareState(proto.FirmwareFailed, message)
	return cause
}

// notifyFirmware sends the state right away instead of waiting a full interval.
// Failure is not fatal: the persisted state goes out on the next hello anyway.
func (a *Agent) notifyFirmware(ctx context.Context) {
	if _, err := a.client.Hello(ctx, a.buildHello(ctx)); err != nil {
		a.Log.Warn("could not report firmware state yet, will retry", "err", err)
	}
}
