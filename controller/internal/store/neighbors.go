package store

import (
	"context"
	"fmt"
	"time"
)

// BSSNeighbor is one BSS of one AP as that AP described itself for 802.11k.
//
// Report is hostapd's opaque encoding; the controller stores and forwards it
// verbatim. Rebuilding it from the template would require re-deriving operating
// classes on every band and width change, and would silently disagree with the
// radio's actual channel.
type BSSNeighbor struct {
	APMAC  string
	Ifname string
	BSSID  string
	SSID   string
	Report string
}

// ReplaceNeighborsForAP applies a full BSS list from one AP.
//
// Same reasoning as the client table: a template edit can remove a BSS, and a
// delta update would leave that BSS advertising itself as a neighbour forever.
func (s *Store) ReplaceNeighborsForAP(ctx context.Context, apMAC string, list []BSSNeighbor) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("neighbors tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM bss_neighbors WHERE ap_mac = ?`, apMAC); err != nil {
		return fmt.Errorf("neighbors clear: %w", err)
	}
	now := time.Now().Unix()
	for _, n := range list {
		if n.Ifname == "" || n.BSSID == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO bss_neighbors (ap_mac, ifname, bssid, ssid, report, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(ap_mac, ifname) DO UPDATE SET
				bssid = excluded.bssid,
				ssid = excluded.ssid,
				report = excluded.report,
				updated_at = excluded.updated_at`,
			apMAC, n.Ifname, n.BSSID, n.SSID, n.Report, now); err != nil {
			return fmt.Errorf("neighbors upsert %s/%s: %w", apMAC, n.Ifname, err)
		}
	}
	return tx.Commit()
}

// AllNeighbors returns every BSS reported by every adopted AP, ordered so
// package rendering is deterministic. A pending device is excluded: it has no
// business appearing in another AP's neighbour reports before it is trusted.
func (s *Store) AllNeighbors(ctx context.Context) ([]BSSNeighbor, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.ap_mac, n.ifname, n.bssid, n.ssid, n.report
		FROM bss_neighbors n
		JOIN devices d ON d.mac = n.ap_mac
		WHERE d.state = ?
		ORDER BY n.ap_mac, n.ifname`, StateAdopted)
	if err != nil {
		return nil, fmt.Errorf("neighbors list: %w", err)
	}
	defer rows.Close()

	var out []BSSNeighbor
	for rows.Next() {
		var n BSSNeighbor
		if err := rows.Scan(&n.APMAC, &n.Ifname, &n.BSSID, &n.SSID, &n.Report); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
