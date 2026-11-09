package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// The network health check exists because a BSS can be up while the management
// address is gone — which passes the wireless check and locks the operator out.
func TestWaitForAddrsPassesWhenAddressesSurvive(t *testing.T) {
	runner := &fakeRunner{replies: map[string]string{
		"ip -o addr show": `1: lo    inet 127.0.0.1/8 scope host lo
2: br-lan.1    inet 10.0.10.251/24 brd 10.0.10.255 scope global lan
3: br-lan.2    inet 10.0.20.251/24 brd 10.0.20.255 scope global lan2
`,
	}}
	a := &Applier{
		Runner:        runner,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		HealthTimeout: 200 * time.Millisecond,
		PollInterval:  10 * time.Millisecond,
	}

	if err := a.waitForAddrs(context.Background(), []string{"10.0.10.251/24", "10.0.20.251/24"}); err != nil {
		t.Fatalf("addresses present but check failed: %v", err)
	}
}

func TestWaitForAddrsFailsWhenAnAddressIsLost(t *testing.T) {
	runner := &fakeRunner{replies: map[string]string{
		"ip -o addr show": `2: br-lan.2    inet 10.0.20.251/24 brd 10.0.20.255 scope global lan2
`,
	}}
	a := &Applier{
		Runner:        runner,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		HealthTimeout: 50 * time.Millisecond,
		PollInterval:  10 * time.Millisecond,
	}

	err := a.waitForAddrs(context.Background(), []string{"10.0.10.251/24", "10.0.20.251/24"})
	if err == nil {
		t.Fatal("lost address not reported")
	}
	if !strings.Contains(err.Error(), "10.0.10.251/24") {
		t.Errorf("error does not name the lost address: %v", err)
	}
}

// Loopback is always there; checking it only risks a false failure if the device
// represents it differently.
func TestWaitForAddrsIgnoresLoopback(t *testing.T) {
	runner := &fakeRunner{replies: map[string]string{"ip -o addr show": ""}}
	a := &Applier{
		Runner:        runner,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		HealthTimeout: 20 * time.Millisecond,
		PollInterval:  5 * time.Millisecond,
	}

	if err := a.waitForAddrs(context.Background(), []string{"127.0.0.1/8"}); err != nil {
		t.Fatalf("loopback caused a failure: %v", err)
	}
}

// A factory-reset AP has to move from its factory addresses to the managed ones.
// When the merged config no longer declares the old addresses and a new static
// address can reach the controller, the apply must proceed instead of rolling
// back forever.
func TestWaitForAddrsAfterNetworkAllowsReachableRenumber(t *testing.T) {
	runner := &fakeRunner{replies: map[string]string{
		"ip -o addr show": `2: br-lan.1    inet 10.10.10.250/24 brd 10.10.10.255 scope global lan
3: br-lan.2    inet 10.10.20.250/24 brd 10.10.20.255 scope global lan2
`,
	}}
	var dialed string
	a := &Applier{
		Runner:        runner,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		HealthTimeout: 100 * time.Millisecond,
		PollInterval:  5 * time.Millisecond,
		ACAddr:        "10.10.30.5:8090",
		DialAC: func(_ context.Context, src net.IP, target string) error {
			if target != "10.10.30.5:8090" {
				t.Errorf("dial target = %q", target)
			}
			dialed = src.String()
			return nil
		},
	}
	merged := []byte("config interface 'lan'\n\toption proto 'static'\n\toption ipaddr '10.10.10.250/24'\n")

	newAddr, err := a.waitForAddrsAfterNetwork(context.Background(),
		[]string{"10.10.10.144/24", "192.168.50.1/24"}, merged)
	if err != nil {
		t.Fatalf("reachable renumbering rejected: %v", err)
	}
	if newAddr != "10.10.10.250" {
		t.Errorf("new address = %q, want 10.10.10.250", newAddr)
	}
	if dialed != "10.10.10.250" {
		t.Errorf("dialed from %q, want 10.10.10.250", dialed)
	}
}

func TestWaitForAddrsAfterNetworkRejectsUnreachableRenumber(t *testing.T) {
	runner := &fakeRunner{replies: map[string]string{
		"ip -o addr show": "2: br-lan.1    inet 10.10.10.250/24 brd 10.10.10.255 scope global lan\n",
	}}
	a := &Applier{
		Runner:        runner,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		HealthTimeout: 30 * time.Millisecond,
		PollInterval:  5 * time.Millisecond,
		ACAddr:        "10.10.30.5:8090",
		DialAC: func(context.Context, net.IP, string) error {
			return errors.New("no route to host")
		},
	}
	merged := []byte("config interface 'lan'\n\toption proto 'static'\n\toption ipaddr '10.10.10.250/24'\n")

	_, err := a.waitForAddrsAfterNetwork(context.Background(),
		[]string{"10.10.10.144/24"}, merged)
	if err == nil {
		t.Fatal("unreachable renumbering accepted")
	}
	if !strings.Contains(err.Error(), "10.10.10.144/24") {
		t.Errorf("error does not name the lost address: %v", err)
	}
}

// If the merged config still asks for an address, losing it is a bug in the
// merge, not a renumbering: a reachable new address must not mask it.
func TestWaitForAddrsAfterNetworkRejectsLostDeclaredAddress(t *testing.T) {
	runner := &fakeRunner{replies: map[string]string{
		"ip -o addr show": "2: br-lan.1    inet 10.10.10.250/24 brd 10.10.10.255 scope global lan\n",
	}}
	called := false
	a := &Applier{
		Runner:        runner,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		HealthTimeout: 30 * time.Millisecond,
		PollInterval:  5 * time.Millisecond,
		ACAddr:        "10.10.30.5:8090",
		DialAC: func(context.Context, net.IP, string) error {
			called = true
			return nil
		},
	}
	merged := []byte("config interface 'lan'\n\toption ipaddr '10.10.10.250/24'\n" +
		"config interface 'wan'\n\toption ipaddr '10.10.10.144/24'\n")

	_, err := a.waitForAddrsAfterNetwork(context.Background(),
		[]string{"10.10.10.144/24"}, merged)
	if err == nil {
		t.Fatal("lost address that the config still declares was accepted")
	}
	if called {
		t.Error("renumbering escape was used for an address the config still declares")
	}
}

func TestDeclaredStaticIPv4s(t *testing.T) {
	merged := []byte(`config interface 'lan'
	option proto 'static'
	option ipaddr '10.10.10.250/24'
config interface 'lan2'
	list ipaddr '10.10.20.250/24'
	list ipaddr 'fd00::1/64'
config interface 'wan'
	option proto 'dhcp'
`)
	got := declaredStaticIPv4s(merged)
	if !got["10.10.10.250"] || !got["10.10.20.250"] {
		t.Errorf("static addresses missing from %v", got)
	}
	if len(got) != 2 {
		t.Errorf("unexpected addresses in %v", got)
	}
}

func TestLocalIPv4AddrsParsesOutput(t *testing.T) {
	runner := &fakeRunner{replies: map[string]string{
		"ip -o addr show": `1: lo    inet 127.0.0.1/8 scope host lo
2: br-lan.1    inet 10.0.10.251/24 brd 10.0.10.255 scope global lan
2: br-lan.1    inet6 fe80::1/64 scope link
`,
	}}
	a := &Applier{Runner: runner, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	got := a.localIPv4Addrs(context.Background())
	want := []string{"10.0.10.251/24", "127.0.0.1/8"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// hostapd must not be started before the bridge VLAN devices exist: it would
// attach BSSes to nothing, they come up `down`, and never recover. That is how
// a real layer-2 takeover failed on an XR1710G.
func TestWaitForNetdevsFailsWhenDeviceMissing(t *testing.T) {
	runner := &fakeRunner{failOn: map[string]error{
		"ip link show br-lan.3": errors.New("Device does not exist"),
	}}
	a := &Applier{
		Runner:        runner,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		HealthTimeout: 50 * time.Millisecond,
		PollInterval:  10 * time.Millisecond,
	}

	err := a.waitForNetdevs(context.Background(), []string{"br-lan.1", "br-lan.3"})
	if err == nil {
		t.Fatal("missing netdev not reported")
	}
	if !strings.Contains(err.Error(), "br-lan.3") {
		t.Errorf("error does not name the missing device: %v", err)
	}
	if strings.Contains(err.Error(), "br-lan.1") {
		t.Errorf("error names a device that exists: %v", err)
	}
}

func TestWaitForNetdevsPassesWhenAllPresent(t *testing.T) {
	a := &Applier{
		Runner:        &fakeRunner{replies: map[string]string{}},
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		HealthTimeout: 100 * time.Millisecond,
		PollInterval:  10 * time.Millisecond,
	}
	if err := a.waitForNetdevs(context.Background(), []string{"br-lan.1", "br-lan.2"}); err != nil {
		t.Fatalf("all devices present but check failed: %v", err)
	}
}

// A failed apply must not be retried every cycle: each retry rewrites network
// and reloads it, which is worse than the bad revision.
func TestApplyBackoffGrowsAndCaps(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{1, time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{6, 32 * time.Minute},
		{10, time.Hour},
		{50, time.Hour},
	}
	for _, c := range cases {
		if got := applyBackoff(c.failures); got != c.want {
			t.Errorf("applyBackoff(%d) = %v, want %v", c.failures, got, c.want)
		}
	}
}
