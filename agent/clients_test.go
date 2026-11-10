package agent

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

type mapRunner struct {
	outputs map[string]string
	calls   []string
}

func (r *mapRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	key := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, key)
	if out, ok := r.outputs[key]; ok {
		return out, nil
	}
	if strings.HasPrefix(key, "ubus call network.wireless status") && r.outputs["status"] != "" {
		return r.outputs["status"], nil
	}
	return "", nil
}

// Captured from `iw dev phy0.1-ap0 station dump` on an MT7996 with two clients.
const stationDump = `Station aa:bb:cc:dd:ee:01 (on phy0.1-ap0)
	inactive time:	1200 ms
	rx bytes:	104857600
	rx packets:	71234
	tx bytes:	20971520
	tx packets:	30012
	tx retries:	120
	tx failed:	3
	signal:  	-47 [-52, -49] dBm
	signal avg:	-48 dBm
	tx bitrate:	1200.9 MBit/s AX HE-MCS 11
	rx bitrate:	1441.0 MBit/s AX HE-MCS 11
	connected time:	86400 seconds

Station aa:bb:cc:dd:ee:02 (on phy0.1-ap0)
	inactive time:	50 ms
	signal:  	-71 dBm
	signal avg:	-72 dBm
	tx bitrate:	286.7 MBit/s
	rx bitrate:	344.1 MBit/s
	connected time:	120 seconds
`

const wirelessStatusFixture = `{
  "radio0": {"up": true, "config": {"band": "2g"},
             "interfaces": [{"section": "wifinet0", "ifname": "phy0.0-ap0", "config": {"ssid": "SSID-A"}}]},
  "radio1": {"up": true, "config": {"band": "5g"},
             "interfaces": [{"section": "wifinet10", "ifname": "phy0.1-ap0", "config": {"ssid": "SSID-A_5G"}},
                            {"section": "wifinet11", "ifname": "phy0.1-ap1", "config": {"ssid": "SSID-B_5G"}}]}
}`

func newCollector(t *testing.T, outputs map[string]string) (*ClientCollector, *mapRunner) {
	t.Helper()
	r := &mapRunner{outputs: outputs}
	// 测试夹具提供的是 iw 输出，应避免探测宿主机真实的 nl80211 接口。
	return &ClientCollector{Runner: r, Source: "iw", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, r
}

func TestCollectClients(t *testing.T) {
	c, runner := newCollector(t, map[string]string{
		"status":                         wirelessStatusFixture,
		"iw dev phy0.1-ap0 station dump": stationDump,
		"iw dev phy0.0-ap0 station dump": "",
		"iw dev phy0.1-ap1 station dump": "",
		"ip neigh show": "10.0.10.5 dev br-lan.1 lladdr AA:BB:CC:DD:EE:01 REACHABLE\n" +
			"10.0.10.9 dev br-lan.1 lladdr aa:bb:cc:dd:ee:99 STALE\n",
	})

	clients := c.Collect(context.Background())
	if len(clients) != 2 {
		t.Fatalf("clients = %d, want 2 (%+v)", len(clients), clients)
	}

	first := clients[0]
	if first.MAC != "aa:bb:cc:dd:ee:01" {
		t.Errorf("mac = %q", first.MAC)
	}
	if first.Signal != -47 {
		t.Errorf("signal = %d, want -47 (first value, not the average bracket)", first.Signal)
	}
	if first.RxRateMbps != 1441 || first.TxRateMbps != 1201 {
		t.Errorf("rates = %d/%d, want 1441/1201 (rounded up)", first.RxRateMbps, first.TxRateMbps)
	}
	if first.ConnectedSeconds != 86400 {
		t.Errorf("connected = %d", first.ConnectedSeconds)
	}
	if first.InactiveSeconds != 1 {
		t.Errorf("inactive = %d, want 1 (1200 ms truncated)", first.InactiveSeconds)
	}
	if first.SSID != "SSID-A_5G" || first.Band != "5g" || first.Ifname != "phy0.1-ap0" {
		t.Errorf("association context = %+v", first)
	}
	// MAC is matched case-insensitively against the neighbour table.
	if first.IP != "10.0.10.5" {
		t.Errorf("ip = %q, want 10.0.10.5", first.IP)
	}

	if clients[1].IP != "" {
		t.Errorf("second client should have no IP, got %q", clients[1].IP)
	}
	if clients[1].Signal != -71 {
		t.Errorf("second signal = %d", clients[1].Signal)
	}

	// Every managed BSS must be asked, not just the first.
	if len(runner.calls) < 4 {
		t.Errorf("expected a dump per interface, got %v", runner.calls)
	}
}

// An AP with no clients must report an empty list, not nil: the controller
// needs to distinguish "no clients" from "this agent cannot tell".
func TestCollectEmptyListIsNotEmptyNil(t *testing.T) {
	c, _ := newCollector(t, map[string]string{
		"status":                         wirelessStatusFixture,
		"iw dev phy0.0-ap0 station dump": "",
		"iw dev phy0.1-ap0 station dump": "",
		"iw dev phy0.1-ap1 station dump": "",
	})

	clients := c.Collect(context.Background())
	if clients == nil {
		t.Fatal("expected an empty slice, got nil")
	}
	if len(clients) != 0 {
		t.Fatalf("expected no clients, got %+v", clients)
	}
}

// When the wireless state itself is unreadable the collector must return nil so
// the controller keeps its previous view instead of wiping the table.
func TestCollectReturnsNilWhenStatusUnavailable(t *testing.T) {
	c, _ := newCollector(t, nil)
	if got := c.Collect(context.Background()); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestParseNeighbours(t *testing.T) {
	out := `10.0.10.5 dev br-lan.1 lladdr aa:bb:cc:dd:ee:01 REACHABLE
10.0.20.7 dev br-lan.2 lladdr aa:bb:cc:dd:ee:02 STALE
fe80::1 dev br-lan.1 lladdr aa:bb:cc:dd:ee:03 router REACHABLE
10.0.30.9 dev br-lan.3  FAILED
`
	got := parseNeighbours(out)
	if len(got) != 2 {
		t.Fatalf("entries = %v", got)
	}
	if got["aa:bb:cc:dd:ee:01"] != "10.0.10.5" {
		t.Errorf("ipv4 lookup failed: %v", got)
	}
	if _, ok := got["aa:bb:cc:dd:ee:03"]; ok {
		t.Error("IPv6 neighbour should not be mapped")
	}
}
