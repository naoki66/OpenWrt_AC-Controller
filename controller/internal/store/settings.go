package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DeviceKeySetting stores a per-device request/encryption credential under a
// normalized MAC. It is never included in the management config export.
func DeviceKeySetting(mac string) string { return "ap_device_key:" + mac }

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
