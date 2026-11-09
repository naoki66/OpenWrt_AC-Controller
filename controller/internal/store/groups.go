package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// APGroup binds a set of APs to one template. Membership lives on the device
// row (devices.group_id) so listing devices stays a single-table query; the
// device count is derived when groups are listed.
type APGroup struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	TemplateID string `json:"template_id"`
	// DeviceCount is computed on read, never stored.
	DeviceCount int   `json:"device_count"`
	CreatedAt   int64 `json:"created_at"`
	UpdatedAt   int64 `json:"updated_at"`
}

// CreateGroup inserts a new group and returns it with its generated ID.
func (s *Store) CreateGroup(ctx context.Context, name, templateID string) (APGroup, error) {
	if name == "" {
		return APGroup{}, errors.New("store: group needs a name")
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return APGroup{}, fmt.Errorf("store: group id: %w", err)
	}
	now := time.Now().Unix()
	g := APGroup{
		ID:         hex.EncodeToString(buf),
		Name:       name,
		TemplateID: templateID,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	const q = `INSERT INTO ap_groups (id, name, template_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`
	if _, err := s.db.ExecContext(ctx, q, g.ID, g.Name, g.TemplateID, g.CreatedAt, g.UpdatedAt); err != nil {
		return APGroup{}, fmt.Errorf("store: create group: %w", err)
	}
	return g, nil
}

// UpsertGroup inserts or replaces a group while keeping the ID the caller
// supplied. CreateGroup generates a random ID because the UI has no opinion,
// but a configuration restore must reproduce the membership references that
// devices already store, so it needs the original ID to survive the round trip.
func (s *Store) UpsertGroup(ctx context.Context, g APGroup) error {
	if g.ID == "" || g.Name == "" {
		return errors.New("store: group needs an id and a name")
	}
	now := time.Now().Unix()
	if g.CreatedAt == 0 {
		g.CreatedAt = now
	}
	const q = `
INSERT INTO ap_groups (id, name, template_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	name        = excluded.name,
	template_id = excluded.template_id,
	updated_at  = excluded.updated_at`
	if _, err := s.db.ExecContext(ctx, q, g.ID, g.Name, g.TemplateID, g.CreatedAt, now); err != nil {
		return fmt.Errorf("store: upsert group %s: %w", g.ID, err)
	}
	return nil
}

// GetGroup returns one group or ErrNotFound.
func (s *Store) GetGroup(ctx context.Context, id string) (APGroup, error) {
	const q = `
SELECT g.id, g.name, g.template_id, g.created_at, g.updated_at,
       (SELECT COUNT(*) FROM devices d WHERE d.group_id = g.id)
FROM ap_groups g WHERE g.id = ?`
	var g APGroup
	err := s.db.QueryRowContext(ctx, q, id).Scan(
		&g.ID, &g.Name, &g.TemplateID, &g.CreatedAt, &g.UpdatedAt, &g.DeviceCount)
	if errors.Is(err, sql.ErrNoRows) {
		return APGroup{}, ErrNotFound
	}
	if err != nil {
		return APGroup{}, err
	}
	return g, nil
}

// ListGroups returns every group with its current member count.
func (s *Store) ListGroups(ctx context.Context) ([]APGroup, error) {
	const q = `
SELECT g.id, g.name, g.template_id, g.created_at, g.updated_at,
       (SELECT COUNT(*) FROM devices d WHERE d.group_id = g.id)
FROM ap_groups g ORDER BY g.name, g.id`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]APGroup, 0, 4)
	for rows.Next() {
		var g APGroup
		if err := rows.Scan(&g.ID, &g.Name, &g.TemplateID, &g.CreatedAt, &g.UpdatedAt, &g.DeviceCount); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// UpdateGroup replaces a group's name and template binding. An empty
// templateID clears the binding; members keep whatever template they already
// run until a re-push happens.
func (s *Store) UpdateGroup(ctx context.Context, id, name, templateID string) error {
	if name == "" {
		return errors.New("store: group needs a name")
	}
	const q = `UPDATE ap_groups SET name = ?, template_id = ?, updated_at = ? WHERE id = ?`
	res, err := s.db.ExecContext(ctx, q, name, templateID, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("store: update group %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteGroup removes a group. The caller is responsible for refusing to
// delete one that still has members: silently detaching them would hide the
// template change from an operator who only meant to clean up an empty group.
func (s *Store) DeleteGroup(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM ap_groups WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete group %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountGroupDevices reports how many devices currently reference the group.
func (s *Store) CountGroupDevices(ctx context.Context, id string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM devices WHERE group_id = ?`, id).Scan(&n)
	return n, err
}

// SetDeviceGroup records group membership. An empty groupID detaches the
// device without touching its template.
func (s *Store) SetDeviceGroup(ctx context.Context, mac, groupID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET group_id = ? WHERE mac = ?`, groupID, mac)
	if err != nil {
		return fmt.Errorf("store: set group for %s: %w", mac, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
