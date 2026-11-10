package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/naoki66/ac-controller/shared/proto"
)

// RadioCollector reads each radio's operating point from the AP.
//
// Everything here is measured, not configured. The distinction matters most for
// the channel: a radio left on `channel 'auto'` reports "auto" in UCI forever
// while ACS moves it around, so a controller reading /etc/config would draw a
// flat line and learn nothing. hostapd's own status is the only place the real
// answer exists.
//
// Airtime utilisation comes from the same call, and is the only interference
// signal available without putting the controller on the data path (design C1).
type RadioCollector struct {
	Runner Runner
	Log    *slog.Logger
	// Ubus, when set, answers the two status calls over the ubusd socket instead
	// of forking the ubus binary. Nil means every call goes through Runner,
	// which is the previous behaviour and the fallback.
	Ubus *ubusClient
	// warnedHostapd keeps a missing hostapd ubus object from logging every cycle.
	warnedHostapd bool
}

// call returns one ubus method's JSON reply, preferring the socket.
//
// JSON either way is what makes the swap safe here: these two calls report every
// radio field the controller stores, and the parsers below stay exactly as they
// were, so no value can drift while the transport changes. See ubusReply.
func (c *RadioCollector) call(ctx context.Context, obj, method string) ([]byte, error) {
	return ubusReply(ctx, c.Ubus, c.Runner, c.Log, obj, method)
}

// hostapdStatus mirrors the parts of `ubus call hostapd.<if> get_status` this
// collector uses. Verified on ImmortalWrt 20261026 / MT7996:
//
//	{"status":"ENABLED","freq":2427,"channel":4,"op_class":81,
//	 "phy":"phy0.0","airtime":{"utilization":45}}
//
// Airtime is a pointer because "no airtime object" (older hostapd) must be
// distinguishable from "0% busy": the sample then carries -1 for unknown.
type hostapdStatus struct {
	Channel int    `json:"channel"`
	Freq    int    `json:"freq"`
	Phy     string `json:"phy"`
	Airtime *struct {
		Utilization int `json:"utilization"`
	} `json:"airtime"`
}

// Collect returns one entry per radio, sorted by section name so that two
// consecutive samples compare cleanly. Radios that are down still appear: the
// controller needs to see that a radio exists even when nothing is on it.
//
// clients, when non-nil, is used to attribute stations to the radio they are
// associated with; nil leaves the per-radio count at zero rather than making a
// second pass over every BSS.
func (c *RadioCollector) Collect(ctx context.Context, clients []proto.ClientStat) []proto.RadioStat {
	out, err := c.call(ctx, "network.wireless", "status")
	if err != nil {
		c.Log.Debug("radio collect: wireless status unavailable", "err", err)
		return nil
	}

	var status wirelessStatus
	if err := json.Unmarshal(out, &status); err != nil {
		c.Log.Debug("radio collect: unparseable wireless status", "err", err)
		return nil
	}

	perIface := map[string]int{}
	for _, cl := range clients {
		if cl.Ifname != "" {
			perIface[cl.Ifname]++
		}
	}

	names := make([]string, 0, len(status))
	for name := range status {
		names = append(names, name)
	}
	sort.Strings(names)

	stats := make([]proto.RadioStat, 0, len(names))
	for _, name := range names {
		radio := status[name]
		st := proto.RadioStat{
			Radio:    name,
			Band:     string(radio.Config.Band),
			Up:       radio.Up && !radio.Disabled,
			HTMode:   string(radio.Config.HTMode),
			WidthMHz: htmodeWidth(string(radio.Config.HTMode)),
			Country:  string(radio.Config.Country),
			TxPower:  int(radio.Config.TxPower),
			// A radio with no BSS has no hostapd object to ask; leave the
			// channel at 0 and the utilisation at -1 rather than guessing.
			Utilization: -1,
		}

		// A configured numeric channel is still worth reporting when hostapd
		// cannot be reached: it is what will be used once the radio comes up.
		if n, err := strconv.Atoi(string(radio.Config.Channel)); err == nil {
			st.Channel = n
		}

		for _, iface := range radio.Interfaces {
			if iface.Ifname == "" {
				continue
			}
			st.Clients += perIface[iface.Ifname]
		}

		if st.Up {
			if ifname := firstIfname(radio); ifname != "" {
				if live, ok := c.hostapd(ctx, ifname); ok {
					if live.Channel > 0 {
						st.Channel = live.Channel
					}
					st.FreqMHz = live.Freq
					st.Phy = live.Phy
					if live.Airtime != nil {
						st.Utilization = live.Airtime.Utilization
					}
				}
			}
		}
		stats = append(stats, st)
	}
	return stats
}

// hostapd reads the live operating point of one BSS. Every BSS on a radio shares
// the radio's channel, so asking the first one is enough.
func (c *RadioCollector) hostapd(ctx context.Context, ifname string) (hostapdStatus, bool) {
	out, err := c.call(ctx, "hostapd."+ifname, "get_status")
	if err != nil {
		if !c.warnedHostapd {
			c.Log.Debug("radio collect: hostapd status unavailable", "ifname", ifname, "err", err)
			c.warnedHostapd = true
		}
		return hostapdStatus{}, false
	}
	var st hostapdStatus
	if err := json.Unmarshal(out, &st); err != nil {
		c.Log.Debug("radio collect: unparseable hostapd status", "ifname", ifname, "err", err)
		return hostapdStatus{}, false
	}
	return st, true
}

// firstIfname picks one BSS to ask about the radio. Every BSS on a radio shares
// its channel, so the first is as good as any.
func firstIfname(radio radioStatus) string {
	for _, iface := range radio.Interfaces {
		if iface.Ifname != "" && (iface.Config.Mode == "" || iface.Config.Mode == "ap") {
			return iface.Ifname
		}
	}
	return ""
}

// htmodeWidth turns a UCI htmode into a channel width in MHz: EHT160 -> 160,
// HE20 -> 20. Returns 0 when the string carries no width, which the UI shows as
// "-" rather than plotting a nonsense value.
func htmodeWidth(htmode string) int {
	if htmode == "" {
		return 0
	}
	end := len(htmode)
	for end > 0 && htmode[end-1] >= '0' && htmode[end-1] <= '9' {
		end--
	}
	if end == len(htmode) {
		return 0
	}
	n, err := strconv.Atoi(htmode[end:])
	if err != nil {
		return 0
	}
	return n
}

// flexString and flexInt accept a UCI option that netifd dumps as either a JSON
// string or a JSON number. `option channel '36'` arrives as "36" on one build
// and as 36 on another; failing to parse either shape would silently drop the
// channel from every sample.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	*f = flexString(strings.Trim(string(b), `"`))
	return nil
}

type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	raw := strings.Trim(strings.Trim(string(b), `"`), " ")
	if raw == "" {
		*f = 0
		return nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		// A non-numeric value is not an error worth failing a sample over.
		*f = 0
		return nil
	}
	*f = flexInt(n)
	return nil
}
