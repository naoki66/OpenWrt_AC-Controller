package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Device action states.
const (
	ActionPending = "pending"
	ActionDone    = "done"
	ActionFailed  = "failed"
	ActionExpired = "expired"
)

const (
	// ActionDispatchTimeout is how long a dispatched action waits for a result
	// before it is marked expired. The agent reports results on the next hello,
	// so this only needs to cover one or two missed heartbeats.
	ActionDispatchTimeout = 10 * time.Minute
	// ActionQueueTTL bounds how long an action may wait for an offline AP.
	ActionQueueTTL = 24 * time.Hour
)

// Action is one queued controller-to-AP command.
type Action struct {
	ID   string            `json:"id"`
	MAC  string            `json:"mac"`
	Type string            `json:"type"`
	Args map[string]string `json:"args,omitempty"`
	// State is pending until the AP reports a result, then done/failed. An
	// action nobody executed before its deadline becomes expired.
	State        string `json:"state"`
	Message      string `json:"message,omitempty"`
	CreatedAt    int64  `json:"created_at"`
	DispatchedAt int64  `json:"dispatched_at,omitempty"`
	FinishedAt   int64  `json:"finished_at,omitempty"`
}

// CreateAction queues one command for mac and returns it with its generated ID.
func (s *Store) CreateAction(ctx context.Context, mac, typ string, args map[string]string) (Action, error) {
	if mac == "" || typ == "" {
		return Action{}, errors.New("store: action needs a mac and a type")
	}
	body := "{}"
	if len(args) > 0 {
		raw, err := json.Marshal(args)
		if err != nil {
			return Action{}, fmt.Errorf("store: encode action args: %w", err)
		}
		body = string(raw)
	}

	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return Action{}, fmt.Errorf("store: action id: %w", err)
	}
	a := Action{
		ID:        hex.EncodeToString(buf),
		MAC:       mac,
		Type:      typ,
		Args:      args,
		State:     ActionPending,
		CreatedAt: time.Now().Unix(),
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO device_actions (id, mac, type, args, state, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		a.ID, a.MAC, a.Type, body, a.State, a.CreatedAt); err != nil {
		return Action{}, fmt.Errorf("store: insert action: %w", err)
	}
	return a, nil
}

// PendingActions returns the commands still waiting for a result, oldest first.
// It expires actions whose deadline passed in the same call so a queue never
// grows without bound when an AP stays offline or a result is lost.
func (s *Store) PendingActions(ctx context.Context, mac string, limit int) ([]Action, error) {
	if limit <= 0 || limit > 64 {
		limit = 16
	}
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE device_actions SET state = ?, finished_at = ?, message = ?
		  WHERE mac = ? AND state = ? AND (
		    (dispatched_at > 0 AND dispatched_at < ?) OR created_at < ?)`,
		ActionExpired, now, "no result before deadline",
		mac, ActionPending,
		now-int64(ActionDispatchTimeout.Seconds()), now-int64(ActionQueueTTL.Seconds())); err != nil {
		return nil, fmt.Errorf("store: expire actions: %w", err)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, mac, type, args, state, message, created_at, dispatched_at, finished_at
		   FROM device_actions WHERE mac = ? AND state = ?
		  ORDER BY created_at LIMIT ?`,
		mac, ActionPending, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list pending actions: %w", err)
	}
	defer rows.Close()
	return scanActions(rows)
}

// MarkActionsDispatched records that the given actions were included in a
// hello reply. It does not change their state: only a result or the deadline
// does that.
func (s *Store) MarkActionsDispatched(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids)+2)
	args = append(args, time.Now().Unix(), ActionPending)
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE device_actions SET dispatched_at = ? WHERE state = ? AND id IN (`+
			strings.Join(placeholders, ",")+`)`, args...); err != nil {
		return fmt.Errorf("store: mark actions dispatched: %w", err)
	}
	return nil
}

// CompleteAction stores the AP's result for one action. A missing row or one
// already finished is not an error: results can legitimately arrive twice.
func (s *Store) CompleteAction(ctx context.Context, mac, id, state, message string) (bool, error) {
	if state != ActionDone && state != ActionFailed {
		return false, fmt.Errorf("store: invalid action result %q", state)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE device_actions SET state = ?, message = ?, finished_at = ?
		  WHERE id = ? AND mac = ? AND state = ?`,
		state, message, time.Now().Unix(), id, mac, ActionPending)
	if err != nil {
		return false, fmt.Errorf("store: complete action: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListActions returns the newest actions for one AP, most recent first.
func (s *Store) ListActions(ctx context.Context, mac string, limit int) ([]Action, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, mac, type, args, state, message, created_at, dispatched_at, finished_at
		   FROM device_actions WHERE mac = ?
		  ORDER BY created_at DESC LIMIT ?`,
		mac, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list actions: %w", err)
	}
	defer rows.Close()
	return scanActions(rows)
}

func scanActions(rows *sql.Rows) ([]Action, error) {
	var out []Action
	for rows.Next() {
		var (
			a    Action
			args string
		)
		if err := rows.Scan(&a.ID, &a.MAC, &a.Type, &args, &a.State, &a.Message,
			&a.CreatedAt, &a.DispatchedAt, &a.FinishedAt); err != nil {
			return nil, fmt.Errorf("store: scan action: %w", err)
		}
		if args != "" && args != "{}" && args != "null" {
			if err := json.Unmarshal([]byte(args), &a.Args); err != nil {
				return nil, fmt.Errorf("store: decode action args: %w", err)
			}
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate actions: %w", err)
	}
	return out, nil
}
