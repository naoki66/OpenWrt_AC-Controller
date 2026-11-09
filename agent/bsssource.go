package agent

import (
	"context"
	"encoding/json"
	"log/slog"
)

// BSS list selection.
//
// The client collector needs three facts about every BSS before it can read a
// station table: the interface name, the SSID and the band. It used to get them
// from `ubus call network.wireless status`, which measured 6.25 ms on the
// reference AP — a ubus round trip plus a process spawn, on every sample, to
// learn a list that changes only when the operator pushes a new template.
//
// NL80211_CMD_GET_INTERFACE carries the same three facts (name, SSID, centre
// frequency) plus the ifindex the station dump wants anyway, for one syscall
// pair. The ubus path stays as the fallback and as the reference the netlink
// answer is compared against: band and SSID must agree with netifd, and a
// disagreement means the attributes were mis-decoded.

// bssInfo is one AP-mode BSS the collector samples.
type bssInfo struct {
	Ifname string
	// Ifindex is 0 when the source does not report one; the station reader
	// then resolves it by name.
	Ifindex int
	SSID    string
	Band    string
}

// BSSReader lists the BSSes to sample.
type BSSReader interface {
	BSS(ctx context.Context) ([]bssInfo, error)
	Name() string
}

// netifdBSSReader is the original path: one ubus call, decoded from netifd.
type netifdBSSReader struct{ runner Runner }

func (r netifdBSSReader) Name() string { return "netifd" }

func (r netifdBSSReader) BSS(ctx context.Context) ([]bssInfo, error) {
	out, err := r.runner.Run(ctx, "ubus", "call", "network.wireless", "status")
	if err != nil {
		return nil, err
	}
	var status wirelessStatus
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		return nil, err
	}
	// Non-nil even when empty: the collector uses nil to mean "the list could
	// not be read", and an AP with no BSS is a different answer.
	list := make([]bssInfo, 0, 8)
	for _, radio := range status {
		band := string(radio.Config.Band)
		for _, iface := range radio.Interfaces {
			if iface.Ifname == "" {
				continue
			}
			list = append(list, bssInfo{Ifname: iface.Ifname, SSID: iface.Config.SSID, Band: band})
		}
	}
	return list, nil
}

// resolveBSSReader builds the reader. A platform without netlink degrades to
// netifd rather than returning nil: no BSS list means no client telemetry at
// all, which is far worse than the process it saves.
func resolveBSSReader(runner Runner, netlink bool, log *slog.Logger) BSSReader {
	fallback := netifdBSSReader{runner: runner}
	if !netlink {
		return fallback
	}
	r := newNetlinkBSSReader()
	if r == nil {
		if log != nil {
			log.Debug("bss list: netlink unavailable on this platform, using netifd")
		}
		return fallback
	}
	return r
}
