package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// capsRunner impersonates the two ubus listings the probe uses.
type capsRunner struct {
	mu        sync.Mutex
	list      string
	listErr   error
	methods   map[string]string
	methodErr map[string]bool
	calls     int
}

func (r *capsRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++

	if name == "ubus" && len(args) == 1 && args[0] == "list" {
		if r.listErr != nil {
			return "", r.listErr
		}
		return r.list, nil
	}
	if name == "ubus" && len(args) == 3 && args[0] == "-v" && args[1] == "list" {
		obj := strings.TrimPrefix(args[2], "hostapd.")
		if r.methodErr[obj] {
			return "", errors.New("object not found")
		}
		return r.methods[obj], nil
	}
	return "", errors.New("unexpected command")
}

func capsFixture(list string, methods map[string]string) (*capsCollector, *capsRunner, *guardClock) {
	r := &capsRunner{list: list, methods: methods, methodErr: map[string]bool{}}
	c := newCapsCollector(r, testLog())
	clock := &guardClock{t: time.Unix(1793698800, 0)}
	c.now = clock.now
	return c, r, clock
}

const fullMethods = `hostapd.phy0-ap0 @abc
{
	"rrm_nr_get_own": {},
	"rrm_nr_set": {},
	"bss_transition_request": {}
}`

func TestCapsCollectAllSupported(t *testing.T) {
	c, runner, _ := capsFixture("hostapd.phy0-ap0\nhostapd.phy0-ap1\n", map[string]string{
		"phy0-ap0": fullMethods,
		"phy0-ap1": fullMethods,
	})

	caps := c.Collect(context.Background())
	if caps == nil {
		t.Fatal("caps = nil, want a report")
	}
	if caps.BSS != 2 || !caps.K || !caps.V {
		t.Fatalf("caps = %+v, want BSS 2 K/V true", *caps)
	}
	// Two listings plus one per BSS.
	if runner.calls != 3 {
		t.Fatalf("ubus calls = %d, want 3", runner.calls)
	}

	// Inside the TTL the cached answer is returned without another probe.
	c.Collect(context.Background())
	if runner.calls != 3 {
		t.Fatalf("cached probe made %d extra ubus calls", runner.calls-3)
	}
}

func TestCapsCollectPartialSupport(t *testing.T) {
	c, _, _ := capsFixture("hostapd.phy0-ap0\nhostapd.phy0-ap1\n", map[string]string{
		"phy0-ap0": fullMethods,
		// One BSS lacks the write half of 11k but still exposes 11v.
		"phy0-ap1": `{"rrm_nr_get_own": {}, "bss_transition_request": {}}`,
	})

	caps := c.Collect(context.Background())
	if caps == nil {
		t.Fatal("caps = nil, want a report")
	}
	if caps.K {
		t.Fatal("K must be false when one BSS cannot write its neighbour list")
	}
	if !caps.V {
		t.Fatal("V must still be true: every BSS exposes bss_transition_request")
	}
	if caps.BSS != 2 {
		t.Fatalf("BSS = %d, want 2", caps.BSS)
	}
}

func TestCapsCollectMethodsMissing(t *testing.T) {
	c, _, _ := capsFixture("hostapd.phy0-ap0\n", map[string]string{
		"phy0-ap0": `{"rrm_nr_get_own": {}, "rrm_nr_set": {}}`,
	})

	caps := c.Collect(context.Background())
	if caps == nil {
		t.Fatal("caps = nil, want a report")
	}
	if !caps.K || caps.V {
		t.Fatalf("caps = %+v, want K true and V false", *caps)
	}
}

func TestCapsCollectNoHostapdObjects(t *testing.T) {
	c, _, _ := capsFixture("network.interface\nsystem\n", nil)

	if caps := c.Collect(context.Background()); caps != nil {
		t.Fatalf("caps = %+v, want nil when hostapd exposes nothing", *caps)
	}
}

func TestCapsCollectListFailure(t *testing.T) {
	c, runner, clock := capsFixture("", nil)
	runner.listErr = errors.New("ubus unavailable")

	if caps := c.Collect(context.Background()); caps != nil {
		t.Fatalf("caps = %+v, want nil when ubus list fails", *caps)
	}

	// A failed probe is cached as well: an offline ubus must not add a
	// subprocess to every heartbeat.
	clock.advance(time.Minute)
	c.Collect(context.Background())
	if runner.calls != 1 {
		t.Fatalf("ubus calls = %d, want 1 within the TTL", runner.calls)
	}
}

func TestCapsCollectMethodCallFailure(t *testing.T) {
	c, runner, _ := capsFixture("hostapd.phy0-ap0\nhostapd.phy0-ap1\n", map[string]string{
		"phy0-ap0": fullMethods,
		"phy0-ap1": fullMethods,
	})
	runner.methodErr["phy0-ap1"] = true

	caps := c.Collect(context.Background())
	if caps == nil {
		t.Fatal("caps = nil, want a report")
	}
	if caps.K || caps.V {
		t.Fatalf("caps = %+v, want K/V false when a BSS cannot be inspected", *caps)
	}
}

// ftConf writes a netifd-style hostapd config: the first BSS of a phy uses
// "interface=", later BSSs use "bss=", exactly like /lib/netifd/hostapd.sh.
func ftConf(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const ftConfTwoBSS = `driver=nl80211
channel=36
# Setup interface: phy0-ap0
interface=phy0-ap0
ssid2="Main"
ieee80211r=1
mobility_domain=ab12
wpa_key_mgmt=FT-PSK
# Setup interface: phy0-ap1
bss=phy0-ap1
ssid2="Guest"
ieee80211r=1
wpa_key_mgmt=FT-PSK
`

func TestCapsFTEnabledEveryBSS(t *testing.T) {
	c, _, _ := capsFixture("hostapd.phy0-ap0\nhostapd.phy0-ap1\n", map[string]string{
		"phy0-ap0": fullMethods,
		"phy0-ap1": fullMethods,
	})
	c.ConfDir = t.TempDir()
	ftConf(t, c.ConfDir, "hostapd-phy0.conf", ftConfTwoBSS)

	caps := c.Collect(context.Background())
	if caps == nil {
		t.Fatal("caps = nil, want a report")
	}
	if !caps.R {
		t.Fatal("R must be true when every BSS sets ieee80211r=1")
	}
	if caps.MD != "ab12" {
		t.Fatalf("MD = %q, want the mobility domain from the config", caps.MD)
	}
}

func TestCapsFTPartialBSS(t *testing.T) {
	c, _, _ := capsFixture("hostapd.phy0-ap0\nhostapd.phy0-ap1\n", map[string]string{
		"phy0-ap0": fullMethods,
		"phy0-ap1": fullMethods,
	})
	c.ConfDir = t.TempDir()
	ftConf(t, c.ConfDir, "hostapd-phy0.conf", `driver=nl80211
interface=phy0-ap0
ieee80211r=1
bss=phy0-ap1
ieee80211r=0
`)

	caps := c.Collect(context.Background())
	if caps == nil {
		t.Fatal("caps = nil, want a report")
	}
	if caps.R {
		t.Fatal("R must be false when one BSS disables 11r")
	}
	if caps.MD != "" {
		t.Fatalf("MD = %q, want empty when 11r is not verified", caps.MD)
	}
}

func TestCapsFTNoConfigFiles(t *testing.T) {
	c, _, _ := capsFixture("hostapd.phy0-ap0\n", map[string]string{
		"phy0-ap0": fullMethods,
	})
	// Empty ConfDir points at a directory with no hostapd configs: 11r must
	// degrade to "not verified" instead of claiming support.
	c.ConfDir = t.TempDir()

	caps := c.Collect(context.Background())
	if caps == nil {
		t.Fatal("caps = nil, want a report")
	}
	if caps.R || caps.MD != "" {
		t.Fatalf("caps = %+v, want R false without readable configs", *caps)
	}
	if !caps.K || !caps.V {
		t.Fatalf("caps = %+v, K/V probing must be unaffected", *caps)
	}
}

func TestCapsFTBSSMissingFromConfig(t *testing.T) {
	c, _, _ := capsFixture("hostapd.phy0-ap0\nhostapd.phy0-ap1\n", map[string]string{
		"phy0-ap0": fullMethods,
		"phy0-ap1": fullMethods,
	})
	c.ConfDir = t.TempDir()
	// phy1's config is missing: ap1 cannot be verified, so the AP as a whole
	// must not claim 11r even though ap0's section enables it.
	ftConf(t, c.ConfDir, "hostapd-phy0.conf", `interface=phy0-ap0
ieee80211r=1
`)

	caps := c.Collect(context.Background())
	if caps == nil {
		t.Fatal("caps = nil, want a report")
	}
	if caps.R {
		t.Fatal("R must be false when a BSS cannot be found in any config")
	}
}
