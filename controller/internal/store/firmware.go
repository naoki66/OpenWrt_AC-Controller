package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Firmware upgrade (P4).
//
// The repository stores whole images keyed by (model, version). It is deliberately
// small: two APs, a handful of builds. The digest is what the agent verifies
// before calling sysupgrade, so a corrupted upload can never be flashed.
//
// The device row keeps only the target and the agent's last report. "Is this AP
// on the target?" is answered by the firmware string the AP sends in every hello,
// never by assuming an upgrade succeeded.

// ErrAlreadyExists is returned when a firmware version is uploaded twice. It is
// separate from ErrNotFound because both surface as 4xx and the caller must be
// able to tell "you already have this" from "there is no such build".
var ErrAlreadyExists = errors.New("already exists")

// Firmware is the metadata of one image in the repository; the blob is omitted
// from list responses so a fleet listing stays small.
type Firmware struct {
	Model      string `json:"model"`
	Version    string `json:"version"`
	Filename   string `json:"filename,omitempty"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	UploadedAt int64  `json:"uploaded_at"`
}

// SaveFirmware stores one image. A duplicate (model, version) is refused rather
// than overwritten: replacing the bytes behind a version an AP is currently
// flashing is exactly the kind of surprise that bricks hardware.
func (s *Store) SaveFirmware(ctx context.Context, fw Firmware, blob []byte) error {
	if strings.TrimSpace(fw.Model) == "" || strings.TrimSpace(fw.Version) == "" {
		return errors.New("firmware model and version are required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO firmwares (model, version, filename, size, sha256, uploaded_at, blob)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		fw.Model, fw.Version, fw.Filename, fw.Size, fw.SHA256, time.Now().Unix(), blob)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			return ErrAlreadyExists
		}
		return fmt.Errorf("save firmware %s/%s: %w", fw.Model, fw.Version, err)
	}
	return nil
}

// ListFirmwares returns every image without its blob.
func (s *Store) ListFirmwares(ctx context.Context) ([]Firmware, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT model, version, filename, size, sha256, uploaded_at
		 FROM firmwares ORDER BY model, version DESC`)
	if err != nil {
		return nil, fmt.Errorf("list firmwares: %w", err)
	}
	defer rows.Close()

	out := make([]Firmware, 0, 8)
	for rows.Next() {
		var fw Firmware
		if err := rows.Scan(&fw.Model, &fw.Version, &fw.Filename, &fw.Size, &fw.SHA256, &fw.UploadedAt); err != nil {
			return nil, fmt.Errorf("list firmwares scan: %w", err)
		}
		out = append(out, fw)
	}
	return out, rows.Err()
}

// GetFirmware returns one image including its bytes.
func (s *Store) GetFirmware(ctx context.Context, model, version string) (Firmware, []byte, error) {
	var (
		fw   Firmware
		blob []byte
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT model, version, filename, size, sha256, uploaded_at, blob
		 FROM firmwares WHERE model = ? AND version = ?`, model, version).
		Scan(&fw.Model, &fw.Version, &fw.Filename, &fw.Size, &fw.SHA256, &fw.UploadedAt, &blob)
	if errors.Is(err, sql.ErrNoRows) {
		return Firmware{}, nil, ErrNotFound
	}
	if err != nil {
		return Firmware{}, nil, fmt.Errorf("get firmware %s/%s: %w", model, version, err)
	}
	return fw, blob, nil
}

// DeleteFirmware removes one image. It does not clear device targets; the next
// hello then simply finds no image and sends nothing.
func (s *Store) DeleteFirmware(ctx context.Context, model, version string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM firmwares WHERE model = ? AND version = ?`, model, version)
	if err != nil {
		return fmt.Errorf("delete firmware %s/%s: %w", model, version, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetFirmwareTarget asks one AP to run a version. An empty version clears the
// request and the last report, which is how an operator says "stop caring".
// A non-empty version always resets the state so a retry after a failure is
// possible: the failure state is what suppresses resending.
func (s *Store) SetFirmwareTarget(ctx context.Context, mac, version string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET fw_target = ?, fw_state = '', fw_message = '', fw_at = 0 WHERE mac = ?`,
		strings.TrimSpace(version), mac)
	if err != nil {
		return fmt.Errorf("set firmware target %s: %w", mac, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordFirmware stores the agent's report. A terminal state is also the end of
// the instruction: once applied, the controller must not keep handing the same
// image to a device that is already running it.
func (s *Store) RecordFirmware(ctx context.Context, mac, state, message string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET fw_state = ?, fw_message = ?, fw_at = strftime('%s','now') WHERE mac = ?`,
		state, message, mac)
	if err != nil {
		return fmt.Errorf("record firmware %s: %w", mac, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
