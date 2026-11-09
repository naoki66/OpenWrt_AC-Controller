package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

func countCalls(calls []string, want string) int {
	n := 0
	for _, c := range calls {
		if c == want {
			n++
		}
	}
	return n
}

func TestSnapshotReusesSampleWithinWindow(t *testing.T) {
	c, runner := newCollector(t, map[string]string{
		"status":                         wirelessStatusFixture,
		"iw dev phy0.1-ap0 station dump": stationDump,
		"iw dev phy0.0-ap0 station dump": "",
		"iw dev phy0.1-ap1 station dump": "",
	})
	ctx := context.Background()

	first := c.Snapshot(ctx, time.Minute)
	if len(first) != 2 {
		t.Fatalf("clients = %d, want 2", len(first))
	}
	iwCalls := countCalls(runner.calls, "iw dev phy0.1-ap0 station dump")
	if iwCalls != 1 {
		t.Fatalf("iw calls after first snapshot = %d, want 1", iwCalls)
	}

	// The heartbeat rides on the loop's sample: a second read inside the window
	// must not touch the device at all.
	second := c.Snapshot(ctx, time.Minute)
	if countCalls(runner.calls, "iw dev phy0.1-ap0 station dump") != iwCalls {
		t.Fatal("second snapshot re-sampled despite a fresh cache entry")
	}
	if len(second) != len(first) || second[0].MAC != first[0].MAC {
		t.Fatalf("cached snapshot differs: %+v vs %+v", second, first)
	}
}

func TestSnapshotResamplesAfterWindow(t *testing.T) {
	c, runner := newCollector(t, map[string]string{
		"status":                         wirelessStatusFixture,
		"iw dev phy0.1-ap0 station dump": stationDump,
	})
	ctx := context.Background()

	c.Snapshot(ctx, time.Hour)
	// A zero budget always samples: the steering loop relies on this to see a
	// fresh signal on every tick.
	c.Snapshot(ctx, 0)
	if n := countCalls(runner.calls, "iw dev phy0.1-ap0 station dump"); n != 2 {
		t.Fatalf("iw calls = %d, want 2", n)
	}
}

func TestSnapshotCachesGenuinelyEmptyList(t *testing.T) {
	c, runner := newCollector(t, map[string]string{
		"status": `{"radio0": {"up": true, "interfaces": []}}`,
	})
	ctx := context.Background()

	first := c.Snapshot(ctx, time.Minute)
	if first == nil {
		t.Fatal("a successful sample with no stations must not be nil")
	}
	if len(first) != 0 {
		t.Fatalf("clients = %d, want 0", len(first))
	}
	c.Snapshot(ctx, time.Minute)

	// "No clients" is a real answer and is worth caching; re-reading it would
	// put the most frequent cost back on an AP that simply has nobody on it.
	if n := countCalls(runner.calls, "ubus call network.wireless status"); n != 1 {
		t.Fatalf("wireless status calls = %d, want 1", n)
	}
}

// failingRunner simulates a ubus that cannot answer.
type failingRunner struct{ calls int }

func (r *failingRunner) Run(context.Context, string, ...string) (string, error) {
	r.calls++
	return "", errors.New("ubus unavailable")
}

func TestSnapshotDoesNotCacheFailures(t *testing.T) {
	r := &failingRunner{}
	c, _ := newCollector(t, nil)
	c.Runner = r
	ctx := context.Background()

	if got := c.Snapshot(ctx, time.Hour); got != nil {
		t.Fatalf("failed sample = %v, want nil", got)
	}
	// nil means "not reported"; caching it would blank out the controller's
	// client table for a whole cycle because of one transient failure.
	if got := c.Snapshot(ctx, time.Hour); got != nil {
		t.Fatalf("failed sample = %v, want nil", got)
	}
	if r.calls != 2 {
		t.Fatalf("attempts = %d, want 2 (a failure must not be cached)", r.calls)
	}
}

func TestSteerTickSamplesThroughSharedSnapshot(t *testing.T) {
	f := newSteerFixture(t)
	f.loop.SetPolicy(defaultPolicy())
	ctx := context.Background()

	f.loop.tick(ctx)
	first := countCalls(f.runner.Calls(), "iw dev phy0.1-ap0 station dump")
	if first != 1 {
		t.Fatalf("first tick ran iw %d times, want 1", first)
	}

	// Consecutive ticks inside the reuse window must reuse the collection: that
	// is what makes the heartbeat free when the loop is already sampling.
	f.loop.tick(ctx)
	if n := countCalls(f.runner.Calls(), "iw dev phy0.1-ap0 station dump"); n != first {
		t.Fatalf("second tick re-sampled: %d iw calls", n)
	}
}
