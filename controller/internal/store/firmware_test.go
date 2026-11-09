package store

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// TestFirmwareStoreRoundTrip covers the blob path specifically: listing must not
// read the image (a fleet listing would otherwise load hundreds of MB), while
// GetFirmware must return the exact bytes the digest was computed over.
func TestFirmwareStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "ac.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	blob := []byte("image-bytes")
	fw := Firmware{Model: "XR1710G", Version: "20261026", Filename: "x.img", Size: int64(len(blob)), SHA256: "abc"}
	if err := s.SaveFirmware(ctx, fw, blob); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.SaveFirmware(ctx, fw, blob); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("duplicate save err = %v, want ErrAlreadyExists", err)
	}

	list, err := s.ListFirmwares(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Version != "20261026" || list[0].Size != int64(len(blob)) {
		t.Errorf("list = %+v", list)
	}

	got, gotBlob, err := s.GetFirmware(ctx, "XR1710G", "20261026")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SHA256 != "abc" || !bytes.Equal(gotBlob, blob) {
		t.Errorf("get = %+v, blob = %q", got, gotBlob)
	}

	if err := s.DeleteFirmware(ctx, "XR1710G", "20261026"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, _, err := s.GetFirmware(ctx, "XR1710G", "20261026"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete err = %v, want ErrNotFound", err)
	}
}

// The device columns exist for both a freshly created and a legacy database, so
// the target/report path is exercised here as well.
func TestFirmwareTargetAndReport(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "ac.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := s.UpsertDiscovered(ctx, Device{MAC: "8c:de:52:11:22:33"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := s.SetFirmwareTarget(ctx, "8c:de:52:11:22:33", "20261026"); err != nil {
		t.Fatalf("set target: %v", err)
	}
	if err := s.RecordFirmware(ctx, "8c:de:52:11:22:33", "upgrading", "sha256 ok"); err != nil {
		t.Fatalf("record: %v", err)
	}

	d, err := s.GetDevice(ctx, "8c:de:52:11:22:33")
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if d.FwTarget != "20261026" || d.FWState != "upgrading" || d.FWAt == 0 {
		t.Errorf("device = %+v", d)
	}

	// Setting a target again resets the last report so the new attempt starts
	// clean instead of inheriting a stale failure.
	if err := s.SetFirmwareTarget(ctx, "8c:de:52:11:22:33", "20261026"); err != nil {
		t.Fatalf("reset target: %v", err)
	}
	d, _ = s.GetDevice(ctx, "8c:de:52:11:22:33")
	if d.FWState != "" || d.FWMessage != "" || d.FWAt != 0 {
		t.Errorf("report not reset: %+v", d)
	}

	if err := s.SetFirmwareTarget(ctx, "aa:bb:cc:dd:ee:ff", "20261026"); !errors.Is(err, ErrNotFound) {
		t.Errorf("set target on unknown device err = %v", err)
	}
}

// A firmware state transition lands in the audit trail with an fw_ prefix —
// that is what the firmware page's upgrade log renders. Repeats of the same
// state (the agent reports it in every hello during a phase) must not create
// more rows, and the kind prefix filter must return exactly those rows.
func TestFirmwareTransitionEventsAndKindFilter(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "ac.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := s.UpsertDiscovered(ctx, Device{MAC: "8c:de:52:11:22:33"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.SetFirmwareTarget(ctx, "8c:de:52:11:22:33", "20261026"); err != nil {
		t.Fatalf("set target: %v", err)
	}

	for _, report := range []struct{ state, msg string }{
		{"downloading", "6.2 MB"},
		{"downloading", "6.2 MB"}, // 同一阶段内的重复上报：不得再记一条
		{"upgrading", "writing flash"},
		{"applied", "sysupgrade ok"},
		{"applied", "sysupgrade ok"}, // 重复
	} {
		if err := s.RecordFirmware(ctx, "8c:de:52:11:22:33", report.state, report.msg); err != nil {
			t.Fatalf("record %s: %v", report.state, err)
		}
	}

	fwEvents, err := s.ListEvents(ctx, 50, 0, "fw_")
	if err != nil {
		t.Fatalf("list fw events: %v", err)
	}
	if len(fwEvents) != 3 {
		t.Fatalf("fw events = %d, want 3 (one per transition)", len(fwEvents))
	}
	// Newest first: applied, upgrading, downloading.
	if fwEvents[0].Kind != "fw_applied" || fwEvents[1].Kind != "fw_upgrading" || fwEvents[2].Kind != "fw_downloading" {
		t.Errorf("kinds = %s/%s/%s, want applied/upgrading/downloading",
			fwEvents[0].Kind, fwEvents[1].Kind, fwEvents[2].Kind)
	}
	if fwEvents[0].Message != "version 20261026: sysupgrade ok" {
		t.Errorf("message = %q, want the target version with the agent detail", fwEvents[0].Message)
	}

	// The prefix filter must not leak other kinds into the result, and the
	// unfiltered listing must still return everything.
	all, err := s.ListEvents(ctx, 50, 0, "")
	if err != nil {
		t.Fatalf("list all events: %v", err)
	}
	if len(all) < len(fwEvents) {
		t.Errorf("unfiltered events = %d, want at least the fw rows", len(all))
	}
	for _, e := range fwEvents {
		if e.MAC != "8c:de:52:11:22:33" {
			t.Errorf("event mac = %q, want the reporting device", e.MAC)
		}
	}
}
