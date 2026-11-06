package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Device states. pending means discovered but not adopted; an AP in pending
// never receives configuration. adopted means the operator took it over.
const (
	StatePending = "pending"
	StateAdopted = "adopted"
)

type Device struct {
	MAC      string `json:"mac"`
	Model    string `json:"model"`
	Firmware string `json:"firmware"`
	IP       string `json:"ip"`
	Hostname string `json:"hostname"`
	State    string `json:"state"`
	// TemplateID is the assigned wireless template; only set once adopted.
	TemplateID string `json:"template_id"`
	// ConfigRev is the newest revision generated for this device. 0 means no
	// package has ever been produced.
	ConfigRev int `json:"config_rev"`
	// AppliedRev is the newest revision the AP reported as applied.
	AppliedRev int `json:"applied_rev"`
	// SyncState is the AP's own verdict on its last apply: idle/applied/
	// rollback/failed. Empty means the AP has never reported.
	SyncState   string `json:"sync_state"`
	SyncMessage string `json:"sync_message,omitempty"`
	SyncAt      int64  `json:"sync_at"`
	// NetworkParams is the per-unit layer-2 override as JSON (model.DeviceNetwork):
	// bridge ports, per-VLAN ports and management addresses. Empty means the unit
	// takes the template as written. Stored as text rather than columns because it
	// is a whole small document that only the renderer reads.
	NetworkParams string `json:"network_params,omitempty"`
	// FwTarget is the firmware version an operator asked this AP to run. Empty
	// means "leave it alone". FWState mirrors the agent's report
	// (upgrading/applied/failed) and is what stops the controller re-sending an
	// image that already failed.
	FwTarget  string `json:"fw_target,omitempty"`
	FWState   string `json:"fw_state,omitempty"`
	FWMessage string `json:"fw_message,omitempty"`
	FWAt      int64  `json:"fw_at,omitempty"`
	// Steering is the daemon the AP reports as enabled (nrsyncd/usteer/dawn).
	Steering string `json:"steering"`
	// Uptime is seconds since the AP booted, as of its last hello. Zero means the
	// agent did not report it, not "just booted".
	Uptime int64 `json:"uptime"`
	// RebootPeriodDays schedules a periodic reboot: every N days at RebootAt
	// (controller-local "HH:MM"). Zero disables the schedule. RebootNextAt is the
	// next due time and is advanced by the scheduler, not by the AP.
	RebootPeriodDays int    `json:"reboot_period_days"`
	RebootAt         string `json:"reboot_at,omitempty"`
	RebootNextAt     int64  `json:"reboot_next_at,omitempty"`
	// LEDOnAt/LEDOffAt schedule a daily status-LED switch (controller-local
	// "HH:MM"); empty disables that edge. LEDOnNextAt/LEDOffNextAt are the next
	// due times, advanced by the scheduler rather than by the AP.
	LEDOnAt      string `json:"led_on_at,omitempty"`
	LEDOffAt     string `json:"led_off_at,omitempty"`
	LEDOnNextAt  int64  `json:"led_on_next_at,omitempty"`
	LEDOffNextAt int64  `json:"led_off_next_at,omitempty"`
	// GroupID is the AP group this device belongs to. The group carries the
	// template for its members; an adopted device whose group template differs
	// from TemplateID has a pending assignment until it is pushed.
	GroupID   string `json:"group_id,omitempty"`
	LastSeen  int64  `json:"last_seen"`
	CreatedAt int64  `json:"created_at"`

	// Online and LastSeenAge are derived from LastSeen at read time and never
	// stored: "online" is a property of the clock, not of the device row.
	//
	// State alone cannot answer "is this AP under control": State is the
	// adoption relationship and survives a reflash, so a controller that
	// reported only State kept showing a reflashed AP as adopted forever.
	Online      bool  `json:"online"`
	LastSeenAge int64 `json:"last_seen_age_seconds"`
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite allows a single writer; keeping the pool small avoids
	// SQLITE_BUSY storms under polling clients.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS devices (
	mac        TEXT PRIMARY KEY,
	model      TEXT NOT NULL DEFAULT '',
	firmware   TEXT NOT NULL DEFAULT '',
	ip         TEXT NOT NULL DEFAULT '',
	hostname   TEXT NOT NULL DEFAULT '',
	state      TEXT NOT NULL DEFAULT 'pending',
	template_id TEXT NOT NULL DEFAULT '',
	config_rev INTEGER NOT NULL DEFAULT 0,
	applied_rev INTEGER NOT NULL DEFAULT 0,
	sync_state TEXT NOT NULL DEFAULT '',
	sync_message TEXT NOT NULL DEFAULT '',
	sync_at INTEGER NOT NULL DEFAULT 0,
	steering TEXT NOT NULL DEFAULT '',
	network_params TEXT NOT NULL DEFAULT '',
	fw_target  TEXT NOT NULL DEFAULT '',
	fw_state   TEXT NOT NULL DEFAULT '',
	fw_message TEXT NOT NULL DEFAULT '',
	fw_at      INTEGER NOT NULL DEFAULT 0,
	uptime     INTEGER NOT NULL DEFAULT 0,
	reboot_period_days INTEGER NOT NULL DEFAULT 0,
	reboot_at  TEXT NOT NULL DEFAULT '',
	reboot_next_at INTEGER NOT NULL DEFAULT 0,
	led_on_at  TEXT NOT NULL DEFAULT '',
	led_off_at TEXT NOT NULL DEFAULT '',
	led_on_next_at INTEGER NOT NULL DEFAULT 0,
	led_off_next_at INTEGER NOT NULL DEFAULT 0,
	group_id   TEXT NOT NULL DEFAULT '',
	last_seen  INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_devices_last_seen ON devices(last_seen);

CREATE TABLE IF NOT EXISTS events (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	ts      INTEGER NOT NULL,
	kind    TEXT NOT NULL,
	mac     TEXT NOT NULL DEFAULT '',
	message TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts);

-- device_actions is the controller-to-AP command queue. An action stays pending
-- (and is re-sent on every hello) until the AP reports a result or it ages out.
-- Rows are kept after completion so the UI can show what was asked and why.
CREATE TABLE IF NOT EXISTS device_actions (
	id            TEXT PRIMARY KEY,
	mac           TEXT NOT NULL,
	type          TEXT NOT NULL,
	args          TEXT NOT NULL DEFAULT '{}',
	state         TEXT NOT NULL DEFAULT 'pending',
	message       TEXT NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL DEFAULT 0,
	dispatched_at INTEGER NOT NULL DEFAULT 0,
	finished_at   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_device_actions_mac ON device_actions(mac, created_at);

-- Client table is a snapshot: the AP reports the full list each cycle, so rows
-- for stations that left are flagged offline rather than deleted (keeps history).
CREATE TABLE IF NOT EXISTS clients (
	mac        TEXT PRIMARY KEY,
	ap_mac     TEXT NOT NULL DEFAULT '',
	ip         TEXT NOT NULL DEFAULT '',
	ssid       TEXT NOT NULL DEFAULT '',
	ifname     TEXT NOT NULL DEFAULT '',
	band       TEXT NOT NULL DEFAULT '',
	signal     INTEGER NOT NULL DEFAULT 0,
	rx_rate    INTEGER NOT NULL DEFAULT 0,
	tx_rate    INTEGER NOT NULL DEFAULT 0,
	connected_since INTEGER NOT NULL DEFAULT 0,
	first_seen INTEGER NOT NULL DEFAULT 0,
	last_seen  INTEGER NOT NULL DEFAULT 0,
	online     INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_clients_ap ON clients(ap_mac);
CREATE INDEX IF NOT EXISTS idx_clients_last_seen ON clients(last_seen);

-- client_meta is controller-side intent about a station: a human alias/note,
-- the "track this client" flag, and the SSID (optionally one AP) it should
-- prefer. The AP reports stations every cycle and would overwrite such fields,
-- so they live beside the snapshot rather than inside it.
CREATE TABLE IF NOT EXISTS client_meta (
	mac        TEXT PRIMARY KEY,
	alias      TEXT NOT NULL DEFAULT '',
	note       TEXT NOT NULL DEFAULT '',
	tracked    INTEGER NOT NULL DEFAULT 0,
	bind_ssid  TEXT NOT NULL DEFAULT '',
	bind_ap_mac TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL DEFAULT 0
);

-- bss_neighbors holds each AP's self-description for 802.11k. It is keyed by
-- (ap_mac, ifname) because a BSS is identified locally by its netdev; the
-- controller uses it to build every peer list. hostapd's report body is stored
-- verbatim and never reinterpreted.
CREATE TABLE IF NOT EXISTS bss_neighbors (
	ap_mac     TEXT NOT NULL,
	ifname     TEXT NOT NULL,
	bssid      TEXT NOT NULL DEFAULT '',
	ssid       TEXT NOT NULL DEFAULT '',
	report     TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (ap_mac, ifname)
);
CREATE INDEX IF NOT EXISTS idx_bss_neighbors_ssid ON bss_neighbors(ssid);

CREATE TABLE IF NOT EXISTS templates (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL DEFAULT '',
	json       TEXT NOT NULL DEFAULT '{}',
	updated_at INTEGER NOT NULL DEFAULT 0
);

-- AP groups bind a set of devices to one template. A device stores only the
-- group id: membership is a controller-side decision, and the AP itself has no
-- use for it in its configuration.
CREATE TABLE IF NOT EXISTS ap_groups (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL DEFAULT '',
	template_id TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL DEFAULT 0,
	updated_at  INTEGER NOT NULL DEFAULT 0
);

-- Telemetry (P4). client_samples is polled at AC_SAMPLE_INTERVAL and holds one
-- row per station per sample; radio_samples holds one row per radio per sample.
-- Both are pruned by age, never by count, and neither is referenced by anything
-- else — the live views (clients table, device row) stay authoritative.
CREATE TABLE IF NOT EXISTS client_samples (
	ts      INTEGER NOT NULL,
	ap_mac  TEXT NOT NULL,
	mac     TEXT NOT NULL,
	ssid    TEXT NOT NULL DEFAULT '',
	band    TEXT NOT NULL DEFAULT '',
	ifname  TEXT NOT NULL DEFAULT '',
	signal  INTEGER NOT NULL DEFAULT 0,
	rx_rate INTEGER NOT NULL DEFAULT 0,
	tx_rate INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_client_samples_mac_ts ON client_samples(mac, ts);
CREATE INDEX IF NOT EXISTS idx_client_samples_ap_ts ON client_samples(ap_mac, ts);
CREATE INDEX IF NOT EXISTS idx_client_samples_ts ON client_samples(ts);

CREATE TABLE IF NOT EXISTS radio_samples (
	ts          INTEGER NOT NULL,
	ap_mac      TEXT NOT NULL,
	radio       TEXT NOT NULL,
	phy         TEXT NOT NULL DEFAULT '',
	band        TEXT NOT NULL DEFAULT '',
	up          INTEGER NOT NULL DEFAULT 0,
	channel     INTEGER NOT NULL DEFAULT 0,
	freq_mhz    INTEGER NOT NULL DEFAULT 0,
	htmode      TEXT NOT NULL DEFAULT '',
	width_mhz   INTEGER NOT NULL DEFAULT 0,
	txpower     INTEGER NOT NULL DEFAULT 0,
	country     TEXT NOT NULL DEFAULT '',
	utilization INTEGER NOT NULL DEFAULT -1,
	clients     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_radio_samples_ap_radio_ts ON radio_samples(ap_mac, radio, ts);
CREATE INDEX IF NOT EXISTS idx_radio_samples_ts ON radio_samples(ts);

-- radio_state is the "now" view behind LatestRadios: one row per (AP, radio),
-- replaced wholesale by the hello handler whenever the agent reports radios.
-- radio_samples stays the append-only history for charts; this table is what
-- makes a renamed or removed radio disappear immediately instead of lingering
-- as a ghost until retention pruning happens to erase its last sample.
CREATE TABLE IF NOT EXISTS radio_state (
	ap_mac      TEXT NOT NULL,
	radio       TEXT NOT NULL,
	band        TEXT NOT NULL DEFAULT '',
	up          INTEGER NOT NULL DEFAULT 0,
	channel     INTEGER NOT NULL DEFAULT 0,
	freq_mhz    INTEGER NOT NULL DEFAULT 0,
	htmode      TEXT NOT NULL DEFAULT '',
	width_mhz   INTEGER NOT NULL DEFAULT 0,
	txpower     INTEGER NOT NULL DEFAULT 0,
	country     TEXT NOT NULL DEFAULT '',
	utilization INTEGER NOT NULL DEFAULT -1,
	clients     INTEGER NOT NULL DEFAULT 0,
	updated_at  INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (ap_mac, radio)
);

-- Packages are kept whole so the exact bytes handed to an AP stay auditable.
CREATE TABLE IF NOT EXISTS packages (
	mac        TEXT NOT NULL,
	rev        INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	sha256     TEXT NOT NULL,
	size       INTEGER NOT NULL,
	blob       BLOB NOT NULL,
	PRIMARY KEY (mac, rev)
);

-- Firmware images are keyed by (model, version) rather than by digest alone:
-- an operator names a build once and can then say "put 20261101 on this AP".
-- The blob is kept whole because sysupgrade must receive exactly the bytes that
-- were uploaded, not something re-encoded along the way.
CREATE TABLE IF NOT EXISTS firmwares (
	model       TEXT NOT NULL,
	version     TEXT NOT NULL,
	filename    TEXT NOT NULL DEFAULT '',
	size        INTEGER NOT NULL,
	sha256      TEXT NOT NULL,
	uploaded_at INTEGER NOT NULL,
	blob        BLOB NOT NULL,
	PRIMARY KEY (model, version)
);

-- Runtime-tunable settings set through the management API. Deliberately a
-- key/value blob: the knobs that live here (currently the AP shared secret)
-- are few and policy-shaped, not schema-shaped.
CREATE TABLE IF NOT EXISTS settings (
	key        TEXT PRIMARY KEY,
	value      TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	// Added after the first deployments, so an existing database has to be
	// widened. SQLite has no "add column if not exists"; the duplicate-column
	// error is the only benign one here and is ignored.
	if _, err := s.db.ExecContext(ctx,
		`ALTER TABLE devices ADD COLUMN network_params TEXT NOT NULL DEFAULT ''`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate devices.network_params: %w", err)
		}
	}

	// Databases created before P2 lack these columns; SQLite has no
	// "ADD COLUMN IF NOT EXISTS", so check first.
	for _, col := range []struct{ name, ddl string }{
		{"template_id", `ALTER TABLE devices ADD COLUMN template_id TEXT NOT NULL DEFAULT ''`},
		{"config_rev", `ALTER TABLE devices ADD COLUMN config_rev INTEGER NOT NULL DEFAULT 0`},
		{"applied_rev", `ALTER TABLE devices ADD COLUMN applied_rev INTEGER NOT NULL DEFAULT 0`},
		{"sync_state", `ALTER TABLE devices ADD COLUMN sync_state TEXT NOT NULL DEFAULT ''`},
		{"sync_message", `ALTER TABLE devices ADD COLUMN sync_message TEXT NOT NULL DEFAULT ''`},
		{"sync_at", `ALTER TABLE devices ADD COLUMN sync_at INTEGER NOT NULL DEFAULT 0`},
		{"steering", `ALTER TABLE devices ADD COLUMN steering TEXT NOT NULL DEFAULT ''`},
		{"fw_target", `ALTER TABLE devices ADD COLUMN fw_target TEXT NOT NULL DEFAULT ''`},
		{"fw_state", `ALTER TABLE devices ADD COLUMN fw_state TEXT NOT NULL DEFAULT ''`},
		{"fw_message", `ALTER TABLE devices ADD COLUMN fw_message TEXT NOT NULL DEFAULT ''`},
		{"fw_at", `ALTER TABLE devices ADD COLUMN fw_at INTEGER NOT NULL DEFAULT 0`},
		{"uptime", `ALTER TABLE devices ADD COLUMN uptime INTEGER NOT NULL DEFAULT 0`},
		{"reboot_period_days", `ALTER TABLE devices ADD COLUMN reboot_period_days INTEGER NOT NULL DEFAULT 0`},
		{"reboot_at", `ALTER TABLE devices ADD COLUMN reboot_at TEXT NOT NULL DEFAULT ''`},
		{"reboot_next_at", `ALTER TABLE devices ADD COLUMN reboot_next_at INTEGER NOT NULL DEFAULT 0`},
		{"led_on_at", `ALTER TABLE devices ADD COLUMN led_on_at TEXT NOT NULL DEFAULT ''`},
		{"led_off_at", `ALTER TABLE devices ADD COLUMN led_off_at TEXT NOT NULL DEFAULT ''`},
		{"led_on_next_at", `ALTER TABLE devices ADD COLUMN led_on_next_at INTEGER NOT NULL DEFAULT 0`},
		{"led_off_next_at", `ALTER TABLE devices ADD COLUMN led_off_next_at INTEGER NOT NULL DEFAULT 0`},
		{"group_id", `ALTER TABLE devices ADD COLUMN group_id TEXT NOT NULL DEFAULT ''`},
	} {
		if err := s.addColumnIfMissing(ctx, "devices", col.name, col.ddl); err != nil {
			return err
		}
	}

	// radio_samples gained its regulatory column after the first telemetry
	// deployments; samples written before it exist keep the default ''.
	if err := s.addColumnIfMissing(ctx, "radio_samples", "country",
		`ALTER TABLE radio_samples ADD COLUMN country TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}

	// Databases created before radio_state existed get one shot at carrying
	// their last-known radios over, so an AP that is offline during the upgrade
	// keeps its (dimmed) rows on the radio page. The settings marker is what
	// makes this run once: an empty radio_state is a legitimate steady state
	// (every AP reporting zero radios) and must not trigger a re-backfill that
	// resurrects names convergence already deleted.
	var backfilled int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM settings WHERE key = 'radio_state_backfilled'`).Scan(&backfilled); err != nil {
		return fmt.Errorf("check radio_state backfill: %w", err)
	}
	if backfilled == 0 {
		if _, err := s.db.ExecContext(ctx, `
INSERT INTO radio_state (ap_mac, radio, band, up, channel, freq_mhz, htmode, width_mhz, txpower, country, utilization, clients, updated_at)
SELECT ap_mac, radio, band, up, channel, freq_mhz, htmode, width_mhz, txpower, country, utilization, clients, ts
FROM (
	SELECT ts, ap_mac, radio, band, up, channel, freq_mhz, htmode, width_mhz, txpower, country, utilization, clients,
	       ROW_NUMBER() OVER (PARTITION BY ap_mac, radio ORDER BY ts DESC, rowid DESC) AS rn
	FROM radio_samples
)
WHERE rn = 1`); err != nil {
			return fmt.Errorf("backfill radio_state: %w", err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at) VALUES ('radio_state_backfilled', '1', strftime('%s','now'))`); err != nil {
			return fmt.Errorf("mark radio_state backfilled: %w", err)
		}
	}
	return nil
}

func (s *Store) addColumnIfMissing(ctx context.Context, table, column, ddl string) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return fmt.Errorf("inspect %s: %w", table, err)
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect %s: %w", table, err)
	}
	rows.Close()

	if _, err := s.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

// UpsertDiscovered records an AP sighting without changing its adoption state.
//
// An empty model/firmware means "this hello did not say", not "this AP lost its
// identity": blanking the model would silently stop firmware instructions, since
// the repository is keyed by it.
func (s *Store) UpsertDiscovered(ctx context.Context, d Device) error {
	now := time.Now().Unix()
	const q = `
INSERT INTO devices (mac, model, firmware, ip, hostname, state, last_seen, created_at)
VALUES (?, ?, ?, ?, ?, 'pending', ?, ?)
ON CONFLICT(mac) DO UPDATE SET
	model     = CASE WHEN excluded.model <> '' THEN excluded.model ELSE devices.model END,
	firmware  = CASE WHEN excluded.firmware <> '' THEN excluded.firmware ELSE devices.firmware END,
	ip        = excluded.ip,
	hostname  = excluded.hostname,
	last_seen = excluded.last_seen`
	_, err := s.db.ExecContext(ctx, q, d.MAC, d.Model, d.Firmware, d.IP, d.Hostname, now, now)
	if err != nil {
		return fmt.Errorf("upsert device %s: %w", d.MAC, err)
	}
	return nil
}

// deviceColumns keeps SELECT lists and Scan order in one place.
const deviceColumns = `mac, model, firmware, ip, hostname, state, template_id, config_rev, applied_rev, sync_state, sync_message, sync_at, steering, last_seen, created_at, network_params, fw_target, fw_state, fw_message, fw_at, uptime, reboot_period_days, reboot_at, reboot_next_at, led_on_at, led_off_at, led_on_next_at, led_off_next_at, group_id`

func scanDevice(row interface{ Scan(...any) error }) (Device, error) {
	var d Device
	err := row.Scan(&d.MAC, &d.Model, &d.Firmware, &d.IP, &d.Hostname,
		&d.State, &d.TemplateID, &d.ConfigRev, &d.AppliedRev,
		&d.SyncState, &d.SyncMessage, &d.SyncAt, &d.Steering, &d.LastSeen, &d.CreatedAt,
		&d.NetworkParams, &d.FwTarget, &d.FWState, &d.FWMessage, &d.FWAt, &d.Uptime,
		&d.RebootPeriodDays, &d.RebootAt, &d.RebootNextAt,
		&d.LEDOnAt, &d.LEDOffAt, &d.LEDOnNextAt, &d.LEDOffNextAt, &d.GroupID)
	return d, err
}

func (s *Store) ListDevices(ctx context.Context) ([]Device, error) {
	q := `SELECT ` + deviceColumns + ` FROM devices ORDER BY last_seen DESC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Device, 0, 8)
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ErrNotFound is returned when a device or template does not exist.
var ErrNotFound = errors.New("not found")

// GetDevice returns one device or ErrNotFound.
func (s *Store) GetDevice(ctx context.Context, mac string) (Device, error) {
	q := `SELECT ` + deviceColumns + ` FROM devices WHERE mac = ?`
	d, err := scanDevice(s.db.QueryRowContext(ctx, q, mac))
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	return d, err
}

// MarkAdopted assigns a template and records the revision just generated.
func (s *Store) MarkAdopted(ctx context.Context, mac, templateID string, rev int) error {
	const q = `UPDATE devices SET state = ?, template_id = ?, config_rev = ? WHERE mac = ?`
	res, err := s.db.ExecContext(ctx, q, StateAdopted, templateID, rev, mac)
	if err != nil {
		return fmt.Errorf("adopt %s: %w", mac, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Release returns a device to pending. The revision counter is deliberately
// kept: re-adopting continues from the last revision instead of restarting at 1,
// so an AP never sees two different packages sharing the same rev.
func (s *Store) Release(ctx context.Context, mac string) error {
	const q = `UPDATE devices SET state = ?, template_id = '' WHERE mac = ?`
	res, err := s.db.ExecContext(ctx, q, StatePending, mac)
	if err != nil {
		return fmt.Errorf("release %s: %w", mac, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetConfigRev records the newest generated revision without changing adoption.
func (s *Store) SetConfigRev(ctx context.Context, mac string, rev int) error {
	const q = `UPDATE devices SET config_rev = ? WHERE mac = ?`
	res, err := s.db.ExecContext(ctx, q, rev, mac)
	if err != nil {
		return fmt.Errorf("set config rev %s: %w", mac, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordHello folds one heartbeat into the device row.
//
// applied_rev and steering are refreshed on every hello: a steady-state AP
// reports no sync state, so keying those fields off SyncState would leave the
// controller permanently showing rev 0 and steering "-" for a healthy device.
// applied_rev is stored as reported, not MAX()ed: an AP that rolls back to a
// lower revision after a controller restore must be shown on that revision,
// otherwise its sync card claims a version it no longer runs. The sync fields
// only move when the AP actually reports an outcome.
func (s *Store) RecordHello(ctx context.Context, mac string, appliedRev int, uptime int64, steering, syncState, syncMessage string) error {
	const q = `
UPDATE devices SET
	applied_rev  = ?,
	uptime       = CASE WHEN ? > 0 THEN ? ELSE uptime END,
	steering     = CASE WHEN ? <> '' THEN ? ELSE steering END,
	sync_state   = CASE WHEN ? <> '' THEN ? ELSE sync_state END,
	sync_message = CASE WHEN ? <> '' THEN ? ELSE sync_message END,
	sync_at      = CASE WHEN ? <> '' THEN strftime('%s','now') ELSE sync_at END
WHERE mac = ?`
	reported := syncState
	res, err := s.db.ExecContext(ctx, q,
		appliedRev,
		uptime, uptime,
		steering, steering,
		reported, syncState,
		reported, syncMessage,
		reported,
		mac)
	if err != nil {
		return fmt.Errorf("record hello %s: %w", mac, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountDevices reports how many APs have ever been seen.
func (s *Store) CountDevices(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices`).Scan(&n)
	return n, err
}

// LogEvent appends to the audit trail.
func (s *Store) LogEvent(ctx context.Context, kind, mac, message string) {
	_, _ = s.db.ExecContext(ctx,
		`INSERT INTO events (ts, kind, mac, message) VALUES (?, ?, ?, ?)`,
		time.Now().Unix(), kind, mac, message)
}

// LoadOrCreateID returns the stable controller identity, generating one on
// first boot. APs pin this value to avoid being adopted by a stray controller.
func LoadOrCreateID(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return string(b), nil
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := hex.EncodeToString(buf)
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(path, []byte(id), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

// SetDeviceNetwork stores the per-unit layer-2 override. An empty string clears
// it, which returns the device to "template as written".
func (s *Store) SetDeviceNetwork(ctx context.Context, mac, params string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET network_params = ? WHERE mac = ?`, params, mac)
	if err != nil {
		return fmt.Errorf("set device network %s: %w", mac, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
