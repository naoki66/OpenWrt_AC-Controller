package agent

import (
	"context"
	"errors"
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
