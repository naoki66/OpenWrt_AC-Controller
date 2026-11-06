package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/naoki66/ac-controller/internal/model"
)

// Template storage keeps the whole model.Template as JSON. The schema is still
// moving (P1 will add the parameterised VLAN pool and roaming policy), and a
// JSON column lets the renderer evolve without a migration per field.

// UpsertTemplate inserts or replaces a template.
func (s *Store) UpsertTemplate(ctx context.Context, t model.Template) error {
	body, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("encode template %s: %w", t.ID, err)
	}
	const q = `
INSERT INTO templates (id, name, json, updated_at) VALUES (?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	name       = excluded.name,
	json       = excluded.json,
	updated_at = excluded.updated_at`
	if _, err := s.db.ExecContext(ctx, q, t.ID, t.Name, string(body), time.Now().Unix()); err != nil {
		return fmt.Errorf("upsert template %s: %w", t.ID, err)
	}
	return nil
}

// GetTemplate returns one template or ErrNotFound.
func (s *Store) GetTemplate(ctx context.Context, id string) (model.Template, error) {
	const q = `SELECT json FROM templates WHERE id = ?`
	var body string
	err := s.db.QueryRowContext(ctx, q, id).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Template{}, ErrNotFound
	}
	if err != nil {
		return model.Template{}, err
	}

	var t model.Template
	if err := json.Unmarshal([]byte(body), &t); err != nil {
		return model.Template{}, fmt.Errorf("decode template %s: %w", id, err)
	}
	return t, nil
}

// ListTemplates returns every template ordered by id.
func (s *Store) ListTemplates(ctx context.Context) ([]model.Template, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT json FROM templates ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]model.Template, 0, 4)
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var t model.Template
		if err := json.Unmarshal([]byte(body), &t); err != nil {
			return nil, fmt.Errorf("decode template: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CountTemplates is used to decide whether the built-in example template should
// be seeded on first boot.
func (s *Store) CountTemplates(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM templates`).Scan(&n)
	return n, err
}

// TemplateRef is how many objects point at one template. Deleting a template
// that is still referenced would leave an AP or a group with an unresolvable
// configuration, so the API refuses instead of cascading silently.
type TemplateRef struct {
	Devices int `json:"devices"`
	Groups  int `json:"groups"`
}

// TemplateRefs returns reference counts for every template that is in use.
// Templates with no references are absent from the map, which keeps the
// response small and makes the common "delete an unused template" case clear.
func (s *Store) TemplateRefs(ctx context.Context) (map[string]TemplateRef, error) {
	out := map[string]TemplateRef{}
	rows, err := s.db.QueryContext(ctx,
		`SELECT template_id, COUNT(*) FROM devices WHERE template_id <> '' GROUP BY template_id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return nil, err
		}
		r := out[id]
		r.Devices = n
		out[id] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.db.QueryContext(ctx,
		`SELECT template_id, COUNT(*) FROM ap_groups WHERE template_id <> '' GROUP BY template_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		r := out[id]
		r.Groups = n
		out[id] = r
	}
	return out, rows.Err()
}

// CountTemplateRefs reports the total reference count for one template.
func (s *Store) CountTemplateRefs(ctx context.Context, id string) (TemplateRef, error) {
	var r TemplateRef
	err := s.db.QueryRowContext(ctx, `
SELECT (SELECT COUNT(*) FROM devices   WHERE template_id = ?),
       (SELECT COUNT(*) FROM ap_groups WHERE template_id = ?)`, id, id).
		Scan(&r.Devices, &r.Groups)
	return r, err
}

// DeleteTemplate removes an unreferenced template.
func (s *Store) DeleteTemplate(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM templates WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete template %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
