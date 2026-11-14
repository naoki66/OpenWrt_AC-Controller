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

// Settings are the runtime-tunable values an operator sets through the
// management API (currently the AP shared secret). They survive restarts and
// take precedence over the environment, so a secret changed in the UI does not
// silently revert on the next container recreate.

// Setting keys.
const (
	// SettingSharedSecret overrides AC_SHARED_SECRET when non-empty. An AP
	// registering with the old secret is rejected the moment this is written,
	// which is exactly the point of rotating it.
	SettingSharedSecret = "shared_secret"
	// SettingManagementKey is generated once per deployment and distributed
	// to approved APs. It is not the operator-configurable legacy secret.
	SettingManagementKey = "management_key"
	// SettingAdminUser is the login name of the management account. Seeded with
	// "admin" on first start; the system page can rename it.
	SettingAdminUser = "admin_user"
	// SettingAdminPass is the salted PBKDF2 hash of the management password.
	// The plain text is never stored; a first start seeds the default hash.
	SettingAdminPass = "admin_password"
	// SettingNTPServers is the controller-managed site time-source list, stored
	// as a JSON array. An absent row means "do not manage AP NTP".
	SettingNTPServers = "ntp_servers"
)

// EnsureManagementKey creates the deployment key on first start and reuses it
// when the database survives a restart or container replacement.
func (s *Store) EnsureManagementKey(ctx context.Context) error {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at)
VALUES (?, ?, ?) ON CONFLICT(key) DO NOTHING`,
		SettingManagementKey, hex.EncodeToString(key[:]), time.Now().Unix())
	return err
}

// GetSetting returns one setting or ErrNotFound.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	const q = `SELECT value FROM settings WHERE key = ?`
	var value string
	err := s.db.QueryRowContext(ctx, q, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get setting %s: %w", key, err)
	}
	return value, nil
}

// SetSetting writes one setting, replacing any previous value.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	const q = `
INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`
	if _, err := s.db.ExecContext(ctx, q, key, value, time.Now().Unix()); err != nil {
		return fmt.Errorf("set setting %s: %w", key, err)
	}
	return nil
}

// DeleteSetting removes one setting; deleting a missing key is not an error so
// "restore the environment default" is idempotent.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
	if err != nil {
		return fmt.Errorf("delete setting %s: %w", key, err)
	}
	return nil
}
