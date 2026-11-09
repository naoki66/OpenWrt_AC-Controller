package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testNetdevPrefix stands in for "netdev:" because Windows rejects ':' in a
// directory name, so the real prefix cannot be used to build a fixture tree.
const testNetdevPrefix = "netdev-"

// buildTestProbe returns a probe rooted at a temporary mac80211-shaped tree.
func buildTestProbe(t *testing.T, bss map[string][]string) *stationProbe {
	t.Helper()
	p := newStationProbe(buildDebugfs(t, bss))
	p.prefix = testNetdevPrefix
	return p
}

// buildDebugfs creates a fake mac80211 debugfs tree: phy → netdev-<ifname> →
// stations/<mac>. A nil station list means the BSS directory exists and is
// empty, which is the case the probe is meant to short-circuit.
func buildDebugfs(t *testing.T, bss map[string][]string) string {
	t.Helper()
	root := t.TempDir()
	phy := filepath.Join(root, "phy0")
	for ifname, stations := range bss {
		dir := filepath.Join(phy, testNetdevPrefix+ifname, "stations")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		for _, mac := range stations {
			if err := os.MkdirAll(filepath.Join(dir, mac), 0o755); err != nil {
				t.Fatalf("mkdir station %s: %v", mac, err)
			}
		}
	}
	return root
}

func TestStationProbeDistinguishesEmptyFromUnknown(t *testing.T) {
	p := buildTestProbe(t, map[string][]string{
		"wlan0": {"sta-01", "sta-02"},
		"wlan1": nil,
	})

	if present, known := p.has("wlan0"); !present || !known {
		t.Fatalf("wlan0 = (%v, %v), want (true, true)", present, known)
	}
	// The whole point: an empty BSS is a *known* zero and may be skipped.
	if present, known := p.has("wlan1"); present || !known {
		t.Fatalf("wlan1 = (%v, %v), want (false, true)", present, known)
	}
	// A BSS the kernel does not expose must never be reported as empty, or a
	// non-mac80211 driver would lose its client telemetry entirely.
	if present, known := p.has("wlan9"); present || known {
		t.Fatalf("wlan9 = (%v, %v), want (false, false)", present, known)
	}
}

func TestStationProbeFallsBackWhenDebugfsMissing(t *testing.T) {
	p := newStationProbe(filepath.Join(t.TempDir(), "not-mounted"))
	if present, known := p.has("wlan0"); present || known {
		t.Fatalf("missing debugfs = (%v, %v), want (false, false)", present, known)
	}
}

func TestStationProbeTreatsUnreadableRootAsUnknown(t *testing.T) {
	root := t.TempDir()
	// A file where a directory is expected reproduces an unmounted or replaced
	// debugfs without needing permissions tricks.
	if err := os.WriteFile(filepath.Join(root, "phy0"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newStationProbe(root)
	if present, known := p.has("wlan0"); present || known {
		t.Fatalf("unusable root = (%v, %v), want (false, false)", present, known)
	}
}

func TestStationProbeCachesWithinTTL(t *testing.T) {
	p := buildTestProbe(t, map[string][]string{"wlan1": nil})
	root := p.root

	// First probe learns "empty". Delete the directory: within the TTL the
	// cached answer must still be served as a known zero.
	if _, known := p.has("wlan1"); !known {
		t.Fatal("first probe should be known")
	}
	if err := os.RemoveAll(filepath.Join(root, "phy0")); err != nil {
		t.Fatal(err)
	}
	if _, known := p.has("wlan1"); !known {
		t.Fatal("answer inside the TTL must come from the cache")
	}

	// Past the TTL the probe re-reads and reports unknown instead of a stale
	// empty answer: a vanished BSS must not be silently skipped.
	advanced := time.Now().Add(2 * stationProbeTTL)
	p.now = func() time.Time { return advanced }
	if _, known := p.has("wlan1"); known {
		t.Fatal("after the TTL a missing BSS must be unknown")
	}
}

func TestStationProbeRecoversWhenDebugfsAppears(t *testing.T) {
	root := filepath.Join(t.TempDir(), "late")
	p := newStationProbe(root)
	p.prefix = testNetdevPrefix
	if _, known := p.has("wlan1"); known {
		t.Fatal("expected unknown before the tree exists")
	}

	phy := filepath.Join(root, "phy0")
	if err := os.MkdirAll(filepath.Join(phy, testNetdevPrefix+"wlan1", "stations"), 0o755); err != nil {
		t.Fatal(err)
	}
	advanced := time.Now().Add(2 * stationProbeTTL)
	p.now = func() time.Time { return advanced }
	if _, known := p.has("wlan1"); !known {
		t.Fatal("probe must pick up a tree mounted after boot")
	}
}

func TestCollectSkipsIwForEmptyBSS(t *testing.T) {
	probe := buildTestProbe(t, map[string][]string{
		"phy0.0-ap0": {"sta-01"},
		"phy0.1-ap0": nil,
		"phy0.1-ap1": nil,
	})
	collector, runner := newCollector(t, map[string]string{
		"status":                         wirelessStatusFixture,
		"iw dev phy0.0-ap0 station dump": stationDump,
	})
	collector.Probe = probe

	clients := collector.Collect(context.Background())
	if len(clients) != 2 {
		t.Fatalf("clients = %d, want 2", len(clients))
	}

	for _, call := range runner.calls {
		if strings.HasPrefix(call, "iw dev phy0.1-ap") {
			t.Fatalf("iw ran for an empty BSS: %s", call)
		}
	}
	if !hasCall(runner.calls, "iw dev phy0.0-ap0 station dump") {
		t.Fatal("iw must still run for a BSS that has stations")
	}
}

func TestCollectRunsIwWhenPresenceUnknown(t *testing.T) {
	collector, runner := newCollector(t, map[string]string{
		"status":                         wirelessStatusFixture,
		"iw dev phy0.0-ap0 station dump": stationDump,
	})
	// No probe override: the default root does not exist in the test environment.
	collector.Log = slog.New(slog.NewTextHandler(os.Stderr, nil))

	clients := collector.Collect(context.Background())
	if len(clients) != 2 {
		t.Fatalf("clients = %d, want 2", len(clients))
	}
	if !hasCall(runner.calls, "iw dev phy0.0-ap0 station dump") {
		t.Fatal("an unknown presence answer must fall back to iw")
	}
}

func hasCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}
