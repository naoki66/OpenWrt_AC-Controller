package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Address preservation for network changes.
//
// The BSS health check answers "did the wireless come back". It cannot answer
// "is this box still reachable": a network change that drops the management
// address leaves every BSS happily beaconing while the AP becomes unreachable
// and only a serial console can fix it. So an apply that touches network also
// has to prove it did not lose an address that existed before.
//
// The rule is deliberately conservative — no address may disappear. Renumbering
// a device is therefore not something a push can do; that needs a deliberate
// out-of-band step, which is the right trade for a box you can only reach over
// the network you are about to change.

// localIPv4Addrs returns the IPv4 addresses currently configured, as CIDR.
func (a *Applier) localIPv4Addrs(ctx context.Context) []string {
	out, err := a.Runner.Run(ctx, "ip", "-o", "addr", "show")
	if err != nil {
		return nil
	}
	var addrs []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] == "inet" {
				addrs = append(addrs, fields[i+1])
			}
		}
	}
	sort.Strings(addrs)
	return addrs
}

// waitForAddrs blocks until every address in want is present again.
//
// It returns an error if one disappears for good, which makes the caller roll
// back. Loopback is skipped: it is always there and checking it only adds
// noise if the device uses a different loopback convention.
func (a *Applier) waitForAddrs(ctx context.Context, want []string) error {
	var expected []string
	for _, w := range want {
		if strings.HasPrefix(w, "127.") {
			continue
		}
		expected = append(expected, w)
	}
	if len(expected) == 0 {
		return nil
	}

	timeout := a.HealthTimeout
	if timeout == 0 {
		timeout = defaultHealthTimeout
	}
	poll := a.PollInterval
	if poll == 0 {
		poll = 500 * time.Millisecond
	}

	deadline := time.Now().Add(timeout)
	for {
		have := map[string]bool{}
		for _, h := range a.localIPv4Addrs(ctx) {
			have[h] = true
		}
		var missing []string
		for _, w := range expected {
			if !have[w] {
				missing = append(missing, w)
			}
		}
		if len(missing) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("address(es) lost after network reload: %s", strings.Join(missing, ", "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}
