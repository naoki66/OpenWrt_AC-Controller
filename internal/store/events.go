package store

import (
	"context"
	"fmt"
)

// Event is one row of the controller's audit trail. The table is append-only:
// nothing reads it back except the system page, so a query is deliberately
// limited to the newest rows instead of offering arbitrary filtering.
type Event struct {
	ID      int64  `json:"id"`
	TS      int64  `json:"ts"`
	Kind    string `json:"kind"`
	MAC     string `json:"mac,omitempty"`
	Message string `json:"message"`
}

// ListEvents returns the newest events, newest first.
func (s *Store) ListEvents(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ts, kind, mac, message FROM events ORDER BY ts DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	out := make([]Event, 0, limit)
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TS, &e.Kind, &e.MAC, &e.Message); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountEvents reports how many audit rows exist. It is cheap because the table
// has a primary key and the system page only needs the total.
func (s *Store) CountEvents(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&n)
	return n, err
}
