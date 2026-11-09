package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// failingBSSReader stands in for a kernel that rejects the interface dump.
type failingBSSReader struct{ calls int }

func (f *failingBSSReader) Name() string { return "nl80211" }
func (f *failingBSSReader) BSS(context.Context) ([]bssInfo, error) {
	f.calls++
	return nil, errors.New("netlink error: operation not supported")
}

// failingNeighbourReader stands in for the same failure on the address table.
type failingNeighbourReader struct{ calls int }

func (f *failingNeighbourReader) Name() string { return "netlink" }
func (f *failingNeighbourReader) Neighbours(context.Context) (map[string]string, error) {
	f.calls++
	return nil, errors.New("netlink error: operation not supported")
}

// AC_NETLINK=off must restore the exact commands the agent used before the
// direct-kernel paths existed: ubus for the BSS list, ip for the addresses. The
// escape hatch is only worth having if it really is the old behaviour.
func TestNetlinkOffUsesUbusAndIP(t *testing.T) {
	c, runner := newCollector(t, map[string]string{
		"status":                         wirelessStatusFixture,
		"iw dev phy0.1-ap0 station dump": stationDump,
		"ip neigh show":                  "10.0.10.5 dev br-lan.1 lladdr aa:bb:cc:dd:ee:01 REACHABLE\n",
	})
	// newCollector leaves Netlink at its zero value, which is what a config
	// built without Load does too.
	list := c.Collect(context.Background())
	if len(list) != 2 {
		t.Fatalf("clients = %d, want 2", len(list))
	}
	if n := countCalls(runner.calls, "ubus call network.wireless status"); n != 1 {
		t.Errorf("ubus calls = %d, want 1", n)
	}
	if n := countCalls(runner.calls, "ip neigh show"); n != 1 {
		t.Errorf("ip neigh calls = %d, want 1", n)
	}
	if list[0].IP != "10.0.10.5" {
		t.Errorf("IP = %q, want 10.0.10.5", list[0].IP)
	}
	if list[0].Band != "5g" || list[0].SSID != "SSID-A_5G" {
		t.Errorf("context = %q/%q, want SSID-A_5G/5g", list[0].SSID, list[0].Band)
	}
}

// A BSS dump the kernel rejects must not blind the collector: netlink worked
// for stations, so the interface list falls back to netifd and the sample
// survives. One failed syscall per cycle would be worse than the process it
// replaced, so the fallback is permanent.
func TestBSSSourceFallsBackToNetifdAfterFailure(t *testing.T) {
	c, runner := newCollector(t, map[string]string{
		"status":                         wirelessStatusFixture,
		"iw dev phy0.1-ap0 station dump": stationDump,
	})
	bad := &failingBSSReader{}
	c.bssReader = bad

	list := c.Collect(context.Background())
	if len(list) != 2 {
		t.Fatalf("clients after fallback = %d, want 2", len(list))
	}
	if bad.calls != 1 {
		t.Errorf("failed source consulted %d times, want 1", bad.calls)
	}
	if _, ok := c.bssSource().(netifdBSSReader); !ok {
		t.Fatalf("source after failure = %T, want netifdBSSReader", c.bssSource())
	}
	if n := countCalls(runner.calls, "ubus call network.wireless status"); n != 1 {
		t.Errorf("ubus calls = %d, want 1", n)
	}
}

// A missing BSS list is "not reported", not "no clients": returning an empty
// list would tell the controller that every client left at once.
func TestBSSFailureYieldsNilNotAnEmptyList(t *testing.T) {
	c, _ := newCollector(t, nil)
	c.bssReader = &failingBSSReader{}
	c.Runner = &mapRunner{} // ubus answers with an empty string, which cannot be parsed
	if list := c.Collect(context.Background()); list != nil {
		t.Fatalf("clients = %+v, want nil when no BSS list can be read", list)
	}
}

// Addresses are a bonus. A failed neighbour read must leave the client rows
// intact with an empty IP, and must not repeat the attempt.
func TestNeighbourFailureIsSilentAndPermanent(t *testing.T) {
	c, _ := newCollector(t, map[string]string{
		"status":                         wirelessStatusFixture,
		"iw dev phy0.1-ap0 station dump": stationDump,
	})
	bad := &failingNeighbourReader{}
	c.neighRead = bad

	list := c.Collect(context.Background())
	if len(list) != 2 {
		t.Fatalf("clients = %d, want 2", len(list))
	}
	for _, cl := range list {
		if cl.IP != "" {
			t.Errorf("IP = %q, want empty", cl.IP)
		}
	}
	if bad.calls != 1 {
		t.Errorf("failed neighbour source consulted %d times, want 1", bad.calls)
	}
	if _, ok := c.neighbourSource().(ipNeighbourReader); !ok {
		t.Errorf("source after failure = %T, want ipNeighbourReader", c.neighbourSource())
	}
}

// Both readers must be resolvable on every platform: the agent only ships for
// OpenWrt, but the package's tests run on the developer's machine.
func TestSourceResolversNeverReturnNil(t *testing.T) {
	log := testLogger()
	for _, enabled := range []bool{true, false} {
		if resolveBSSReader(&mapRunner{}, enabled, log) == nil {
			t.Fatalf("resolveBSSReader(%v) = nil", enabled)
		}
		if resolveNeighbourReader(&mapRunner{}, enabled, log) == nil {
			t.Fatalf("resolveNeighbourReader(%v) = nil", enabled)
		}
	}
}
