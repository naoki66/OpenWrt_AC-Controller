package model

import (
	"net"
	"regexp"
	"strings"
)

// NTP server lists are small and site-wide. Normalization lives here so the
// controller and the agent agree on what is a usable time source.

const NTPMaxServers = 8

var ntpHostRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)

// NormalizeNTPServers keeps only plausible, deduplicated time sources. The
// runner passes each value as one argv element, so this is a sanity check
// rather than a shell-escaping boundary; it still keeps a malformed controller
// setting from breaking /etc/config/system.
func NormalizeNTPServers(list []string) []string {
	out := make([]string, 0, min(len(list), NTPMaxServers))
	seen := map[string]bool{}
	for _, raw := range list {
		s := strings.ToLower(strings.TrimSpace(raw))
		if s == "" || len(s) > 255 {
			continue
		}
		if net.ParseIP(s) == nil && !ntpHostRe.MatchString(s) {
			continue
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) >= NTPMaxServers {
			break
		}
	}
	return out
}
