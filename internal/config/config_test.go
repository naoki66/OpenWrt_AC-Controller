package config

import (
	"strings"
	"testing"
)

// The credential warnings are the only startup-time defence against an open
// management API or a shared secret that doubles as the admin token, so the
// interesting combinations are pinned here.
func TestNTPDefaults(t *testing.T) {
	t.Setenv("AC_NTP_SERVERS", "")
	cfg := Load()
	want := []string{"ntp.ntsc.ac.cn", "ntp1.aliyun.com", "ntp.tencent.com", "cn.pool.ntp.org"}
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
