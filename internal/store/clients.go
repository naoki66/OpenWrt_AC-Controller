package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Client is one associated station as last reported by an AP.
type Client struct {
	MAC              string `json:"mac"`
	APMAC            string `json:"ap_mac"`
	IP               string `json:"ip"`
	SSID             string `json:"ssid"`
	Ifname           string `json:"ifname"`
	Band             string `json:"band"`
	Signal           int    `json:"signal_dbm"`
	RxRate           int    `json:"rx_rate_mbps"`
	TxRate           int    `json:"tx_rate_mbps"`
	ConnectedSeconds int64  `json:"connected_seconds"`
	FirstSeen        int64  `json:"first_seen"`
	LastSeen         int64  `json:"last_seen"`
	Online           bool   `json:"online"`
	// Operator intent merged from client_meta. They are kept out of the
	// snapshot columns because the AP's report would otherwise erase them.
	Alias    string `json:"alias,omitempty"`
	Note     string `json:"note,omitempty"`
	Tracked  bool   `json:"tracked"`
	BindSSID string `json:"bind_ssid,omitempty"`
	BindAP   string `json:"bind_ap_mac,omitempty"`
}

// ReplaceClientsForAP applies a full station list from one AP.
//
// The whole list is authoritative: every client previously attributed to this AP
// is first marked offline, then the reported ones are upserted back to online.
// Deltas would be cheaper but a single lost hello would then leave phantom
// clients on the dashboard forever.
func (s *Store) ReplaceClientsForAP(ctx context.Context, apMAC string, clients []Client) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("clients tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`UPDATE clients SET online = 0 WHERE ap_mac = ?`, apMAC); err != nil {
		return fmt.Errorf("mark clients offline: %w", err)
	}

	const upsert = `
INSERT INTO clients (mac, ap_mac, ip, ssid, ifname, band, signal, rx_rate, tx_rate,
                     connected_since, first_seen, last_seen, online)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
ON CONFLICT(mac) DO UPDATE SET
	ap_mac          = excluded.ap_mac,
	ip              = excluded.ip,
	ssid            = excluded.ssid,
	ifname          = excluded.ifname,
	band            = excluded.band,
	signal          = excluded.signal,
	rx_rate         = excluded.rx_rate,
	tx_rate         = excluded.tx_rate,
	connected_since = excluded.connected_since,
	last_seen       = excluded.last_seen,
	online          = 1`

	now := time.Now().Unix()
	for _, c := range clients {
		firstSeen := now
		if c.ConnectedSeconds > 0 {
			firstSeen = now - c.ConnectedSeconds
		}
		if _, err := tx.ExecContext(ctx, upsert,
			c.MAC, apMAC, c.IP, c.SSID, c.Ifname, c.Band,
			c.Signal, c.RxRate, c.TxRate, firstSeen, firstSeen, now,
		); err != nil {
			return fmt.Errorf("upsert client %s: %w", c.MAC, err)
		}
	}
	return tx.Commit()
}

// ListClients returns every known client, newest association first, with the
// operator metadata joined in.
func (s *Store) ListClients(ctx context.Context) ([]Client, error) {
	const q = `
SELECT c.mac, c.ap_mac, c.ip, c.ssid, c.ifname, c.band, c.signal, c.rx_rate, c.tx_rate,
       CASE WHEN c.last_seen > c.connected_since THEN c.last_seen - c.connected_since ELSE 0 END,
       c.connected_since, c.last_seen, c.online,
       COALESCE(m.alias, ''), COALESCE(m.note, ''), COALESCE(m.tracked, 0),
       COALESCE(m.bind_ssid, ''), COALESCE(m.bind_ap_mac, '')
FROM clients c
LEFT JOIN client_meta m ON m.mac = c.mac
ORDER BY c.online DESC, c.signal DESC, c.mac`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Client, 0, 16)
	for rows.Next() {
		c, err := scanClientRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetClient returns one station, including operator metadata, or ErrNotFound.
func (s *Store) GetClient(ctx context.Context, mac string) (Client, error) {
	const q = `
SELECT c.mac, c.ap_mac, c.ip, c.ssid, c.ifname, c.band, c.signal, c.rx_rate, c.tx_rate,
       CASE WHEN c.last_seen > c.connected_since THEN c.last_seen - c.connected_since ELSE 0 END,
       c.connected_since, c.last_seen, c.online,
       COALESCE(m.alias, ''), COALESCE(m.note, ''), COALESCE(m.tracked, 0),
       COALESCE(m.bind_ssid, ''), COALESCE(m.bind_ap_mac, '')
FROM clients c
LEFT JOIN client_meta m ON m.mac = c.mac
WHERE c.mac = ?`
	c, err := scanClientRow(s.db.QueryRowContext(ctx, q, mac))
	if err == sql.ErrNoRows {
		return Client{}, ErrNotFound
	}
	return c, err
}

// scanClientRow scans the shared client+meta projection. It accepts both
// *sql.Row and *sql.Rows through the small scanner interface.
func scanClientRow(row interface{ Scan(...any) error }) (Client, error) {
	var (
		c       Client
		online  int
		tracked int
	)
	if err := row.Scan(&c.MAC, &c.APMAC, &c.IP, &c.SSID, &c.Ifname, &c.Band,
		&c.Signal, &c.RxRate, &c.TxRate, &c.ConnectedSeconds,
		&c.FirstSeen, &c.LastSeen, &online,
		&c.Alias, &c.Note, &tracked, &c.BindSSID, &c.BindAP); err != nil {
		return Client{}, err
	}
	c.Online = online == 1
	c.Tracked = tracked == 1
	return c, nil
}

// CountClients reports how many stations are currently associated.
func (s *Store) CountClients(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM clients WHERE online = 1`).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}
