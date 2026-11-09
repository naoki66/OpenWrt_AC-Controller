// Package render turns a template into the UCI files an access point applies.
//
// Output is byte-for-byte deterministic for a given template: sections are
// sorted, no timestamps are emitted, and option order is fixed. P1's acceptance
// criterion is "rendered config matches the deployed baseline", which is only
// checkable if the renderer never produces incidental churn.
//
// The network model was corrected against a real XR1710G: VLAN separation is
// DSA bridge-vlan (`vlan_filtering` on the bridge plus a PVID per port), not
// software 802.1q sub-interfaces.
package render

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/naoki66/ac-controller/shared/model"
)

// File names inside a configuration package.
const (
	FileWireless = "wireless"
	FileNetwork  = "network"
	// FileSteering is a one-line file naming the steering daemon the AP should
	// run. It is not UCI: the AP only starts/stops an init script, it does not
	// merge configuration.
	FileSteering = "steering"
	// FileNeighbors carries the per-BSS 802.11k neighbour lists. Also not UCI:
	// hostapd holds them in memory only, so the AP re-applies them with
	// rrm_nr_set on every revision and after any reload.
	FileNeighbors = "neighbors"
	// FileSteerPolicy is a JSON document with the parameters of the agent's
	// local steering loop (thresholds, force-disassociation, cooldowns). Absent
	// means the loop stays off — the whole feature is opt-in per template.
	FileSteerPolicy = "steer-policy"
)

// ACPrefix marks every section the controller owns. The AP-side merge replaces
// exactly these and leaves locally authored sections alone.
const ACPrefix = "ac_"

// Options controls how section names are emitted.
type Options struct {
	// Prefix is prepended to the section names the controller invents and that
	// nothing outside the controller refers to: BSSes, bridge VLANs and the
	// bridge device. Production uses "ac_" so the AP-side merge can recognise
	// what it owns.
	//
	// Two kinds of name deliberately never take the prefix:
	//
	//   - `config interface` in the network file, because firewall zones and
	//     DHCP config reference those names (see Network).
	//   - `config wifi-device`, because the name radio0..radio2 is the
	//     platform's own: a wifi-iface's `option device`, LuCI's radio page and
	//     the vendor documentation all use it, and it is what a freshly flashed
	//     AP already contains. Ownership survives without the prefix because
	//     the merge replaces any section whose type and name the rendered set
	//     claims.
	//
	// The parity check against the deployed baseline uses "" because that file
	// was written by hand before the controller existed; comparing with a prefix
	// would report a name difference on every BSS and hide real ones.
	Prefix string
}

// bandOrder fixes the mapping between a band and its radio index. On the
// XR1710G radio0..radio2 are 2.4 / 5 / 6 GHz in DTS order, and the golden
// baseline uses the same numbering.
var bandOrder = map[string]int{
	model.Band2G: 0,
	model.Band5G: 1,
	model.Band6G: 2,
}

// radioName is the UCI section name of a wifi-device. It keeps the platform's
// own spelling (radio0..radio2) so that every reference outside the controller
// — a wifi-iface's `option device`, LuCI's radio page, the vendor's
// documentation, an operator's notes — keeps pointing at the same radio.
// See Options.Prefix for why this is the one name that is not prefixed.
func radioName(index int) string { return "radio" + strconv.Itoa(index) }

// Wireless renders the wifi-device and wifi-iface sections.
func Wireless(t model.Template, opts Options) ([]byte, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}

	radios := append([]model.Radio(nil), t.Radios...)
	sort.SliceStable(radios, func(i, j int) bool { return radios[i].Index < radios[j].Index })

	var b strings.Builder
	for _, r := range radios {
		if r.Disabled {
			continue
		}
		section(&b, "wifi-device", radioName(r.Index))
		option(&b, "type", "mac80211")
		if r.Path != "" {
			option(&b, "path", r.Path)
		}
		// MT7996 puts all three phys behind one PCIe function; without the index
		// netifd cannot tell them apart and binds every section to radio0.
		option(&b, "radio", strconv.Itoa(r.Index))
		option(&b, "band", r.Band)
		channel := r.Channel
		if channel == "" {
			channel = "auto"
		}
		option(&b, "channel", channel)
		if r.HTMode != "" {
			option(&b, "htmode", r.HTMode)
		}
		if r.Country != "" {
			option(&b, "country", r.Country)
		}
		if r.CellDensity != nil {
			option(&b, "cell_density", strconv.Itoa(*r.CellDensity))
		}
		boolOption(&b, "he_su_beamformer", r.HESUBeamformer)
		boolOption(&b, "he_mu_beamformer", r.HEMUBeamformer)
		boolOption(&b, "eht_su_beamformer", r.EHTSUBeamformer)
		boolOption(&b, "eht_mu_beamformer", r.EHTMUBeamformer)
		if r.BSSColor != nil {
			option(&b, "he_bss_color", strconv.Itoa(*r.BSSColor))
		}
		boolOption(&b, "radar_background", r.RadarBackground)
		boolOption(&b, "he_twt_responder", r.TWTResponder)
		option(&b, "txpower", strconv.Itoa(r.TxPower))
		option(&b, "disabled", "0")

		keys := make([]string, 0, len(r.Extra))
		for k := range r.Extra {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			option(&b, k, r.Extra[k])
		}
	}

	radioIndex := map[string]int{}
	for _, r := range t.Radios {
		if !r.Disabled {
			radioIndex[r.Band] = r.Index
		}
	}

	for _, iface := range buildInterfaces(t) {
		section(&b, "wifi-iface", opts.Prefix+iface.name)
		option(&b, "device", radioName(radioIndex[iface.band]))
		option(&b, "mode", "ap")
		option(&b, "ssid", iface.src.SSID)
		if iface.src.Hidden {
			option(&b, "hidden", "1")
		}
		if iface.src.MaxSTA > 0 {
			option(&b, "maxassoc", strconv.Itoa(iface.src.MaxSTA))
		}
		if mode := strings.ToLower(iface.src.MACFilter); mode == model.MACFilterAllow || mode == model.MACFilterDeny {
			option(&b, "macfilter", mode)
			for _, mac := range iface.src.MACList {
				list(&b, "maclist", strings.ToLower(strings.TrimSpace(mac)))
			}
		}
		option(&b, "encryption", Encryption(iface.src.Security))
		if iface.src.RequiresPSK() {
			option(&b, "key", iface.src.PSK)
		}
		// When the template defines netifd interfaces the controller owns the
		// layer-2 fabric and this references one of them; when it does not, the
		// reference is a local one and must stay verbatim.
		//
		// It is deliberately NOT prefixed: see Network for why the interface
		// names in the network file keep their device-local spelling.
		network := iface.src.Network
		option(&b, "network", network)

		roam := iface.src.Roaming
		if roam.K {
			option(&b, "ieee80211k", "1")
		}
		if roam.V {
			option(&b, "bss_transition", "1")
		}
		boolOption(&b, "ieee80211v", iface.src.IEEE80211V)
		// Explicit RRM keys. Redundant when ieee80211k is set (hostapd then
		// defaults both to 1), but LuCI models them as separate switches too, so
		// an operator can enable the reports without the umbrella flag.
		boolOption(&b, "rrm_neighbor_report", iface.src.RRM)
		boolOption(&b, "rrm_beacon_report", iface.src.RRM)
		if iface.src.WNMSleepMode {
			option(&b, "wnm_sleep_mode", "1")
		}
		if iface.src.WNMSleepModeNoKeys {
			option(&b, "wnm_sleep_mode_no_keys", "1")
		}
		if iface.src.ProxyARP {
			option(&b, "proxy_arp", "1")
		}
		if iface.src.Isolate {
			option(&b, "isolate", "1")
		}
		if roam.R {
			option(&b, "ieee80211r", "1")
			if roam.FTOverDS != nil {
				option(&b, "ft_over_ds", boolstr(*roam.FTOverDS))
			}
			if iface.src.FTPSKGenerateLocal != nil {
				option(&b, "ft_psk_generate_local", boolstr(*iface.src.FTPSKGenerateLocal))
			}
			option(&b, "mobility_domain", roam.MobilityDomain)
		}
		boolOption(&b, "disassoc_low_ack", iface.src.DisassocLowAck)
		if iface.src.Disabled {
			option(&b, "disabled", "1")
		}
	}

	return []byte(b.String()), nil
}

// Network renders the layer-2 fabric: the bridge device, one bridge-vlan per
// VLAN, and the netifd interfaces on top of them.
//
// The bridge device section is emitted only when the template declares bridge
// ports. On a live AP the bridge already exists and is not the controller's to
// rewrite; during the parity check against the baseline it is part of the file.
func Network(t model.Template, opts Options) ([]byte, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}

	var b strings.Builder
	br := t.Bridge

	vlans := append([]model.BridgeVLAN(nil), br.VLANs...)
	sort.SliceStable(vlans, func(i, j int) bool { return vlans[i].VLAN < vlans[j].VLAN })

	if br.Name != "" && len(br.Ports) > 0 {
		name := ""
		if opts.Prefix != "" {
			name = opts.Prefix + "bridge"
		}
		section(&b, "device", name)
		option(&b, "name", br.Name)
		option(&b, "type", "bridge")
		for _, p := range br.Ports {
			list(&b, "ports", p)
		}
		// bridge-vlan sections are inert unless the bridge itself filters
		// VLANs. The editor sets this field when VLANs are enabled, so the
		// package turns on br-lan's VLAN filtering instead of leaving the
		// operator to set it by hand on every AP. Nil keeps the option out of
		// the file (and out of the pinned baseline); a template that declares
		// no VLANs never emits it, because filtering a bridge with no VLANs
		// would cut every port.
		if br.VLANFiltering != nil && len(vlans) > 0 {
			option(&b, "vlan_filtering", boolstr(*br.VLANFiltering))
		}
		if br.IGMPSnooping != nil {
			option(&b, "igmp_snooping", boolstr(*br.IGMPSnooping))
		}
	}

	for _, v := range vlans {
		// The deployed baseline writes these sections anonymously. A named
		// section is functionally identical, but the AP-side merge can only
		// replace what it can identify, so production names them.
		// UCI section names are unique per file regardless of type, so this
		// cannot be "ac_vlan10": that name is already taken by the netifd
		// interface created on top of this VLAN.
		name := ""
		if opts.Prefix != "" {
			name = opts.Prefix + "bvlan" + strconv.Itoa(v.VLAN)
		}
		section(&b, "bridge-vlan", name)
		option(&b, "device", br.Name)
		option(&b, "vlan", strconv.Itoa(v.VLAN))
		for _, p := range v.Ports {
			list(&b, "ports", p)
		}
	}

	ifaces := append([]model.Interface(nil), br.Interfaces...)
	sort.SliceStable(ifaces, func(i, j int) bool { return ifaces[i].Name < ifaces[j].Name })

	for _, in := range ifaces {
		//
		// The controller replaces the whole type on the AP (ucimerge
		// ReplaceTypes), so the prefix is not needed to identify ownership here
		// — and adding it would be actively harmful: the firewall zone, the
		// DHCP config and every operator habit refer to these interfaces by
		// their device-local name. Renaming lan3 to ac_lan3 drops it out of
		// `firewall.@zone[0].network`, and with the default INPUT policy of
		// REJECT that locks SSH/HTTP/ICMP out of the AP while its own outbound
		// traffic keeps working — an outage that looks like dead hardware.
		// Observed on 10.0.30.251.
		section(&b, "interface", in.Name)
		if in.Device != "" {
			option(&b, "device", in.Device)
		}
		if in.Proto != "" {
			option(&b, "proto", in.Proto)
		}
		if in.IP6Assign != "" {
			option(&b, "ip6assign", in.IP6Assign)
		}
		if in.IP6IfaceID != "" {
			option(&b, "ip6ifaceid", in.IP6IfaceID)
		}
		if in.Broadcast != "" {
			option(&b, "broadcast", in.Broadcast)
		}
		for _, ip := range in.IPAddr {
			list(&b, "ipaddr", ip)
		}
		if in.Netmask != "" {
			option(&b, "netmask", in.Netmask)
		}
		if in.Gateway != "" {
			option(&b, "gateway", in.Gateway)
		}
		if in.DNS != "" {
			option(&b, "dns", in.DNS)
		}
	}

	return []byte(b.String()), nil
}

// Steering renders the steering-daemon instruction file. An empty template
// value means "do not manage steering", which is different from "none"
// (explicitly stop any daemon).
func Steering(t model.Template) []byte {
	daemon := t.Steering
	if daemon == "" {
		return nil
	}
	return []byte(daemon + "\n")
}

// SteerPolicy renders the parameters for the agent's local steering loop.
// A template without a policy renders no file at
// all: the agent then keeps its passive behaviour, which makes the whole
// feature opt-in per template.
func SteerPolicy(t model.Template) []byte {
	if t.SteerPolicy == nil {
		return nil
	}
	p := *t.SteerPolicy
	p.Normalize()
	body, err := json.Marshal(p)
	if err != nil {
		return nil
	}
	return append(body, '\n')
}

type renderedIface struct {
	name string
	src  model.SSID
	band string
}

// buildInterfaces expands each SSID into one BSS per band, then sorts the result
// so output does not depend on the order SSIDs were submitted in.
func buildInterfaces(t model.Template) []renderedIface {
	type pair struct {
		src  model.SSID
		band string
	}
	var pairs []pair
	for _, s := range t.SSIDs {
		for _, band := range s.Bands {
			pairs = append(pairs, pair{src: s, band: band})
		}
	}

	sort.SliceStable(pairs, func(i, j int) bool {
		bi, bj := bandOrder[pairs[i].band], bandOrder[pairs[j].band]
		if bi != bj {
			return bi < bj
		}
		if pairs[i].src.Network != pairs[j].src.Network {
			return pairs[i].src.Network < pairs[j].src.Network
		}
		if pairs[i].src.SSID != pairs[j].src.SSID {
			return pairs[i].src.SSID < pairs[j].src.SSID
		}
		return pairs[i].band < pairs[j].band
	})

	// Two SSIDs can sanitise to the same slug (e.g. "Home-5G" and "Home 5G"),
	// so collisions are disambiguated with a counter rather than dropped.
	used := map[string]int{}
	out := make([]renderedIface, 0, len(pairs))
	for _, p := range pairs {
		base := "ssid_" + slug(p.src.SSID) + "_" + p.band
		name := base
		if n := used[base]; n > 0 {
			name = base + "_" + strconv.Itoa(n+1)
		}
		used[base]++

		out = append(out, renderedIface{name: name, src: p.src, band: p.band})
	}
	return out
}

// Encryption maps the semantic security mode to the UCI encryption string. The
// cipher is spelled out explicitly because that is what the deployed AP runs;
// bare "psk2"/"sae" would let another package's default decide the cipher.
func Encryption(security string) string {
	switch security {
	case model.SecWPA2:
		return "psk2+ccmp"
	case model.SecWPA3:
		return "sae+ccmp"
	case model.SecWPA2WPA3:
		return "sae-mixed"
	case model.SecOWE:
		return "owe"
	default:
		return "none"
	}
}

func section(b *strings.Builder, kind, name string) {
	if b.Len() > 0 {
		b.WriteByte('\n')
	}
	if name == "" {
		fmt.Fprintf(b, "config %s\n", kind)
		return
	}
	fmt.Fprintf(b, "config %s %s\n", kind, quote(name))
}

func option(b *strings.Builder, key, value string) {
	fmt.Fprintf(b, "\toption %s %s\n", key, quote(value))
}

func list(b *strings.Builder, key, value string) {
	fmt.Fprintf(b, "\tlist %s %s\n", key, quote(value))
}

func boolOption(b *strings.Builder, key string, v *bool) {
	if v == nil {
		return
	}
	option(b, key, boolstr(*v))
}

func boolstr(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// quote wraps a value in UCI single quotes, escaping embedded quotes the way
// UCI's own parser expects.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// slug produces a UCI section name fragment. Section names must stay stable
// across renders, so only lossy-but-deterministic character mapping is used.
func slug(s string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore && b.Len() > 0 {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "bss"
	}
	return out
}
