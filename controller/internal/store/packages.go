package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Package is one signed configuration revision exactly as it was handed out.
// The blob is kept verbatim so that "what did the AP actually download" stays
// answerable after the template has moved on.
type Package struct {
	MAC       string `json:"mac"`
	Rev       int    `json:"rev"`
	CreatedAt int64  `json:"created_at"`
	SHA256    string `json:"sha256"`
	Size      int    `json:"size"`
}

// CreatePackage stores a built revision. Revisions are immutable: attempting to
// store the same rev twice fails instead of silently overwriting history.
func (s *Store) CreatePackage(ctx context.Context, mac string, rev int, sha256 string, blob []byte) error {
	const q = `
INSERT INTO packages (mac, rev, created_at, sha256, size, blob)
VALUES (?, ?, strftime('%s','now'), ?, ?, ?)`
	if _, err := s.db.ExecContext(ctx, q, mac, rev, sha256, len(blob), blob); err != nil {
		return fmt.Errorf("store package %s rev %d: %w", mac, rev, err)
	}
	return nil
}

// TrimPackages keeps only the newest keep revisions for one device. Zero means
// unlimited. The signed blobs are the largest rows in the database, so a
// flash-constrained deployment wants this at 1; a controller with room to spare
// leaves it at 0 and keeps the full audit trail.
func (s *Store) TrimPackages(ctx context.Context, mac string, keep int) error {
	if keep <= 0 {
		return nil
	}
	const q = `
DELETE FROM packages WHERE mac = ? AND rev NOT IN (
	SELECT rev FROM packages WHERE mac = ? ORDER BY rev DESC LIMIT ?)`
	if _, err := s.db.ExecContext(ctx, q, mac, mac, keep); err != nil {
		return fmt.Errorf("trim packages %s: %w", mac, err)
	}
	return nil
}

// LatestPackage returns the newest stored revision for a device.
func (s *Store) LatestPackage(ctx context.Context, mac string) (Package, []byte, error) {
	const q = `
SELECT rev, created_at, sha256, size, blob
FROM packages WHERE mac = ? ORDER BY rev DESC LIMIT 1`
	return s.scanPackage(s.db.QueryRowContext(ctx, q, mac))
}

// PackageByRev returns a specific revision.
func (s *Store) PackageByRev(ctx context.Context, mac string, rev int) (Package, []byte, error) {
	const q = `
SELECT rev, created_at, sha256, size, blob
FROM packages WHERE mac = ? AND rev = ?`
	return s.scanPackage(s.db.QueryRowContext(ctx, q, mac, rev))
}

func (s *Store) scanPackage(row *sql.Row) (Package, []byte, error) {
	var (
		p    Package
		blob []byte
	)
	err := row.Scan(&p.Rev, &p.CreatedAt, &p.SHA256, &p.Size, &blob)
	if errors.Is(err, sql.ErrNoRows) {
		return Package{}, nil, ErrNotFound
	}
	if err != nil {
		return Package{}, nil, err
	}
	return p, blob, nil
}

// ListPackages returns revision metadata without the blobs.
func (s *Store) ListPackages(ctx context.Context, mac string) ([]Package, error) {
	const q = `
SELECT rev, created_at, sha256, size
FROM packages WHERE mac = ? ORDER BY rev DESC`
	rows, err := s.db.QueryContext(ctx, q, mac)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Package, 0, 4)
	for rows.Next() {
		p := Package{MAC: mac}
		if err := rows.Scan(&p.Rev, &p.CreatedAt, &p.SHA256, &p.Size); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
