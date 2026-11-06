package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ClientMeta is controller-side intent about a station. It is deliberately
// separate from the periodic client snapshot: the AP would overwrite any extra
// column on every hello.
type ClientMeta struct {
	MAC       string `json:"mac"`
	Alias     string `json:"alias,omitempty"`
	Note      string `json:"note,omitempty"`
	Tracked   bool   `json:"tracked"`
	BindSSID  string `json:"bind_ssid,omitempty"`
	BindAPMAC string `json:"bind_ap_mac,omitempty"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
}

// GetClientMeta returns the metadata for one MAC. A missing row is not an
// error: it simply means "no operator intent yet".
func (s *Store) GetClientMeta(ctx context.Context, mac string) (ClientMeta, error) {
	var (
		m       ClientMeta
		tracked int
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT mac, alias, note, tracked, bind_ssid, bind_ap_mac, updated_at
		FROM client_meta WHERE mac = ?`, mac).
		Scan(&m.MAC, &m.Alias, &m.Note, &tracked, &m.BindSSID, &m.BindAPMAC, &m.UpdatedAt)
	if err == sql.ErrNoRows {
		return ClientMeta{MAC: mac}, nil
	}
	if err != nil {
		return ClientMeta{}, err
	}
	m.Tracked = tracked == 1
	return m, nil
}

// UpsertClientMeta stores the whole metadata row. updated_at is set here so
// callers cannot forget it.
func (s *Store) UpsertClientMeta(ctx context.Context, m ClientMeta) error {
	tracked := 0
	if m.Tracked {
		tracked = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO client_meta (mac, alias, note, tracked, bind_ssid, bind_ap_mac, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(mac) DO UPDATE SET
			alias = excluded.alias,
			note = excluded.note,
			tracked = excluded.tracked,
			bind_ssid = excluded.bind_ssid,
			bind_ap_mac = excluded.bind_ap_mac,
			updated_at = excluded.updated_at`,
		m.MAC, m.Alias, m.Note, tracked, m.BindSSID, m.BindAPMAC, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("upsert client meta %s: %w", m.MAC, err)
	}
	return nil
}

// ListClientBindings returns the stations that should prefer a specific SSID
// (and optionally one AP). Disabled/empty bindings are excluded because the
// agent must not receive intent it cannot act on.
func (s *Store) ListClientBindings(ctx context.Context) ([]ClientMeta, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT mac, bind_ssid, bind_ap_mac
		FROM client_meta
		WHERE bind_ssid <> ''
		ORDER BY mac`)
	if err != nil {
		return nil, fmt.Errorf("list client bindings: %w", err)
	}
	defer rows.Close()

	var out []ClientMeta
	for rows.Next() {
		var m ClientMeta
		if err := rows.Scan(&m.MAC, &m.BindSSID, &m.BindAPMAC); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
