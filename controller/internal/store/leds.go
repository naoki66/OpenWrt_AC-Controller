package store

import (
	"context"
	"fmt"
)

// SetLEDSchedule stores a per-device daily status-LED switch policy. An empty
// on/off time disables that edge and clears its due time.
func (s *Store) SetLEDSchedule(ctx context.Context, mac, onAt, offAt string, onNext, offNext int64) error {
	if onAt == "" {
		onNext = 0
	}
	if offAt == "" {
		offNext = 0
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET led_on_at = ?, led_off_at = ?, led_on_next_at = ?, led_off_next_at = ? WHERE mac = ?`,
		onAt, offAt, onNext, offNext, mac)
	if err != nil {
		return fmt.Errorf("set LED schedule %s: %w", mac, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DueLEDSchedules lists online, adopted devices with at least one LED edge
// due. onlineAfter is the same silence window the UI uses, so a schedule never
// fires at an AP that would only collect an expired action.
func (s *Store) DueLEDSchedules(ctx context.Context, now, onlineAfter int64, limit int) ([]Device, error) {
	if limit <= 0 || limit > 128 {
		limit = 32
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+deviceColumns+` FROM devices
		  WHERE state = ? AND (
		    (led_on_next_at > 0 AND led_on_next_at <= ?) OR
		    (led_off_next_at > 0 AND led_off_next_at <= ?)
		  ) AND last_seen >= ?
		  ORDER BY
		    CASE WHEN led_on_next_at > 0 THEN led_on_next_at ELSE 9223372036854775807 END,
		    CASE WHEN led_off_next_at > 0 THEN led_off_next_at ELSE 9223372036854775807 END
		  LIMIT ?`,
		StateAdopted, now, now, now-onlineAfter, limit)
	if err != nil {
		return nil, fmt.Errorf("list due LED schedules: %w", err)
	}
	defer rows.Close()

	var out []Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, fmt.Errorf("scan due LED schedule: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due LED schedules: %w", err)
	}
	return out, nil
}

// AdvanceLEDSchedule moves one edge's due time forward. Only the selected
// edge is advanced: if both edges were missed while the controller was down,
// the other stays due and fires on the next scheduler tick.
func (s *Store) AdvanceLEDSchedule(ctx context.Context, mac string, on bool, nextAt int64) error {
	column := "led_off_next_at"
	if on {
		column = "led_on_next_at"
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET `+column+` = ? WHERE mac = ?`, nextAt, mac)
	if err != nil {
		return fmt.Errorf("advance LED schedule %s: %w", mac, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
