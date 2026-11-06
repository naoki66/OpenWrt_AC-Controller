package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// minSecretBytes is the shortest credential the startup check accepts without
// complaining. A 4-character token on an internet-reachable management port is
// an invitation; 16 bytes is the floor, not a recommendation.
const minSecretBytes = 16

// Config holds runtime settings. Every field can be overridden by an
// environment variable so the same binary runs unmodified in Docker and
// from a shell.
type Config struct {
	// DataDir stores the SQLite database and the generated AC identity.
	DataDir string
	// HTTPAddr is the listen address of the web UI and REST API.
	HTTPAddr string
	// DiscoverAddr is the UDP listen address used for AP discovery.
	DiscoverAddr string
	// SharedSecret is required from APs during registration. Empty disables
	// the check, which is only acceptable on a trusted management segment.
	SharedSecret string
	// AdminToken guards the management REST API and everything it exposes —
	// templates carry PSKs in clear text. Empty keeps the old open behaviour
	// and the server logs a warning at startup.
	AdminToken string
	// AdvertiseIP forces the address handed to APs in AC_OFFER. When empty
	// the controller derives it per request from the routing table.
	AdvertiseIP string
	// SampleInterval is the minimum gap between two telemetry samples from one
	// AP. Heartbeats arrive every 20s; storing every one of them would add
	// ~130k rows a day for 30 clients without making a chart any clearer.
	SampleInterval time.Duration
	// SampleRetention is how long telemetry rows are kept before the hourly
	// prune removes them.
	SampleRetention time.Duration
	// OfflineAfter is how long an AP may go without a heartbeat before the UI
	// reports it offline. Zero means the api package default (90s), which is
	// three agent heartbeats plus margin.
	OfflineAfter time.Duration
	// NTPServers is the site time-source list handed to every AP. A database
	// setting set through the UI takes precedence; this is only the default
	// seeded from AC_NTP_SERVERS on first start.
	NTPServers []string
	// HTTPPort is advertised to APs so they know where to connect back.
	HTTPPort int
}

func Load() Config {
	c := Config{
		DataDir:         env("AC_DATA_DIR", "./data"),
		HTTPAddr:        env("AC_HTTP_ADDR", ":8080"),
		DiscoverAddr:    env("AC_DISCOVER_ADDR", ":29820"),
		SharedSecret:    env("AC_SHARED_SECRET", ""),
		AdminToken:      env("AC_ADMIN_TOKEN", ""),
		AdvertiseIP:     env("AC_ADVERTISE_IP", ""),
		SampleInterval:  dur("AC_SAMPLE_INTERVAL", 60*time.Second),
		SampleRetention: dur("AC_SAMPLE_RETENTION", 7*24*time.Hour),
		OfflineAfter:    dur("AC_OFFLINE_AFTER", 90*time.Second),
		// Portable default: public sources only. The gateway address is site
		// specific, so an operator who has an internal NTP server puts it first
		// from the page or AC_NTP_SERVERS. The controller is deliberately not in
		// the list; an AC outage must not remove time.
		NTPServers: listEnv("AC_NTP_SERVERS", []string{
			"ntp.ntsc.ac.cn", "ntp1.aliyun.com", "ntp.tencent.com", "cn.pool.ntp.org",
		}),
	}
	c.HTTPPort = portFromAddr(c.HTTPAddr)
	if v := env("AC_HTTP_PORT", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.HTTPPort = n
		}
	}
	return c
}

func (c Config) DBPath() string {
	return filepath.Join(c.DataDir, "ac.db")
}

func (c Config) IDFile() string {
	return filepath.Join(c.DataDir, "ac.id")
}

// SecretWarnings reports credentials that weaken the management/AP boundary.
//
// It returns warnings rather than errors on purpose: an unauthenticated
// controller is still the right thing to boot on an isolated management VLAN,
// and refusing to start would be worse than saying so loudly. The one case that
// is actively dangerous — reusing the shared secret as the admin token — is
// still a warning, because the deployment in question may be locked down.
func (c Config) SecretWarnings() []string {
	var out []string
	if c.SharedSecret == "" {
		out = append(out, "AC_SHARED_SECRET is empty: AP registration will not be authenticated")
	}
	if c.AdminToken == "" {
		out = append(out, "AC_ADMIN_TOKEN is empty: automation has no Bearer token "+
			"(the Web UI and management API still require a login account)")
	}
	if c.SharedSecret != "" && c.AdminToken != "" && c.SharedSecret == c.AdminToken {
		// Both credentials being equal defeats the split: every AP, and everyone
		// who extracted the secret from one AP's /etc/config, can read PSKs and
		// rewrite templates.
		out = append(out, "AC_ADMIN_TOKEN equals AC_SHARED_SECRET: APs can read the management API; "+
			"use two different values")
	}
	for name, value := range map[string]string{"AC_ADMIN_TOKEN": c.AdminToken, "AC_SHARED_SECRET": c.SharedSecret} {
		if value != "" && len(value) < minSecretBytes {
			out = append(out, fmt.Sprintf("%s is shorter than %d bytes: generate one with `openssl rand -base64 32`",
				name, minSecretBytes))
		}
	}
	return out
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// listEnv splits a comma/space separated environment value. Empty entries are
// dropped, so a trailing comma is harmless. An unset or empty variable yields
// the supplied default.
func listEnv(key string, def []string) []string {
	raw := env(key, "")
	if raw == "" {
		return append([]string(nil), def...)
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if v := strings.TrimSpace(f); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// dur reads a Go duration ("90s", "24h"). A value that does not parse falls back
// to the default rather than refusing to start: a typo in a tuning knob must not
// take the controller down.
func dur(key string, def time.Duration) time.Duration {
	raw := env(key, "")
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func portFromAddr(addr string) int {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			if n, err := strconv.Atoi(addr[i+1:]); err == nil {
				return n
			}
			break
		}
	}
	return 8080
}
