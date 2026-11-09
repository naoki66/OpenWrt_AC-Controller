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

// ListEvents returns the newest events, newest first. Offset exists so the
// system page can walk further back than the first page without loading the
// whole audit trail into memory; ordering is stable enough to page on because
// the sort key ends with the autoincrement id. A non-empty kind filters by
// prefix (e.g. "fw_" for the firmware upgrade log); prefix matching uses
// substr instead of LIKE so underscores in kind names stay literal.
func (s *Store) ListEvents(ctx context.Context, limit, offset int, kind string) ([]Event, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	q := `SELECT id, ts, kind, mac, message FROM events`
	args := []any{}
	if kind != "" {
		q += ` WHERE substr(kind, 1, length(?)) = ?`
		args = append(args, kind, kind)
	}
	q += ` ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
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
