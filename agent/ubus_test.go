package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// Client-layer tests: the object-id cache, the stale-id retry and the breaker.
// They use a fake transport, so they run everywhere — no ubusd, no Linux.

type fakeTransport struct {
	lookupFn func(ctx context.Context, obj string) (uint32, error)
	invokeFn func(ctx context.Context, id uint32, method string) (map[string]any, error)
	closes   int
}

func (f *fakeTransport) Lookup(ctx context.Context, obj string) (uint32, error) {
	if f.lookupFn == nil {
		return 0, errors.New("no lookup configured")
	}
	return f.lookupFn(ctx, obj)
}

func (f *fakeTransport) Invoke(ctx context.Context, id uint32, method string) (map[string]any, error) {
	if f.invokeFn == nil {
		return nil, errors.New("no invoke configured")
	}
	return f.invokeFn(ctx, id, method)
}

func (f *fakeTransport) Close() error {
	f.closes++
	return nil
}

func testUbusClient(tr ubusTransport) *ubusClient {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newUbusClient(log)
	c.tr = tr
	return c
}

func TestUbusClientCachesObjectID(t *testing.T) {
	lookups, invokes := 0, 0
	tr := &fakeTransport{
		lookupFn: func(context.Context, string) (uint32, error) {
			lookups++
			return 5, nil
		},
		invokeFn: func(_ context.Context, id uint32, method string) (map[string]any, error) {
			invokes++
			if id != 5 || method != "status" {
				t.Errorf("invoke(%d, %q)", id, method)
			}
			return map[string]any{"up": true}, nil
		},
	}
	c := testUbusClient(tr)

	for i := 0; i < 3; i++ {
		if _, err := c.Call(context.Background(), "network.wireless", "status"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if lookups != 1 {
		t.Errorf("lookups = %d, want 1: the id must be cached", lookups)
	}
	if invokes != 3 {
		t.Errorf("invokes = %d, want 3", invokes)
	}
}

// TestUbusClientRetriesStaleObjectID covers the hostapd restart that follows
// every `wifi reload`: the cached id points at a destroyed object, and one fresh
// lookup has to recover instead of falling back to the CLI for the rest of the
// minute.
func TestUbusClientRetriesStaleObjectID(t *testing.T) {
	lookups := 0
	tr := &fakeTransport{
		lookupFn: func(context.Context, string) (uint32, error) {
			lookups++
			return 2, nil
		},
		invokeFn: func(_ context.Context, id uint32, _ string) (map[string]any, error) {
			if id == 1 {
				return nil, statusError("invoke get_status", ubusStatusNotFound)
			}
			return map[string]any{"status": "ENABLED"}, nil
		},
	}
	c := testUbusClient(tr)
	// The id from before the reload. ubusd never reuses ids, so it is now dead.
	c.ids["hostapd.phy0.0-ap0"] = 1

	got, err := c.Call(context.Background(), "hostapd.phy0.0-ap0", "get_status")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got["status"] != "ENABLED" {
		t.Fatalf("reply = %#v", got)
	}
	if lookups != 1 {
		t.Fatalf("lookups = %d, want 1 (one fresh lookup after the stale id)", lookups)
	}
	if id := c.ids["hostapd.phy0.0-ap0"]; id != 2 {
		t.Fatalf("cached id = %d, want the refreshed 2", id)
	}
}

func TestUbusClientBreaksAfterRepeatedTransportFailures(t *testing.T) {
	lookups := 0
	tr := &fakeTransport{
		lookupFn: func(context.Context, string) (uint32, error) {
			lookups++
			return 0, errors.New("dial unix /var/run/ubus/ubus.sock: no such file")
		},
	}
	c := testUbusClient(tr)
	now := time.Now()
	c.Now = func() time.Time { return now }

	for i := 0; i < ubusFailureLimit; i++ {
		if _, err := c.Call(context.Background(), "network.wireless", "status"); err == nil {
			t.Fatalf("call %d: expected a failure", i)
		}
	}
	if lookups != ubusFailureLimit {
		t.Fatalf("lookups = %d, want %d", lookups, ubusFailureLimit)
	}

	// The breaker is now open: no transport call, and the caller gets the
	// sentinel every fallback path keys on.
	_, err := c.Call(context.Background(), "network.wireless", "status")
	if !errors.Is(err, ErrUbusUnavailable) {
		t.Fatalf("during backoff: err = %v, want ErrUbusUnavailable", err)
	}
	if lookups != ubusFailureLimit {
		t.Fatalf("lookups = %d during backoff, want %d", lookups, ubusFailureLimit)
	}

	// After the backoff window it tries again.
	now = now.Add(ubusBackoff + time.Second)
	if _, err := c.Call(context.Background(), "network.wireless", "status"); err == nil {
		t.Fatal("expected the retry to fail too, the transport is still broken")
	}
	if lookups != ubusFailureLimit+1 {
		t.Fatalf("lookups = %d after the backoff, want %d", lookups, ubusFailureLimit+1)
	}
}

// TestUbusClientStatusErrorIsNotATransportFailure keeps the breaker honest: the
// daemon answering "object not found" means the socket works, and a board
// without hostapd objects must not end up in backoff.
func TestUbusClientStatusErrorIsNotATransportFailure(t *testing.T) {
	invokes := 0
	tr := &fakeTransport{
		lookupFn: func(context.Context, string) (uint32, error) { return 3, nil },
		invokeFn: func(context.Context, uint32, string) (map[string]any, error) {
			invokes++
			return nil, statusError("invoke get_status", ubusStatusNotFound)
		},
	}
	c := testUbusClient(tr)

	for i := 0; i < ubusFailureLimit+3; i++ {
		if _, err := c.Call(context.Background(), "hostapd.missing", "get_status"); err == nil {
			t.Fatalf("call %d: expected a status error", i)
		}
	}
	if invokes < ubusFailureLimit+3 {
		t.Fatalf("invokes = %d, want every call to reach the transport", invokes)
	}
	if !c.skipTill.IsZero() {
		t.Fatalf("breaker tripped at %v on status errors", c.skipTill)
	}
}

func TestUbusClientNilIsUnavailable(t *testing.T) {
	var c *ubusClient
	if _, err := c.Call(context.Background(), "network.wireless", "status"); !errors.Is(err, ErrUbusUnavailable) {
		t.Fatalf("err = %v, want ErrUbusUnavailable", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close on a nil client: %v", err)
	}
}

// TestUbusClientCallJSON pins the reason the collectors can be swapped without
// touching their parsers: the reply comes back as JSON, so it feeds the same
// json.Unmarshal the CLI text did.
func TestUbusClientCallJSON(t *testing.T) {
	tr := &fakeTransport{
		lookupFn: func(context.Context, string) (uint32, error) { return 1, nil },
		invokeFn: func(context.Context, uint32, string) (map[string]any, error) {
			return map[string]any{"b": true, "a": int64(1)}, nil
		},
	}
	c := testUbusClient(tr)

	got, err := c.CallJSON(context.Background(), "hostapd.x", "get_status")
	if err != nil {
		t.Fatalf("CallJSON: %v", err)
	}
	if string(got) != `{"a":1,"b":true}` {
		t.Fatalf("json = %s", got)
	}
}
