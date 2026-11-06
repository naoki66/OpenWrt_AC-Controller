// Package model holds the configuration entities the controller pushes to an
// access point.
//
// The shapes here were corrected against a real deployment (see
// docs/ac-p0-baseline.md). Two differences from the original design matter:
//
//   - VLAN separation is DSA bridge-vlan (vlan_filtering on the bridge with a
//     PVID per port), not software 802.1q sub-interfaces.
//   - Radios carry an explicit phy index because all three MT7996 phys sit on
//     the same PCIe path; and 6 GHz runs EHT320, so Wi-Fi 7 htmode must be
//     expressible.
package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Bands, in the order radios are numbered on the XR1710G (radio0..radio2).
const (
	Band2G = "2g"
	Band5G = "5g"
	Band6G = "6g"
)

// Wireless security modes. Enterprise modes are deliberately absent: the
// project does not implement 802.1X or a portal (design §12).
const (
	SecWPA2     = "wpa2"
	SecWPA3     = "wpa3"
	SecWPA2WPA3 = "wpa2/wpa3"
	SecOWE      = "owe"
	SecOpen     = "open"
)

// Template is a complete provisioning intent for one AP.
type Template struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Bridge Bridge  `json:"bridge"`
	Radios []Radio `json:"radios"`
	SSIDs  []SSID  `json:"ssids"`
	// Steering names the client-steering daemon the AP should run. The
	// controller never makes per-client steering decisions itself (design D4):
	// those are millisecond-scale and belong on the AP.
	Steering string `json:"steering,omitempty"`
	// SteerPolicy, when set, is the parameters for the agent's local steering
	// loop (docs/ac-11v-steering.md): threshold-triggered 802.11v suggestions
	// with an optional force-disassociation backstop. Nil means "no policy" —
	// the agent keeps its passive behaviour and the file is not rendered.
	SteerPolicy *SteerPolicy `json:"steer_policy,omitempty"`
}

// SteerPolicy is rendered as the steer-policy file. Field semantics follow
// what commercial controllers expose (Omada Minimum RSSI + Force-Disassociation,
// Huawei Smart Roam): the agent detects a sticky client locally, suggests a
// transition via 802.11v BTM, and optionally deauthenticates it after the
// client ignores the suggestion. All durations are milliseconds; zero means
// "feature off" except the consecutive-sample count, which must be >= 1.
type SteerPolicy struct {
	// BtmSNR is the signal (dBm) at or below which a client counts as sticky.
	// Commercial default is -70 (aggressive -65, IoT-friendly -75).
	BtmSNR int `json:"btm_snr"`
	// BtmConsecutive samples (taken seconds apart) below BtmSNR before a BTM is
	// sent. Huawei triggers on the third consecutive weak sample.
	BtmConsecutive int `json:"btm_consecutive"`
	// BtmDisassocImminent marks the BTM request as imminent, which pushes
	// compliant clients to move at once. Off by default.
	BtmDisassocImminent bool `json:"btm_disassoc_imminent,omitempty"`
	// ForceAfterMS: when the client is still associated this long after the
	// BTM, it is deauthenticated (del_client). 0 disables the force path —
	// suggestion only, the safe default.
	ForceAfterMS int `json:"force_after_ms,omitempty"`
	// BanMS is the del_client ban_time: how long the AP refuses the client back
	// onto this BSS, which is what actually forces it to try a neighbour.
	BanMS int `json:"ban_ms,omitempty"`
	// MinConnectedMS protects clients that just associated from being steered
	// away before they even settle (default 30s in every surveyed product).
	MinConnectedMS int `json:"min_connected_ms,omitempty"`
	// CooldownMS suppresses further steering of the same client after a BTM or
	// a force kick — Huawei's "unable to roam" protection.
	CooldownMS int `json:"cooldown_ms,omitempty"`
	// MinRateMbps is the receive rate (Mbps) at or above which a weak-signal
	// client is left alone. RSSI is a poor proxy for "suffering": rate
	// adaptation already reacts to a degrading link, so a client at -81 dBm
	// that still holds 144 Mbps is fine where it is.
	//
	// Zero means "use the agent's default" (24). A negative value disables the
	// check, which is the only way to get pure RSSI behaviour — a large
	// positive value would exempt nobody, but it also would not say so.
	MinRateMbps int `json:"min_rate_mbps,omitempty"`
}

// Normalize fills the defaults a rendered policy always carries, so the file
// on the AP is self-describing and the agent never guesses.
func (p *SteerPolicy) Normalize() {
	if p.BtmConsecutive < 1 {
		p.BtmConsecutive = 3
	}
	if p.MinConnectedMS <= 0 {
		p.MinConnectedMS = 30000
	}
	if p.CooldownMS <= 0 {
		p.CooldownMS = 120000
	}
	if p.BanMS <= 0 && p.ForceAfterMS > 0 {
		p.BanMS = 30000
	}
}

// Validate rejects policies that would misbehave on the device.
func (p *SteerPolicy) Validate() error {
	if p.BtmSNR >= 0 {
		return fmt.Errorf("steer_policy.btm_snr %d is not a negative dBm value", p.BtmSNR)
	}
	if p.BtmConsecutive < 1 || p.BtmConsecutive > 60 {
		return fmt.Errorf("steer_policy.btm_consecutive %d outside 1..60", p.BtmConsecutive)
	}
	if p.ForceAfterMS < 0 || p.ForceAfterMS > 10*60*1000 {
		return fmt.Errorf("steer_policy.force_after_ms %d outside 0..600000", p.ForceAfterMS)
	}
	if p.BanMS < 0 || p.BanMS > 30*60*1000 {
		return fmt.Errorf("steer_policy.ban_ms %d outside 0..1800000", p.BanMS)
	}
	if p.MinRateMbps < -1 || p.MinRateMbps > 1000 {
		return fmt.Errorf("steer_policy.min_rate_mbps %d outside -1..1000", p.MinRateMbps)
	}
	return nil
}

// Steering daemons. nrsyncd only publishes 802.11k neighbour reports and lets
// the client decide; usteer and dawn actively steer, which the design warns
// against because their kick paths have produced disconnect storms.
const (
	SteeringNone    = "none"
	SteeringNRSyncd = "nrsyncd"
	SteeringUsteer  = "usteer"
	SteeringDawn    = "dawn"
)

// SSID is one BSS. Network names the netifd interface it is bridged into; the
// VLAN number lives on the bridge definition, not here, because the deployed AP
// names its interfaces lan/lan2/lan6 rather than by VLAN id.
type SSID struct {
	SSID  string   `json:"ssid"`
	Bands []string `json:"bands"`
	// Network is the netifd interface name the BSS bridges into (lan, lan2...).
	Network  string `json:"network"`
	Security string `json:"security"`
	PSK      string `json:"psk,omitempty"`
	Hidden   bool   `json:"hidden,omitempty"`
	Isolate  bool   `json:"isolate,omitempty"`
	// MaxSTA caps how many stations may associate with one BSS (UCI
	// `maxassoc`). Zero means hostapd's default, i.e. unlimited.
	MaxSTA int `json:"maxsta,omitempty"`
	// BusinessIsolate asserts that this service must not share its netifd
	// interface with another enabled SSID. The isolation itself is carried by
	// the bridge/VLAN the network points at — there is no hostapd knob for
	// cross-BSS isolation — so the flag is enforced at validation time instead
	// of being silently rendered into a no-op option.
	BusinessIsolate bool `json:"business_isolate,omitempty"`
	// Schedule is a planned on/off window for this BSS. The AP-side
	// wifi_schedule is deliberately disabled (docs/ac-boot-wireless.md), so
	// this is stored intent and shown in the UI; nothing executes it yet.
	Schedule *SSIDSchedule `json:"schedule,omitempty"`
	// MACFilter is hostapd's macfilter mode: empty/absent means no list,
	// "allow" is a whitelist and "deny" a blacklist. MACList carries the
	// addresses; it is only meaningful together with a mode.
	MACFilter string   `json:"macfilter,omitempty"`
	MACList   []string `json:"maclist,omitempty"`
	Disabled  bool     `json:"disabled,omitempty"`
	Roaming   Roaming  `json:"roaming,omitempty"`
	// 802.11v / WNM knobs seen on the deployed AP. IEEE80211V is separate from
	// Roaming.V: the deployed 2.4 GHz BSSes set the legacy flag while the 5/6 GHz
	// ones only set bss_transition, and conflating them produced 17 needless
	// differences against the baseline.
	IEEE80211V         *bool `json:"ieee80211v,omitempty"`
	WNMSleepMode       bool  `json:"wnm_sleep_mode,omitempty"`
	WNMSleepModeNoKeys bool  `json:"wnm_sleep_mode_no_keys,omitempty"`
	ProxyARP           bool  `json:"proxy_arp,omitempty"`
	// FTPSKGenerateLocal is tri-state: nil leaves it out of the rendered file so
	// the hostapd default applies.
	FTPSKGenerateLocal *bool `json:"ft_psk_generate_local,omitempty"`
	// DisassocLowAck is tri-state for the same reason; the deployed 5 GHz BSSes
	// set it to 0 explicitly.
	DisassocLowAck *bool `json:"disassoc_low_ack,omitempty"`
	// RRM emits rrm_neighbor_report/rrm_beacon_report directly.
	//
	// This mirrors LuCI, which exposes them as two independent switches under
	// 802.11k rather than a single flag. On the deployed firmware it is
	// redundant — `ieee80211k '1'` already makes hostapd default both to 1 — so
	// leaving it nil is the right default. It exists for the case where an
	// operator wants the reports on without the 11k umbrella flag.
	// See docs/ac-p0-baseline.md §8.
	RRM *bool `json:"rrm_neighbor_report,omitempty"`
}

// Roaming controls the 802.11k/v/r knobs. Steering itself is not done by the
// controller (design D4); these only enable the report/transition machinery.
type Roaming struct {
	K bool `json:"k"`
	V bool `json:"v"`
	R bool `json:"r"`
	// MobilityDomain is per VLAN on the deployed AP: the same value on 5 GHz and
	// 6 GHz lets a client FT between bands without a full re-association.
	MobilityDomain string `json:"mobility_domain,omitempty"`
	// FTOverDS is tri-state; the deployed baseline explicitly sets it to 0.
	FTOverDS *bool `json:"ft_over_ds,omitempty"`
}

// Radio pins one phy. MT7996 exposes three phys behind a single PCIe function,
// so Path alone cannot identify a radio: Index feeds `option radio 'N'`.
type Radio struct {
	Index   int    `json:"index"`
	Path    string `json:"path"`
	Band    string `json:"band"`
	Channel string `json:"channel"` // "auto" or a channel number
	// HTMode is emitted verbatim (HT20, HE40, EHT160, EHT320). Derive it with
	// HTModeFor when the template does not care.
	HTMode   string `json:"htmode"`
	Country  string `json:"country,omitempty"`
	TxPower  int    `json:"txpower"`
	Disabled bool   `json:"disabled,omitempty"`

	CellDensity *int `json:"cell_density,omitempty"`
	BSSColor    *int `json:"he_bss_color,omitempty"`
	// Beamformer / feature switches are pointers because "absent" and "false"
	// mean different things to hostapd.
	HESUBeamformer  *bool `json:"he_su_beamformer,omitempty"`
	HEMUBeamformer  *bool `json:"he_mu_beamformer,omitempty"`
	EHTSUBeamformer *bool `json:"eht_su_beamformer,omitempty"`
	EHTMUBeamformer *bool `json:"eht_mu_beamformer,omitempty"`
	RadarBackground *bool `json:"radar_background,omitempty"`
	TWTResponder    *bool `json:"he_twt_responder,omitempty"`
	// Extra is a pass-through for knobs the controller does not model. Rendered
	// after the known options, sorted by key, so output stays deterministic.
	Extra map[string]string `json:"extra,omitempty"`
}

// UnmarshalJSON accepts a channel written as a number, which an earlier version
// of the model did.
//
// Templates are stored as JSON blobs and never rewritten in place, so a stored
// template keeps whatever shape the model had the day it was saved. Decoding is
// all-or-nothing per row and one caller (the template list) fails its whole
// response on a single bad row, so a numeric channel from an old database would
// make every template disappear from the UI after an upgrade.
func (r *Radio) UnmarshalJSON(data []byte) error {
	type alias Radio
	aux := struct {
		Channel json.RawMessage `json:"channel"`
		*alias
	}{alias: (*alias)(r)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	// The embedded alias loses to the explicit field, so Channel above is the
	// raw value and alias.Channel stayed empty.
	r.Channel = ""
	if len(aux.Channel) == 0 || string(aux.Channel) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(aux.Channel, &s); err == nil {
		r.Channel = s
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(aux.Channel, &n); err != nil {
		return fmt.Errorf("radio channel: %w", err)
	}
	// A channel is an integer in practice, but JSON may carry it as 36.0.
	// Format with the shortest representation so both spellings decode alike.
	if i, err := n.Int64(); err == nil {
		r.Channel = strconv.FormatInt(i, 10)
		return nil
	}
	f, err := n.Float64()
	if err != nil {
		return fmt.Errorf("radio channel: %w", err)
	}
	r.Channel = strconv.FormatFloat(f, 'f', -1, 64)
	return nil
}

// Bridge describes the layer-2 fabric the AP is part of. It is intentionally
// separate from the SSIDs: several SSIDs share one VLAN, and VLAN numbering is a
// property of the site, not of a BSS.
type Bridge struct {
	Name         string       `json:"name"`
	Ports        []string     `json:"ports,omitempty"`
	IGMPSnooping *bool        `json:"igmp_snooping,omitempty"`
	VLANs        []BridgeVLAN `json:"vlans"`
	// VLANFiltering turns on `option vlan_filtering` for the bridge. Nil means
	// the option is not rendered at all, which keeps packages generated before
	// this field existed byte-identical and lets an AP keep its own setting.
	// The template editor sets it together with a non-empty VLAN list.
	VLANFiltering *bool `json:"vlan_filtering,omitempty"`
	// Interfaces are the netifd interfaces created on top of each bridge VLAN.
	Interfaces []Interface `json:"interfaces"`
}

// BridgeVLAN is one `config bridge-vlan` section. Port entries use the DSA
// suffix notation: "wan:t" (tagged), "lan2:u*" (untagged, PVID).
type BridgeVLAN struct {
	VLAN  int      `json:"vlan"`
	Ports []string `json:"ports"`
	// Name and Note are operator-facing metadata for the VLAN matrix. UCI has
	// no place for them, so they are stored in the template JSON and never
	// rendered.
	Name string `json:"name,omitempty"`
	Note string `json:"note,omitempty"`
}

// ManagesNetwork reports whether this template renders anything into the
// network file. A wireless-only template leaves Bridge zeroed and renders
// nothing, which is how an AP keeps the layer-2 configuration it was flashed
// with.
//
// The distinction matters for the per-unit override: addresses and port maps
// only mean something relative to the interfaces a template defines, so a
// template that defines none must ignore the override instead of rejecting it.
func (t Template) ManagesNetwork() bool {
	return (t.Bridge.Name != "" && len(t.Bridge.Ports) > 0) ||
		len(t.Bridge.VLANs) > 0 ||
		len(t.Bridge.Interfaces) > 0
}

// Interface is a netifd interface bound to a bridge VLAN.
type Interface struct {
	Name       string   `json:"name"`
	Device     string   `json:"device"`
	Proto      string   `json:"proto"`
	IPAddr     []string `json:"ipaddr,omitempty"`
	Netmask    string   `json:"netmask,omitempty"`
	Gateway    string   `json:"gateway,omitempty"`
	DNS        string   `json:"dns,omitempty"`
	Broadcast  string   `json:"broadcast,omitempty"`
	IP6Assign  string   `json:"ip6assign,omitempty"`
	IP6IfaceID string   `json:"ip6ifaceid,omitempty"`
}

var mobilityDomainRe = regexp.MustCompile(`^[0-9a-fA-F]{4}$`)
var clockRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
var macAddrRe = regexp.MustCompile(`^[0-9a-fA-F]{2}(:[0-9a-fA-F]{2}){5}$`)

// MAC filter modes hostapd understands. An absent value means "no filter"; it
// is different from a whitelist that happens to be empty (which rejects every
// station), so validation requires an explicit list for both modes.
const (
	MACFilterAllow = "allow"
	MACFilterDeny  = "deny"
)

// SSIDSchedule is one daily on/off window. Days use ISO numbering (1 = Monday
// … 7 = Sunday). Start and end are wall-clock times on the AP; a window may
// not cross midnight — two windows (one ending 23:59, one starting 00:00)
// express that, which keeps the future executor trivial.
type SSIDSchedule struct {
	Days  []int  `json:"days"`
	Start string `json:"start"`
	End   string `json:"end"`
}

// Validate rejects windows an executor could not evaluate unambiguously.
func (w SSIDSchedule) Validate(where string) error {
	var errs []error
	if len(w.Days) == 0 {
		errs = append(errs, fmt.Errorf("%s: at least one weekday is required", where))
	}
	seen := map[int]bool{}
	for _, d := range w.Days {
		if d < 1 || d > 7 {
			errs = append(errs, fmt.Errorf("%s: weekday %d outside 1-7", where, d))
		}
		if seen[d] {
			errs = append(errs, fmt.Errorf("%s: weekday %d listed twice", where, d))
		}
		seen[d] = true
	}
	if !clockRe.MatchString(w.Start) {
		errs = append(errs, fmt.Errorf("%s: start %q is not HH:MM", where, w.Start))
	}
	if !clockRe.MatchString(w.End) {
		errs = append(errs, fmt.Errorf("%s: end %q is not HH:MM", where, w.End))
	}
	if clockRe.MatchString(w.Start) && clockRe.MatchString(w.End) && w.Start >= w.End {
		errs = append(errs, fmt.Errorf("%s: start %s must be before end %s", where, w.Start, w.End))
	}
	return errors.Join(errs...)
}

// VHT is accepted because the 5G hardware offers it (verified via
// `iwinfo <if> htmodelist` on XR1710G: HT20 VHT20 HE20 EHT20 … VHT160 HE160
// EHT160) even though HTModeFor never derives it.
var htmodeRe = regexp.MustCompile(`^(HT|VHT|HE|EHT)(20|40|80|160|320)$`)

// Validate reports everything wrong with the template at once so an operator
// fixing a template over the API does not play whack-a-mole.
func (t Template) Validate() error {
	var errs []error
	if strings.TrimSpace(t.ID) == "" {
		errs = append(errs, errors.New("id is required"))
	}
	if strings.TrimSpace(t.Name) == "" {
		errs = append(errs, errors.New("name is required"))
	}
	if len(t.SSIDs) == 0 {
		errs = append(errs, errors.New("at least one ssid is required"))
	}

	networks := map[string]bool{}
	for _, iface := range t.Bridge.Interfaces {
		if strings.TrimSpace(iface.Name) == "" {
			errs = append(errs, errors.New("bridge.interfaces: name is required"))
			continue
		}
		networks[iface.Name] = true
	}

	seenBand := map[string]bool{}
	seenIndex := map[int]bool{}
	enabled := map[string]bool{}
	// netOwner records the first enabled SSID on each network, and isolatedNet
	// marks networks whose owner demanded business isolation. A flagged SSID
	// must be alone on its network no matter which side of the pair declares it.
	netOwner := map[string]string{}
	isolatedNet := map[string]bool{}
	for i, r := range t.Radios {
		where := fmt.Sprintf("radios[%d]", i)
		if !validBand(r.Band) {
			errs = append(errs, fmt.Errorf("%s: unknown band %q", where, r.Band))
			continue
		}
		if seenBand[r.Band] {
			errs = append(errs, fmt.Errorf("%s: band %q configured twice", where, r.Band))
		}
		seenBand[r.Band] = true
		// A duplicate index renders two sections with the same name, which uci
		// would silently rename. It is also the signature of a template written
		// against the pre-P1 model, where radios had no index at all.
		if seenIndex[r.Index] {
			errs = append(errs, fmt.Errorf("%s: radio index %d used twice", where, r.Index))
		}
		seenIndex[r.Index] = true
		if r.Channel != "" && r.Channel != "auto" {
			if _, err := parseChannel(r.Channel); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", where, err))
			}
		}
		if r.HTMode != "" && !htmodeRe.MatchString(r.HTMode) {
			errs = append(errs, fmt.Errorf("%s: htmode %q is not HT/VHT/HE/EHT + 20/40/80/160/320", where, r.HTMode))
		}
		if r.TxPower < 0 || r.TxPower > 40 {
			errs = append(errs, fmt.Errorf("%s: txpower %d out of range 0-40", where, r.TxPower))
		}
		if !r.Disabled {
			enabled[r.Band] = true
		}
	}
	// A template with every radio disabled would render an AP that beacons
	// nothing: the SSID checks below catch the common case indirectly, but an
	// explicit rule makes the intent visible and also covers an empty radio list.
	if len(enabled) == 0 {
		errs = append(errs, errors.New("at least one radio must be enabled"))
	}

	for i, s := range t.SSIDs {
		where := fmt.Sprintf("ssids[%d]", i)
		switch {
		case strings.TrimSpace(s.SSID) == "":
			errs = append(errs, fmt.Errorf("%s: ssid is required", where))
		case len(s.SSID) > 32:
			errs = append(errs, fmt.Errorf("%s: ssid longer than 32 bytes", where))
		}
		if len(s.Bands) == 0 {
			errs = append(errs, fmt.Errorf("%s: bands is required", where))
		}
		for _, b := range s.Bands {
			if !validBand(b) {
				errs = append(errs, fmt.Errorf("%s: unknown band %q", where, b))
				continue
			}
			if !enabled[b] {
				errs = append(errs, fmt.Errorf("%s: band %q has no enabled radio", where, b))
			}
		}
		if !s.Disabled {
			if s.Network == "" {
				errs = append(errs, fmt.Errorf("%s: network is required", where))
			} else if len(networks) > 0 && !networks[s.Network] {
				errs = append(errs, fmt.Errorf("%s: network %q is not defined in bridge.interfaces", where, s.Network))
			}
		}
		if err := validateSecurity(s); err != nil {
			errs = append(errs, fmt.Errorf("%s %q: %w", where, s.SSID, err))
		}
		if s.MaxSTA < 0 || s.MaxSTA > 512 {
			errs = append(errs, fmt.Errorf("%s %q: maxsta %d outside 0-512", where, s.SSID, s.MaxSTA))
		}
		switch s.MACFilter {
		case "", MACFilterAllow, MACFilterDeny:
			if s.MACFilter == MACFilterAllow || s.MACFilter == MACFilterDeny {
				if len(s.MACList) == 0 {
					errs = append(errs, fmt.Errorf("%s %q: macfilter %q needs at least one MAC", where, s.SSID, s.MACFilter))
				}
			} else if len(s.MACList) > 0 {
				errs = append(errs, fmt.Errorf("%s %q: maclist requires macfilter allow or deny", where, s.SSID))
			}
		default:
			errs = append(errs, fmt.Errorf("%s %q: macfilter %q must be allow or deny", where, s.SSID, s.MACFilter))
		}
		seenMAC := map[string]bool{}
		for _, m := range s.MACList {
			norm := strings.ToLower(strings.TrimSpace(m))
			if !macAddrRe.MatchString(norm) {
				errs = append(errs, fmt.Errorf("%s %q: maclist entry %q is not a MAC address", where, s.SSID, m))
				continue
			}
			if seenMAC[norm] {
				errs = append(errs, fmt.Errorf("%s %q: maclist entry %q is duplicated", where, s.SSID, m))
			}
			seenMAC[norm] = true
		}
		if s.BusinessIsolate && !s.Disabled && s.Network != "" {
			if other, ok := netOwner[s.Network]; ok {
				errs = append(errs, fmt.Errorf(
					"%s %q: business isolation requires a dedicated network; %q already uses %q",
					where, s.SSID, other, s.Network))
			}
			isolatedNet[s.Network] = true
		}
		if !s.Disabled && s.Network != "" {
			if other, ok := netOwner[s.Network]; ok {
				if isolatedNet[s.Network] {
					errs = append(errs, fmt.Errorf(
						"%s %q: network %q is reserved by %q with business isolation",
						where, s.SSID, s.Network, other))
				}
			} else {
				netOwner[s.Network] = s.SSID
			}
		}
		if s.Schedule != nil {
			if err := s.Schedule.Validate(where + " " + strconv.Quote(s.SSID) + " schedule"); err != nil {
				errs = append(errs, err)
			}
		}
		if s.Roaming.R && !mobilityDomainRe.MatchString(s.Roaming.MobilityDomain) {
			errs = append(errs, fmt.Errorf("%s %q: mobility_domain must be 4 hex digits when r is enabled", where, s.SSID))
		}
	}

	switch t.Steering {
	case "", SteeringNone, SteeringNRSyncd, SteeringUsteer, SteeringDawn:
	default:
		errs = append(errs, fmt.Errorf("steering %q is not one of none, nrsyncd, usteer, dawn", t.Steering))
	}

	if t.SteerPolicy != nil {
		if err := t.SteerPolicy.Validate(); err != nil {
			errs = append(errs, err)
		}
	}

	seenVLAN := map[int]bool{}
	for i, v := range t.Bridge.VLANs {
		if v.VLAN < 1 || v.VLAN > 4094 {
			errs = append(errs, fmt.Errorf("bridge.vlans[%d]: vlan %d out of range 1-4094", i, v.VLAN))
		}
		if seenVLAN[v.VLAN] {
			errs = append(errs, fmt.Errorf("bridge.vlans[%d]: vlan %d declared twice", i, v.VLAN))
		}
		seenVLAN[v.VLAN] = true
	}
	return errors.Join(errs...)
}

func validateSecurity(s SSID) error {
	switch s.Security {
	case SecWPA2, SecWPA3, SecWPA2WPA3:
		if len(s.PSK) < 8 || len(s.PSK) > 63 {
			return fmt.Errorf("psk must be 8-63 bytes for %s", s.Security)
		}
	case SecOWE, SecOpen:
		if s.PSK != "" {
			return fmt.Errorf("psk must be empty for %s", s.Security)
		}
	default:
		return fmt.Errorf("unknown security %q (want %s)", s.Security,
			strings.Join([]string{SecWPA2, SecWPA3, SecWPA2WPA3, SecOWE, SecOpen}, ", "))
	}
	return nil
}

// RequiresPSK reports whether the security mode carries a passphrase.
func (s SSID) RequiresPSK() bool {
	return s.Security == SecWPA2 || s.Security == SecWPA3 || s.Security == SecWPA2WPA3
}

func validBand(b string) bool {
	return b == Band2G || b == Band5G || b == Band6G
}

// HTModeFor derives the UCI htmode from band and channel width. EHT is used on
// 5 and 6 GHz because the deployed MT7996 runs Wi-Fi 7 there; 2.4 GHz stays HE
// (EHT is not defined for 20/40 MHz on this hardware).
func HTModeFor(band string, width int) (string, error) {
	switch band {
	case Band2G:
		switch width {
		case 20:
			return "HE20", nil
		case 40:
			return "HE40", nil
		}
		return "", fmt.Errorf("%s radio cannot use %d MHz", band, width)
	case Band5G:
		switch width {
		case 20:
			return "EHT20", nil
		case 40:
			return "EHT40", nil
		case 80:
			return "EHT80", nil
		case 160:
			return "EHT160", nil
		}
		return "", fmt.Errorf("%s radio cannot use %d MHz", band, width)
	case Band6G:
		switch width {
		case 20:
			return "EHT20", nil
		case 40:
			return "EHT40", nil
		case 80:
			return "EHT80", nil
		case 160:
			return "EHT160", nil
		case 320:
			return "EHT320", nil
		}
		return "", fmt.Errorf("%s radio cannot use %d MHz", band, width)
	}
	return "", fmt.Errorf("unknown band %q", band)
}

func parseChannel(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, fmt.Errorf("channel %q is neither 'auto' nor a number", s)
	}
	if n < 1 || n > 233 {
		return 0, fmt.Errorf("channel %d out of range", n)
	}
	return n, nil
}

// DeviceNetwork carries the values a shared template cannot express because they
// belong to one physical unit: which ports this AP actually wires, and which
// management addresses it owns.
//
// It exists because a template is deliberately shared (both XR1710Gs run the
// same 18 SSIDs) while the layer-2 fabric is not (one unit uplinks through
// lan2/lan3/lan4, the other wires them in a different order, and each has its
// own address). Splitting these keeps one template for the fleet without
// pretending the hardware is identical.
type DeviceNetwork struct {
	// BridgePorts overrides Bridge.Ports for this unit.
	BridgePorts []string `json:"bridge_ports,omitempty"`
	// VLANs replaces the template's bridge VLAN list entirely.
	//
	// Not a per-VLAN port override, because the set of VLANs is itself a
	// property of the unit: of the two deployed XR1710Gs, one carries a VLAN 3169
	// that the other simply does not have. Adjusting ports inside a fixed list
	// cannot express that.
	VLANs []BridgeVLAN `json:"vlans,omitempty"`
	// IPAddr overrides an interface's addresses, keyed by interface name.
	// Keyed by name rather than by VLAN because the address belongs to the
	// netifd interface, which is what an operator names in their notes.
	IPAddr map[string][]string `json:"ipaddr,omitempty"`
	// IGMPSnooping overrides Bridge.IGMPSnooping.
	IGMPSnooping *bool `json:"igmp_snooping,omitempty"`
	// VLANFiltering overrides Bridge.VLANFiltering. The editor sets it to true
	// when this unit's VLAN override is enabled, so the generated bridge filters
	// the VLANs it declares.
	VLANFiltering *bool `json:"vlan_filtering,omitempty"`
}

// IsZero reports whether the override changes anything. A zero override means
// "this unit uses the template as written", which is the normal case for a
// wireless-only template.
func (d DeviceNetwork) IsZero() bool {
	return len(d.BridgePorts) == 0 && len(d.VLANs) == 0 &&
		len(d.IPAddr) == 0 && d.IGMPSnooping == nil && d.VLANFiltering == nil
}

// WithDeviceNetwork returns a copy of the template with the per-unit values
// applied. The receiver is not modified: templates are shared, and a caller that
// rendered one device must not change what the next device sees.
// UnmanagedInterfaceAddrs lists the interfaces a device override assigns an
// address to that the template does not define. The controller renders only the
// interfaces in the template, so any address listed here would be dropped —
// which on a management interface means the AP stops being reachable.
func (t Template) UnmanagedInterfaceAddrs(ov DeviceNetwork) []string {
	known := map[string]bool{}
	for _, in := range t.Bridge.Interfaces {
		known[in.Name] = true
	}
	var out []string
	for name := range ov.IPAddr {
		if !known[name] {
			out = append(out, name)
		}
	}
	return out
}

// DeviceRadio pins radio settings that belong to one physical unit rather than
// the shared template: two APs running the same SSID layout often need
// different channels or power levels.
type DeviceRadio struct {
	Index    int    `json:"index"`
	Channel  string `json:"channel,omitempty"`
	HTMode   string `json:"htmode,omitempty"`
	TxPower  *int   `json:"txpower,omitempty"`
	Country  string `json:"country,omitempty"`
	Disabled *bool  `json:"disabled,omitempty"`
}

// DeviceRadioOverrides is the per-unit radio override document stored on a
// device. An empty list means "never overridden" and renders the template as
// written.
type DeviceRadioOverrides struct {
	Radios []DeviceRadio `json:"radios,omitempty"`
}

func (d DeviceRadioOverrides) IsZero() bool { return len(d.Radios) == 0 }

// WithDeviceRadios returns a copy of the template with per-unit radio values
// applied. Only the fields present on the override replace template values, so
// an override written for channel alone keeps the template's mode and power.
func (t Template) WithDeviceRadios(ov DeviceRadioOverrides) Template {
	if ov.IsZero() {
		return t
	}
	out := t
	out.Radios = append([]Radio(nil), t.Radios...)
	for i := range out.Radios {
		for _, o := range ov.Radios {
			if o.Index != out.Radios[i].Index {
				continue
			}
			r := &out.Radios[i]
			if o.Channel != "" {
				r.Channel = o.Channel
			}
			if o.HTMode != "" {
				r.HTMode = o.HTMode
			}
			if o.TxPower != nil {
				r.TxPower = *o.TxPower
			}
			if o.Country != "" {
				r.Country = o.Country
			}
			if o.Disabled != nil {
				r.Disabled = *o.Disabled
			}
		}
	}
	return out
}

func (t Template) WithDeviceNetwork(ov DeviceNetwork) Template {
	if ov.IsZero() {
		return t
	}

	out := t
	out.Bridge = t.Bridge
	out.Bridge.Ports = append([]string(nil), t.Bridge.Ports...)
	out.Bridge.VLANs = append([]BridgeVLAN(nil), t.Bridge.VLANs...)
	out.Bridge.Interfaces = append([]Interface(nil), t.Bridge.Interfaces...)

	if len(ov.BridgePorts) > 0 {
		out.Bridge.Ports = append([]string(nil), ov.BridgePorts...)
	}
	if ov.IGMPSnooping != nil {
		v := *ov.IGMPSnooping
		out.Bridge.IGMPSnooping = &v
	}
	if ov.VLANFiltering != nil {
		v := *ov.VLANFiltering
		out.Bridge.VLANFiltering = &v
	}
	if len(ov.VLANs) > 0 {
		out.Bridge.VLANs = append([]BridgeVLAN(nil), ov.VLANs...)
	}
	for i := range out.Bridge.Interfaces {
		name := out.Bridge.Interfaces[i].Name
		if addrs, ok := ov.IPAddr[name]; ok {
			out.Bridge.Interfaces[i].IPAddr = append([]string(nil), addrs...)
		}
	}
	return out
}
