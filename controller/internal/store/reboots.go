package store

import (
	"context"
	"fmt"
)

// SetRebootSchedule stores a per-device periodic reboot policy. days == 0
// disables it and clears the due time; weekday (0-6) and monthday (1-28)
// select the weekly/monthly frequency (sentinels -1/0 mean "not applicable").
func (s *Store) SetRebootSchedule(ctx context.Context, mac string, days, weekday, monthday int, at string, nextAt int64) error {
	if days <= 0 {
		days, weekday, monthday, at, nextAt = 0, -1, 0, "", 0
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET reboot_period_days = ?, reboot_at = ?, reboot_next_at = ?,
		   reboot_weekday = ?, reboot_monthday = ? WHERE mac = ?`,
		days, at, nextAt, weekday, monthday, mac)
	if err != nil {
		return fmt.Errorf("set reboot schedule %s: %w", mac, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DueReboots lists online, adopted devices whose scheduled reboot time has
// passed. onlineAfter is the same silence window the UI uses, so a schedule
// never fires at an AP that would only collect an expired action.
func (s *Store) DueReboots(ctx context.Context, now, onlineAfter int64, limit int) ([]Device, error) {
	if limit <= 0 || limit > 128 {
		limit = 32
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+deviceColumns+` FROM devices
	  WHERE state = ? AND reboot_period_days > 0 AND reboot_next_at > 0
	    AND reboot_next_at <= ? AND last_seen >= ?
	  ORDER BY reboot_next_at LIMIT ?`,
		StateAdopted, now, now-onlineAfter, limit)
	if err != nil {
		return nil, fmt.Errorf("list due reboots: %w", err)
	}
	defer rows.Close()

	var out []Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, fmt.Errorf("scan due reboot: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due reboots: %w", err)
	}
	return out, nil
}

// AdvanceRebootSchedule moves one device's due time forward. The scheduler
// advances even when a reboot is already pending: the manual command in flight
// is enough, and advancing prevents a second one right after it completes.
func (s *Store) AdvanceRebootSchedule(ctx context.Context, mac string, nextAt int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET reboot_next_at = ? WHERE mac = ?`, nextAt, mac)
	if err != nil {
		return fmt.Errorf("advance reboot schedule %s: %w", mac, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// HasPendingAction reports whether an action of this type is already queued for
// the device, so the scheduler does not stack duplicates.
func (s *Store) HasPendingAction(ctx context.Context, mac, typ string) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM device_actions WHERE mac = ? AND type = ? AND state = ?`,
		mac, typ, ActionPending).Scan(&n); err != nil {
		return false, fmt.Errorf("check pending action %s: %w", mac, err)
	}
	return n > 0, nil
}
