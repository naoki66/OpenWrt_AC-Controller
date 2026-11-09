package config

import (
	"strings"
	"testing"
	"time"
)

// The openwrt profile is the whole point of the runtime trim: it must turn off
// the two subsystems that cost flash writes and memory peaks, and it must not
// change anything when the operator supplies an explicit value.
func TestOpenWRTProfileDefaults(t *testing.T) {
	for _, key := range []string{
		"AC_TELEMETRY_HISTORY", "AC_FIRMWARE", "AC_PACKAGE_HISTORY",
		"AC_EVENTS_MAX", "AC_SAMPLE_INTERVAL", "AC_SAMPLE_RETENTION",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("AC_PROFILE", "openwrt")

	cfg := Load()
	if cfg.Profile != "openwrt" {
		t.Fatalf("profile = %q", cfg.Profile)
	}
	if cfg.TelemetryHistory {
		t.Error("telemetry history should be off")
	}
	if cfg.FirmwareEnabled {
		t.Error("firmware repository should be off")
	}
	if cfg.PackageHistory != 1 {
		t.Errorf("package history = %d, want 1", cfg.PackageHistory)
	}
	if cfg.EventsMax != 500 {
		t.Errorf("events max = %d, want 500", cfg.EventsMax)
	}
	if cfg.SampleInterval != 120*time.Second {
		t.Errorf("sample interval = %s, want 120s", cfg.SampleInterval)
	}
	if cfg.SampleRetention != 24*time.Hour {
		t.Errorf("sample retention = %s, want 24h", cfg.SampleRetention)
	}
}

func TestFullProfileKeepsFeatures(t *testing.T) {
	for _, key := range []string{
		"AC_TELEMETRY_HISTORY", "AC_FIRMWARE", "AC_PACKAGE_HISTORY",
		"AC_EVENTS_MAX", "AC_SAMPLE_INTERVAL", "AC_SAMPLE_RETENTION",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("AC_PROFILE", "full")

	cfg := Load()
	if !cfg.TelemetryHistory || !cfg.FirmwareEnabled {
		t.Fatalf("full profile disabled a feature: telemetry=%v firmware=%v",
			cfg.TelemetryHistory, cfg.FirmwareEnabled)
	}
	if cfg.PackageHistory != 0 || cfg.EventsMax != 0 {
		t.Errorf("limits should be unlimited: packages=%d events=%d",
			cfg.PackageHistory, cfg.EventsMax)
	}
	if cfg.SampleInterval != 60*time.Second || cfg.SampleRetention != 7*24*time.Hour {
		t.Errorf("sample defaults = %s/%s", cfg.SampleInterval, cfg.SampleRetention)
	}
}

func TestOpenWRTProfileHonoursExplicitValues(t *testing.T) {
	t.Setenv("AC_PROFILE", "openwrt")
	t.Setenv("AC_TELEMETRY_HISTORY", "1")
	t.Setenv("AC_FIRMWARE", "true")
	t.Setenv("AC_PACKAGE_HISTORY", "3")
	t.Setenv("AC_EVENTS_MAX", "0")
	t.Setenv("AC_SAMPLE_INTERVAL", "90s")
	t.Setenv("AC_SAMPLE_RETENTION", "48h")

	cfg := Load()
	if !cfg.TelemetryHistory || !cfg.FirmwareEnabled {
		t.Fatal("explicit on was overridden by the profile")
	}
	if cfg.PackageHistory != 3 || cfg.EventsMax != 0 {
		t.Errorf("explicit limits ignored: packages=%d events=%d", cfg.PackageHistory, cfg.EventsMax)
	}
	if cfg.SampleInterval != 90*time.Second || cfg.SampleRetention != 48*time.Hour {
		t.Errorf("explicit sample settings ignored: %s/%s", cfg.SampleInterval, cfg.SampleRetention)
	}
}

// The credential warnings are the only startup-time defence against an open
// management API or a shared secret that doubles as the admin token, so the
// interesting combinations are pinned here.
func TestNTPDefaults(t *testing.T) {
	t.Setenv("AC_NTP_SERVERS", "")
	cfg := Load()
	want := []string{"ntp.tencent.com", "ntp1.aliyun.com", "ntp.ntsc.ac.cn", "cn.ntp.org.cn"}
	if len(cfg.NTPServers) != len(want) {
		t.Fatalf("default NTP = %v, want %v", cfg.NTPServers, want)
	}
	for i := range want {
		if cfg.NTPServers[i] != want[i] {
			t.Fatalf("default NTP = %v, want %v", cfg.NTPServers, want)
		}
	}

	t.Setenv("AC_NTP_SERVERS", "192.168.1.1, pool.ntp.org")
	cfg = Load()
	if len(cfg.NTPServers) != 2 || cfg.NTPServers[0] != "192.168.1.1" ||
		cfg.NTPServers[1] != "pool.ntp.org" {
		t.Fatalf("env NTP = %v", cfg.NTPServers)
	}
}

func TestSecretWarnings(t *testing.T) {
	const strong = "0123456789abcdef0123456789abcdef"
	const other = "fedcba9876543210fedcba9876543210"

	cases := []struct {
		name string
		cfg  Config
		want []string
	}{
		{
			name: "nothing configured is open on purpose",
			cfg:  Config{},
			want: []string{"AC_SHARED_SECRET is empty", "AC_ADMIN_TOKEN is empty"},
		},
		{
			name: "two different strong credentials warn about nothing",
			cfg:  Config{SharedSecret: strong, AdminToken: other},
		},
		{
			name: "the same value defeats the split",
			cfg:  Config{SharedSecret: strong, AdminToken: strong},
			want: []string{"AC_ADMIN_TOKEN equals AC_SHARED_SECRET"},
		},
		{
			name: "short admin token",
			cfg:  Config{SharedSecret: strong, AdminToken: "abc"},
			want: []string{"AC_ADMIN_TOKEN is shorter than"},
		},
		{
			name: "short shared secret",
			cfg:  Config{SharedSecret: "abc", AdminToken: other},
			want: []string{"AC_SHARED_SECRET is shorter than"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.cfg.SecretWarnings()
			if len(got) != len(c.want) {
				t.Fatalf("warnings = %v, want %v", got, c.want)
			}
			for _, want := range c.want {
				found := false
				for _, w := range got {
					if strings.Contains(w, want) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("warnings = %v, none contains %q", got, want)
				}
			}
		})
	}
}
