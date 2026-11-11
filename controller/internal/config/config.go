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
// complaining. The wire credentials are HKDF-derived and 64 hex characters;
// this floor only guards the single human-facing value against a trivial guess.
const minSecretBytes = 12

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
	// AdminUser / AdminPassword seed the login account on first start. After
	// seeding the credentials live in the settings table, so later edits of
	// the environment have no effect — change them from the system page.
	AdminUser     string
	AdminPassword string
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
	// Profile names the deployment preset. "openwrt" trims the features that
	// cost flash writes and memory peaks on a router; anything else keeps the
	// full feature set.
	Profile string
	// TelemetryHistory controls whether client and radio samples are stored for
	// the charts. Turning it off keeps the live views but removes the largest
	// source of database growth.
	TelemetryHistory bool
	// FirmwareEnabled controls the image repository and the AP upgrade channel.
	// When false the binary still runs, but no image is stored or offered.
	FirmwareEnabled bool
	// PackageHistory caps how many signed revisions are kept per device. Zero
	// keeps every revision.
	PackageHistory int
	// EventsMax caps the audit log. Zero keeps every event.
	EventsMax int
}

func Load() Config {
	profile := strings.ToLower(strings.TrimSpace(env("AC_PROFILE", "full")))
	openwrt := profile == "openwrt"
	c := Config{
		Profile: profile,
		DataDir: env("AC_DATA_DIR", "./data"),
		// 8090 rather than 8080: the latter is commonly taken by the host's
		// apache2 on the NAS deployments this controller targets.
		HTTPAddr:        env("AC_HTTP_ADDR", ":8090"),
		DiscoverAddr:    env("AC_DISCOVER_ADDR", ":29820"),
		SharedSecret:    env("AC_SHARED_SECRET", ""),
		AdminToken:      env("AC_ADMIN_TOKEN", ""),
		AdminUser:       env("AC_ADMIN_USER", "admin"),
		AdminPassword:   env("AC_ADMIN_PASSWORD", ""),
		AdvertiseIP:     env("AC_ADVERTISE_IP", ""),
		SampleInterval:  dur("AC_SAMPLE_INTERVAL", 60*time.Second),
		SampleRetention: dur("AC_SAMPLE_RETENTION", 7*24*time.Hour),
		OfflineAfter:    dur("AC_OFFLINE_AFTER", 90*time.Second),
		// Portable default: public sources only. The gateway address is site
		// specific, so an operator who has an internal NTP server puts it first
		// from the page or AC_NTP_SERVERS. The controller is deliberately not in
		// the list; an AC outage must not remove time.
		NTPServers: listEnv("AC_NTP_SERVERS", []string{
			"ntp.tencent.com", "ntp1.aliyun.com", "ntp.ntsc.ac.cn", "cn.ntp.org.cn",
		}),
	}
	c.HTTPPort = portFromAddr(c.HTTPAddr)
	if v := env("AC_HTTP_PORT", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.HTTPPort = n
		}
	}

	c.TelemetryHistory = boolEnv("AC_TELEMETRY_HISTORY", !openwrt)
	c.FirmwareEnabled = boolEnv("AC_FIRMWARE", !openwrt)
	c.PackageHistory = intEnv("AC_PACKAGE_HISTORY", 0)
	c.EventsMax = intEnv("AC_EVENTS_MAX", 0)
	if openwrt {
		// Router-friendly defaults, applied only when the operator did not ask
		// for something else explicitly: keep one signed revision per AP, bound
		// the audit log, sample half as often and retain telemetry for one day.
		if !envSet("AC_PACKAGE_HISTORY") {
			c.PackageHistory = 1
		}
		if !envSet("AC_EVENTS_MAX") {
			c.EventsMax = 500
		}
		if !envSet("AC_SAMPLE_INTERVAL") {
			c.SampleInterval = 120 * time.Second
		}
		if !envSet("AC_SAMPLE_RETENTION") {
			c.SampleRetention = 24 * time.Hour
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

// SecretWarnings reports optional credentials and unsafe credential reuse.
func (c Config) SecretWarnings() []string {
	var out []string
	if c.SharedSecret == "" {
		out = append(out, "AC_SHARED_SECRET is empty: no AP peer coordination key is configured on the controller")
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

// envSet reports whether the operator supplied a non-empty value. It separates
// "unset, use the profile default" from "explicitly set to zero/off".
func envSet(key string) bool {
	v, ok := os.LookupEnv(key)
	return ok && strings.TrimSpace(v) != ""
}

func boolEnv(key string, def bool) bool {
	raw := strings.ToLower(strings.TrimSpace(env(key, "")))
	if raw == "" {
		return def
	}
	switch raw {
	case "1", "true", "on", "yes":
		return true
	case "0", "false", "off", "no":
		return false
	default:
		return def
	}
}

func intEnv(key string, def int) int {
	raw := env(key, "")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return def
	}
	return n
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
	return 8090
}
