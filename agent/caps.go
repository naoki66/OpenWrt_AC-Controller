package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/naoki66/ac-controller/shared/proto"
)

// hostapd capability probe.
//
// The template asks for 11k/11v, but the running hostapd is what decides
// whether a BTM can be sent at all: a package can request roaming features on a
// build that was compiled without them, and the controller then shows a
// configured capability that does not exist. This probe reports the methods
// hostapd actually exposes, so the UI can separate "configured" from "works".
//
// The answer changes only when hostapd restarts or a radio is rebuilt, and each
// probe costs one ubus fork per BSS, so it is cached. A failed probe is cached
// too: a broken ubus must not add a subprocess timeout to every heartbeat.
const capsCacheTTL = 5 * time.Minute

// capsCollector probes the hostapd ubus surface, cached for capsCacheTTL.
type capsCollector struct {
	Runner Runner
	Log    *slog.Logger
	// ConfDir overrides the directory holding netifd's generated hostapd
	// configs (/var/run on OpenWrt). Empty means the standard location;
	// tests inject a temporary directory.
	ConfDir string
	// now is overridable for tests.
	now func() time.Time

	mu     sync.Mutex
	caps   *proto.WifiCaps
	at     time.Time
	known  bool
	probes int
}

func newCapsCollector(r Runner, log *slog.Logger) *capsCollector {
	return &capsCollector{Runner: r, Log: log, now: time.Now}
}

// Collect returns the cached capability set, probing at most once per TTL.
// A nil result means "not reported": ubus could not enumerate hostapd, or the
// AP has no BSS to ask about.
func (c *capsCollector) Collect(ctx context.Context) *proto.WifiCaps {
	if c == nil || c.Runner == nil {
		return nil
	}
	now := c.now()

	c.mu.Lock()
	if c.known && now.Sub(c.at) < capsCacheTTL {
		caps := c.caps
		c.mu.Unlock()
		return caps
	}
	c.mu.Unlock()

	caps := c.probe(ctx)

	c.mu.Lock()
	c.caps = caps
	c.at = now
	c.known = true
	c.probes++
	c.mu.Unlock()
	return caps
}

// probe asks ubus which hostapd objects exist and which methods each exposes.
//
// A method name appearing in `ubus -v list` is the whole test: it is the same
// surface the agent calls, so a method listed there is one that would answer.
// K needs both the read and the write side; V needs the BTM entry point; R
// (802.11r/FT) has no ubus method at all and is read from netifd's generated
// hostapd configs instead (ftScan).
func (c *capsCollector) probe(ctx context.Context) *proto.WifiCaps {
	out, err := c.Runner.Run(ctx, "ubus", "list")
	if err != nil {
		c.Log.Debug("caps: ubus list unavailable", "err", err)
		return nil
	}

	var names []string
	for _, line := range strings.Split(out, "\n") {
		ifname, ok := strings.CutPrefix(strings.TrimSpace(line), "hostapd.")
		if !ok || ifname == "" {
			continue
		}
		names = append(names, ifname)
	}
	if len(names) == 0 {
		c.Log.Debug("caps: no hostapd objects")
		return nil
	}

	caps := &proto.WifiCaps{BSS: len(names), K: true, V: true, R: true}
	ft, md := c.ftScan()
	for _, ifname := range names {
		// The verbose listing is the only cheap way to see the method set. One
		// failed BSS means "not every BSS supports it", which is exactly how the
		// flag is defined; do not retry it here.
		methods, err := c.Runner.Run(ctx, "ubus", "-v", "list", "hostapd."+ifname)
		if err != nil {
			c.Log.Debug("caps: ubus -v list failed", "ifname", ifname, "err", err)
			caps.K = false
			caps.V = false
			continue
		}
		if !strings.Contains(methods, "rrm_nr_get_own") || !strings.Contains(methods, "rrm_nr_set") {
			caps.K = false
		}
		if !strings.Contains(methods, "bss_transition_request") {
			caps.V = false
		}
		// Same all-BSS semantics as K/V: one BSS without FT means the AP as a
		// whole does not do 11r, since a roaming client may land on any BSS.
		enabled, seen := ft[ifname]
		if !seen || !enabled {
			caps.R = false
		}
	}
	if caps.R {
		caps.MD = md
	}
	return caps
}

// ftScan reads the hostapd config files netifd generated for the running
// daemon (/var/run/hostapd-<phy>.conf on OpenWrt) and maps every BSS to its
// 802.11r setting. FT has no ubus method to probe — nothing in the ubus
// surface changes with ieee80211r — so the generated config, the exact input
// the running daemon was started with, is the closest thing to runtime truth.
// The files change only when wifi is (re)configured, and the caller caches the
// whole probe, so the read cost is amortised. An empty map means the configs
// could not be read and 11r stays "not verified" (false).
//
// A BSS section is introduced by "interface=" (first BSS of the phy) or
// "bss=" (every following one); the FT options live inside that section.
func (c *capsCollector) ftScan() (map[string]bool, string) {
	patterns := []string{
		filepath.Join(c.confDir(), "hostapd-*.conf"),
		filepath.Join(c.confDir(), "hostapd", "*.conf"),
	}
	ft := map[string]bool{}
	md := ""
	var files []string
	for _, p := range patterns {
		matches, err := filepath.Glob(p)
		if err != nil {
			c.Log.Debug("caps: bad glob", "pattern", p, "err", err)
			continue
		}
		files = append(files, matches...)
	}
	if len(files) == 0 {
		c.Log.Debug("caps: no hostapd config files, 11r not verified")
		return ft, md
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			c.Log.Debug("caps: read hostapd config failed", "file", file, "err", err)
			continue
		}
		cur := ""
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if name, ok := strings.CutPrefix(line, "interface="); ok {
				cur = strings.TrimSpace(name)
				continue
			}
			if name, ok := strings.CutPrefix(line, "bss="); ok {
				cur = strings.TrimSpace(name)
				continue
			}
			if cur == "" {
				continue
			}
			if val, ok := strings.CutPrefix(line, "ieee80211r="); ok {
				ft[cur] = strings.TrimSpace(val) == "1"
				continue
			}
			if val, ok := strings.CutPrefix(line, "mobility_domain="); ok {
				if md == "" {
					md = strings.TrimSpace(val)
				}
			}
		}
	}
	return ft, md
}

// confDir returns the directory holding netifd's generated hostapd configs.
func (c *capsCollector) confDir() string {
	if c.ConfDir != "" {
		return c.ConfDir
	}
	return "/var/run"
}
