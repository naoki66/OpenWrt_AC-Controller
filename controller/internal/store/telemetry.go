package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Time-series telemetry (P4).
//
// Two tables with deliberately different shapes, because the two quantities
// behave differently:
//
//   - client_samples is a regular poll: every associated station, every sample
//     interval. Signal and rate are continuous, so the value of a bucket is its
//     average.
//   - radio_samples is one row per radio per interval, but the interesting
//     columns are not averageable. A channel is a *state*: averaging 36 and 149
//     produces 92.5, which is not a channel. So the query keeps the last value
//     in each bucket and the UI draws a step chart.
//
// Both are pruned by age, not by count. An AP with 200 clients at a 60s
// interval produces ~7k rows an hour; retention is what keeps that bounded.

// ClientSample is one station observation at one instant.
type ClientSample struct {
	TS     int64
	APMAC  string
	MAC    string
	SSID   string
	Band   string
	Ifname string
	Signal int
	RxRate int
	TxRate int
}

// RadioSample is one radio's operating point at one instant.
type RadioSample struct {
	TS          int64
	APMAC       string
	Radio       string
	Phy         string
	Band        string
	Up          bool
	Channel     int
	FreqMHz     int
	HTMode      string
	WidthMHz    int
	TxPower     int
	Country     string
	Utilization int
	Clients     int
}

// ClientPoint is one bucket of one client's series.
type ClientPoint struct {
	TS     int64 `json:"ts"`
	Signal int   `json:"signal_dbm"`
	RxRate int   `json:"rx_rate_mbps"`
	TxRate int   `json:"tx_rate_mbps"`
}

// RadioPoint is one bucket of one radio's series.
type RadioPoint struct {
	TS          int64  `json:"ts"`
	APMAC       string `json:"ap_mac"`
	Radio       string `json:"radio"`
	Band        string `json:"band"`
	Up          bool   `json:"up"`
	Channel     int    `json:"channel"`
	FreqMHz     int    `json:"freq_mhz"`
	HTMode      string `json:"htmode,omitempty"`
	WidthMHz    int    `json:"width_mhz"`
	TxPower     int    `json:"txpower"`
	Country     string `json:"country,omitempty"`
	Utilization int    `json:"utilization"`
	Clients     int    `json:"clients"`
}

// ClientCountPoint is one bucket of "how many stations on this AP".
type ClientCountPoint struct {
	TS      int64  `json:"ts"`
	APMAC   string `json:"ap_mac"`
	Clients int    `json:"clients"`
}

// RecordSamples appends one telemetry batch. It is called on a throttled
// cadence, not on every hello, so a full test can drive it directly with any
// timestamp it likes.
func (s *Store) RecordSamples(ctx context.Context, clients []ClientSample, radios []RadioSample) error {
	if len(clients) == 0 && len(radios) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("samples tx: %w", err)
	}
	defer tx.Rollback()

	const clientInsert = `INSERT INTO client_samples
		(ts, ap_mac, mac, ssid, band, ifname, signal, rx_rate, tx_rate)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	for _, c := range clients {
		if _, err := tx.ExecContext(ctx, clientInsert,
			c.TS, c.APMAC, c.MAC, c.SSID, c.Band, c.Ifname,
			c.Signal, c.RxRate, c.TxRate); err != nil {
			return fmt.Errorf("insert client sample: %w", err)
		}
	}

	const radioInsert = `INSERT INTO radio_samples
		(ts, ap_mac, radio, phy, band, up, channel, freq_mhz, htmode, width_mhz, txpower, country, utilization, clients)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	for _, r := range radios {
		up := 0
		if r.Up {
			up = 1
		}
		if _, err := tx.ExecContext(ctx, radioInsert,
			r.TS, r.APMAC, r.Radio, r.Phy, r.Band, up,
			r.Channel, r.FreqMHz, r.HTMode, r.WidthMHz, r.TxPower, r.Country, r.Utilization, r.Clients,
		); err != nil {
			return fmt.Errorf("insert radio sample: %w", err)
		}
	}
	return tx.Commit()
}

// ClientSeries returns one client's signal and rates, averaged into buckets.
//
// Averaging is right here: both quantities are continuous and the alternative
// (picking one sample per bucket) would make a chart jump around on noise.
func (s *Store) ClientSeries(ctx context.Context, mac string, from, to int64, bucket int) ([]ClientPoint, error) {
	if bucket <= 0 {
		bucket = 60
	}
	const q = `
SELECT (ts / ?) * ? AS bucket,
       CAST(ROUND(AVG(signal)) AS INTEGER),
       CAST(ROUND(AVG(rx_rate)) AS INTEGER),
       CAST(ROUND(AVG(tx_rate)) AS INTEGER)
FROM client_samples
WHERE mac = ? AND ts >= ? AND ts <= ?
GROUP BY bucket
ORDER BY bucket`
	rows, err := s.db.QueryContext(ctx, q, bucket, bucket, mac, from, to)
	if err != nil {
		return nil, fmt.Errorf("client series: %w", err)
	}
	defer rows.Close()

	out := make([]ClientPoint, 0, 64)
	for rows.Next() {
		var p ClientPoint
		if err := rows.Scan(&p.TS, &p.Signal, &p.RxRate, &p.TxRate); err != nil {
			return nil, fmt.Errorf("client series scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RadioSeries returns one radio's history (or every radio's, when apMAC is
// empty), reduced to one point per bucket.
//
// The reduction is done here rather than in SQL on purpose: a channel is a
// state, not a measurement, so a bucket must carry the last value observed in
// it. AVG(channel) would invent channels that do not exist. Utilisation and
// client count are averaged, because those really are measurements.
// bucketAcc accumulates the rows that fall into one bucket of a radio series.
type bucketAcc struct {
	point RadioPoint
	n     int
	util  int
	clnt  int
}

func (s *Store) RadioSeries(ctx context.Context, apMAC string, from, to int64, bucket int) ([]RadioPoint, error) {
	if bucket <= 0 {
		bucket = 60
	}
	q := `SELECT ts, ap_mac, radio, band, up, channel, freq_mhz, htmode, width_mhz, txpower, country, utilization, clients
FROM radio_samples WHERE ts >= ? AND ts <= ?`
	args := []any{from, to}
	if apMAC != "" {
		q += ` AND ap_mac = ?`
		args = append(args, apMAC)
	}
	q += ` ORDER BY ap_mac, radio, ts`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("radio series: %w", err)
	}
	defer rows.Close()

	// Rows arrive grouped by series and ordered by time, so a single accumulator
	// is enough: a changed key means the previous bucket is finished.
	var (
		cur    *bucketAcc
		curKey string
	)
	out := make([]RadioPoint, 0, 128)
	flush := func() {
		if cur == nil || cur.n == 0 {
			return
		}
		cur.point.Utilization = cur.util / cur.n
		cur.point.Clients = cur.clnt / cur.n
		out = append(out, cur.point)
	}

	for rows.Next() {
		var (
			p  RadioPoint
			up int
			ts int64
		)
		if err := rows.Scan(&ts, &p.APMAC, &p.Radio, &p.Band, &up,
			&p.Channel, &p.FreqMHz, &p.HTMode, &p.WidthMHz, &p.TxPower, &p.Country, &p.Utilization, &p.Clients); err != nil {
			return nil, fmt.Errorf("radio series scan: %w", err)
		}
		p.Up = up == 1
		p.TS = (ts / int64(bucket)) * int64(bucket)

		if key := p.APMAC + "|" + p.Radio + "|" + fmt.Sprint(p.TS); key != curKey {
			flush()
			cur = &bucketAcc{}
			curKey = key
		}
		// Later rows overwrite the discrete fields: last value wins.
		cur.point = p
		cur.n++
		cur.util += p.Utilization
		cur.clnt += p.Clients
	}
	flush()
	return out, rows.Err()
}

// Keep a coarse freshness timestamp without rewriting identical samples on
// every heartbeat. Changed state is always persisted immediately.
const stateRefreshSeconds = 300

// ReplaceRadiosForAP converges the "now" view onto the set the AP just
// reported: names absent from the list are deleted, so a renamed or removed
// radio vanishes on the next hello instead of haunting the radio page until
// retention pruning erases its last sample. Callers uphold the nil/empty
// contract — a nil list means "agent does not report radios" and never
// reaches here; an empty list is authoritative and clears the AP's rows.
func (s *Store) ReplaceRadiosForAP(ctx context.Context, apMAC string, radios []RadioSample) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("radio state tx: %w", err)
	}
	defer tx.Rollback()

	const insert = `INSERT INTO radio_state
		(ap_mac, radio, band, up, channel, freq_mhz, htmode, width_mhz, txpower, country, utilization, clients, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(ap_mac, radio) DO UPDATE SET
			band=excluded.band, up=excluded.up, channel=excluded.channel, freq_mhz=excluded.freq_mhz,
			htmode=excluded.htmode, width_mhz=excluded.width_mhz, txpower=excluded.txpower,
			country=excluded.country, utilization=excluded.utilization, clients=excluded.clients,
			updated_at=excluded.updated_at
		WHERE (radio_state.band, radio_state.up, radio_state.channel, radio_state.freq_mhz, radio_state.htmode,
			radio_state.width_mhz, radio_state.txpower, radio_state.country, radio_state.utilization, radio_state.clients)
		   IS NOT (excluded.band, excluded.up, excluded.channel, excluded.freq_mhz, excluded.htmode,
			excluded.width_mhz, excluded.txpower, excluded.country, excluded.utilization, excluded.clients)
		   OR radio_state.updated_at <= excluded.updated_at - ?`
	now := time.Now().Unix()
	names := make([]string, 0, len(radios))
	for _, r := range radios {
		if r.Radio == "" {
			continue
		}
		names = append(names, r.Radio)
		up := 0
		if r.Up {
			up = 1
		}
		if _, err := tx.ExecContext(ctx, insert,
			apMAC, r.Radio, r.Band, up, r.Channel, r.FreqMHz, r.HTMode,
			r.WidthMHz, r.TxPower, r.Country, r.Utilization, r.Clients, now, stateRefreshSeconds); err != nil {
			return fmt.Errorf("radio state insert: %w", err)
		}
	}
	encoded, err := json.Marshal(names)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM radio_state
		WHERE ap_mac = ? AND radio NOT IN (SELECT value FROM json_each(?))`, apMAC, string(encoded)); err != nil {
		return fmt.Errorf("remove stale radios: %w", err)
	}
	return tx.Commit()
}

// LatestRadios returns the last reported state of every (AP, radio) pair.
// This is what the radio page shows as "now"; it reads radio_state rather
// than the samples history, so retired radio names disappear on the hello
// that stops reporting them while an offline AP keeps its last known rows.
func (s *Store) LatestRadios(ctx context.Context) ([]RadioPoint, error) {
	const q = `
SELECT updated_at, ap_mac, radio, band, up, channel, freq_mhz, htmode, width_mhz, txpower, country, utilization, clients
FROM radio_state
ORDER BY ap_mac, radio`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("latest radios: %w", err)
	}
	defer rows.Close()

	out := make([]RadioPoint, 0, 8)
	for rows.Next() {
		var (
			p  RadioPoint
			up int
		)
		if err := rows.Scan(&p.TS, &p.APMAC, &p.Radio, &p.Band, &up,
			&p.Channel, &p.FreqMHz, &p.HTMode, &p.WidthMHz, &p.TxPower, &p.Country, &p.Utilization, &p.Clients); err != nil {
			return nil, fmt.Errorf("latest radios scan: %w", err)
		}
		p.Up = up == 1
		out = append(out, p)
	}
	return out, rows.Err()
}

// ClientCounts returns how many distinct stations each AP had per bucket. It is
// derived from client_samples so there is exactly one source of truth for "how
// many clients are on this AP".
func (s *Store) ClientCounts(ctx context.Context, from, to int64, bucket int) ([]ClientCountPoint, error) {
	if bucket <= 0 {
		bucket = 60
	}
	const q = `
SELECT (ts / ?) * ? AS bucket, ap_mac, COUNT(DISTINCT mac)
FROM client_samples
WHERE ts >= ? AND ts <= ?
GROUP BY bucket, ap_mac
ORDER BY bucket, ap_mac`
	rows, err := s.db.QueryContext(ctx, q, bucket, bucket, from, to)
	if err != nil {
		return nil, fmt.Errorf("client counts: %w", err)
	}
	defer rows.Close()

	out := make([]ClientCountPoint, 0, 64)
	for rows.Next() {
		var p ClientCountPoint
		if err := rows.Scan(&p.TS, &p.APMAC, &p.Clients); err != nil {
			return nil, fmt.Errorf("client counts scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PruneSamples deletes telemetry older than ts and returns how many rows went.
// Without this the tables grow without bound: two APs with 30 clients add
// roughly a quarter of a million rows a day.
func (s *Store) PruneSamples(ctx context.Context, before int64) (int64, error) {
	var total int64
	for _, table := range []string{"client_samples", "radio_samples"} {
		res, err := s.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE ts < ?`, before)
		if err != nil {
			return total, fmt.Errorf("prune %s: %w", table, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("prune %s rows: %w", table, err)
		}
		total += n
	}
	return total, nil
}

// CountSamples reports the current size of both telemetry tables. Surfaced in
// health so a growing database is visible before it becomes a problem.
func (s *Store) CountSamples(ctx context.Context) (clients, radios int64, err error) {
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM client_samples`).Scan(&clients); err != nil {
		return 0, 0, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM radio_samples`).Scan(&radios); err != nil {
		return 0, 0, err
	}
	return clients, radios, nil
}

// OldestSample returns the timestamp of the earliest retained sample, or 0 when
// there is none. The UI uses it to say how much history actually exists.
func (s *Store) OldestSample(ctx context.Context) (int64, error) {
	var ts int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MIN(ts), 0) FROM client_samples`).Scan(&ts)
	return ts, err
}

// SampleAge is a convenience for retention math.
func SampleAge(d time.Duration) int64 {
	return time.Now().Add(-d).Unix()
}
