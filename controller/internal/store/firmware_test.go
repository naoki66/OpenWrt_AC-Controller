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
