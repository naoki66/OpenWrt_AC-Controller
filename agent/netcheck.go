package agent

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/naoki66/ac-controller/shared/ucimerge"
)

// Address preservation for network changes.
//
// The BSS health check answers "did the wireless come back". It cannot answer
// "is this box still reachable": a network change that drops the management
// address leaves every BSS happily beaconing while the AP becomes unreachable
// and only a serial console can fix it. So an apply that touches network also
// has to prove it did not lose an address that existed before.
//
// The default rule is deliberately conservative — no address may disappear. A
// factory-reset AP cannot satisfy it: its addresses belong to the factory
// routing-mode config, and the takeover removes them by design, so every push
// rolled back and the AP stayed on its default SSIDs.
//
// A renumbering therefore passes only under two conditions: the lost addresses
// are no longer declared anywhere in the merged config (they were deliberately
// removed, not accidentally dropped), and one of the new static addresses is
// present and can reach the controller. That keeps the lockout guarantee while
// letting an operator move a device between management addresses with a push.

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
	_, err := a.verifyAddrs(ctx, want, nil)
	return err
}

// waitForAddrsAfterNetwork is waitForAddrs for a network apply. Unlike the plain
// check it accepts a deliberate renumbering: an address that disappeared is
// tolerated when the merged config no longer declares it and a new static
// address from that config is up and can reach the controller. The returned
// address is the one that replaced the lost management address, empty when
// nothing moved.
func (a *Applier) waitForAddrsAfterNetwork(ctx context.Context, want []string, mergedNetwork []byte) (string, error) {
	return a.verifyAddrs(ctx, want, mergedNetwork)
}

func (a *Applier) verifyAddrs(ctx context.Context, want []string, mergedNetwork []byte) (string, error) {
	var expected []string
	for _, w := range want {
		if strings.HasPrefix(w, "127.") {
			continue
		}
		expected = append(expected, w)
	}
	if len(expected) == 0 {
		return "", nil
	}

	// Addresses the merged config still asks for. If one of those is missing the
	// package meant to keep it, so a new reachable address must not mask that.
	declared := map[string]bool{}
	if len(mergedNetwork) > 0 {
		declared = declaredStaticIPv4s(mergedNetwork)
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
		haveCIDR := a.localIPv4Addrs(ctx)
		have := map[string]bool{}
		haveIP := map[string]bool{}
		for _, h := range haveCIDR {
			have[h] = true
			haveIP[bareIPv4(h)] = true
		}

		var missing []string
		for _, w := range expected {
			if !have[w] {
				missing = append(missing, w)
			}
		}
		if len(missing) == 0 {
			return "", nil
		}

		if len(declared) > 0 && !anyMissingStillDeclared(missing, declared) {
			if ip := a.newAddrReachingController(ctx, expected, declared, haveIP); ip != "" {
				if a.Log != nil {
					a.Log.Warn("management address moved; the new address reached the controller",
						"lost", strings.Join(missing, ", "), "new", ip)
				}
				return ip, nil
			}
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("address(es) lost after network reload: %s", strings.Join(missing, ", "))
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(poll):
		}
	}
}

// anyMissingStillDeclared reports whether the merged config still asks for one of
// the lost addresses. Such an address was not intentionally removed.
func anyMissingStillDeclared(missing []string, declared map[string]bool) bool {
	for _, m := range missing {
		if declared[bareIPv4(m)] {
			return true
		}
	}
	return false
}

// newAddrReachingController returns a static address from the merged config that
// is up now, was not there before, and can reach the controller. Empty means the
// renumbering is not proven safe.
func (a *Applier) newAddrReachingController(ctx context.Context, before []string, declared, haveIP map[string]bool) string {
	if a.ACAddr == "" {
		return ""
	}
	old := map[string]bool{}
	for _, b := range before {
		old[bareIPv4(b)] = true
	}
	for ip := range declared {
		if old[ip] || !haveIP[ip] {
			continue
		}
		if err := a.dialAC(ctx, net.ParseIP(ip)); err == nil {
			return ip
		}
	}
	return ""
}

func (a *Applier) dialAC(ctx context.Context, src net.IP) error {
	if a.DialAC != nil {
		return a.DialAC(ctx, src, a.ACAddr)
	}
	target := a.ACAddr
	if i := strings.Index(target, "://"); i >= 0 {
		target = target[i+3:]
	}
	if i := strings.IndexByte(target, '/'); i >= 0 {
		target = target[:i]
	}
	d := net.Dialer{
		Timeout:   2 * time.Second,
		LocalAddr: &net.TCPAddr{IP: src},
	}
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return err
	}
	return conn.Close()
}

// declaredStaticIPv4s collects the static IPv4 addresses a UCI network file
// declares on its interface sections, without their prefix length.
func declaredStaticIPv4s(network []byte) map[string]bool {
	out := map[string]bool{}
	for _, s := range ucimerge.Parse(string(network)).Sections {
		if s.Type != "interface" {
			continue
		}
		for _, line := range s.Lines {
			key, val, ok := uciOption(line)
			if !ok || key != "ipaddr" {
				continue
			}
			ip := bareIPv4(val)
			if net.ParseIP(ip) != nil && !strings.Contains(ip, ":") {
				out[ip] = true
			}
		}
	}
	return out
}

// uciOption parses `option key 'value'` and `list key 'value'` lines.
func uciOption(line string) (key, val string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 || (fields[0] != "option" && fields[0] != "list") {
		return "", "", false
	}
	val = strings.Join(fields[2:], " ")
	val = strings.Trim(val, `'"`)
	return fields[1], val, true
}

func bareIPv4(addr string) string {
	if i := strings.IndexByte(addr, '/'); i >= 0 {
		return addr[:i]
	}
	return addr
}
