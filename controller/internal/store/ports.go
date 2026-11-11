package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// PortState is one wired port as last measured on an AP.
type PortState struct {
	APMAC     string `json:"ap_mac"`
	Name      string `json:"name"`
	MAC       string `json:"mac,omitempty"`
	Up        bool   `json:"up"`
	SpeedMbps int    `json:"speed_mbps,omitempty"`
	Duplex    string `json:"duplex,omitempty"`
	MTU       int    `json:"mtu,omitempty"`
	UpdatedAt int64  `json:"updated_at"`
}

// ReplacePortsForAP converges the now-view onto the set the AP just reported.
// A renamed or removed port disappears on the next hello, matching the radio
// state contract: the caller never calls this with a nil list.
func (s *Store) ReplacePortsForAP(ctx context.Context, apMAC string, ports []PortState) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("port state tx: %w", err)
	}
	defer tx.Rollback()

	const insert = `INSERT INTO port_state
		(ap_mac, name, mac, up, speed_mbps, duplex, mtu, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(ap_mac, name) DO UPDATE SET
			mac=excluded.mac, up=excluded.up, speed_mbps=excluded.speed_mbps,
			duplex=excluded.duplex, mtu=excluded.mtu, updated_at=excluded.updated_at
		WHERE (port_state.mac, port_state.up, port_state.speed_mbps, port_state.duplex, port_state.mtu)
		   IS NOT (excluded.mac, excluded.up, excluded.speed_mbps, excluded.duplex, excluded.mtu)
		   OR port_state.updated_at <= excluded.updated_at - ?`
	now := time.Now().Unix()
	names := make([]string, 0, len(ports))
	for _, p := range ports {
		names = append(names, p.Name)
		up := 0
		if p.Up {
			up = 1
		}
		if _, err := tx.ExecContext(ctx, insert,
			apMAC, p.Name, p.MAC, up, p.SpeedMbps, p.Duplex, p.MTU, now, stateRefreshSeconds); err != nil {
			return fmt.Errorf("insert port %s: %w", p.Name, err)
		}
	}
	encoded, err := json.Marshal(names)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM port_state
		WHERE ap_mac = ? AND name NOT IN (SELECT value FROM json_each(?))`, apMAC, string(encoded)); err != nil {
		return fmt.Errorf("remove stale ports: %w", err)
	}
	return tx.Commit()
}

// LatestPorts returns every AP's wired ports, ordered by AP and port name.
func (s *Store) LatestPorts(ctx context.Context) ([]PortState, error) {
	const q = `SELECT ap_mac, name, mac, up, speed_mbps, duplex, mtu, updated_at
FROM port_state ORDER BY ap_mac, name`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]PortState, 0, 16)
	for rows.Next() {
		var (
			p  PortState
			up int
		)
		if err := rows.Scan(&p.APMAC, &p.Name, &p.MAC, &up,
			&p.SpeedMbps, &p.Duplex, &p.MTU, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.Up = up == 1
		out = append(out, p)
	}
	return out, rows.Err()
}
