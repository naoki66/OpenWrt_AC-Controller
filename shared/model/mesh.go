package model

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// MeshBackhaul 描述回程配置；未设置或 mode=wired 时保持原有渲染结果不变。
// 当前 802.11s 仅承载一个无标签 LAN，VLAN 干线需要另行实现。
type MeshBackhaul struct {
	Mode      string `json:"mode"`
	ID        string `json:"id,omitempty"`
	Key       string `json:"key,omitempty"`
	Band      string `json:"band,omitempty"`
	Network   string `json:"network,omitempty"`
	Dedicated bool   `json:"dedicated,omitempty"`
}

func (t Template) MeshEnabled() bool { return t.Mesh != nil && t.Mesh.Mode == "80211s" }

var meshNetworkRE = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

func (t Template) validateMesh() error {
	m := t.Mesh
	if m == nil || m.Mode == "wired" {
		return nil
	}
	if m.Mode != "80211s" {
		return errors.New("mesh.mode must be wired or 80211s")
	}
	var errs []error
	bad := func(s string) { errs = append(errs, fmt.Errorf("mesh: %s", s)) }
	if len(m.ID) < 1 || len(m.ID) > 32 || strings.IndexFunc(m.ID, unicode.IsControl) >= 0 {
		bad("id must be 1-32 bytes without control characters")
	}
	if len(m.Key) < 8 || len(m.Key) > 63 || strings.IndexFunc(m.Key, unicode.IsControl) >= 0 {
		bad("SAE key must be 8-63 bytes without control characters")
	}
	if m.Band != Band2G && m.Band != Band5G {
		bad("backhaul band must be 2g or 5g; 6 GHz is not supported in this version")
	}
	if !meshNetworkRE.MatchString(m.Network) {
		bad("network must be a named netifd interface")
	}
	found := false
	for _, r := range t.Radios {
		if r.Band != m.Band || r.Disabled {
			continue
		}
		found = true
		n, err := strconv.Atoi(r.Channel)
		if err != nil || n < 1 || n > 196 {
			bad("backhaul radio requires a fixed channel shared by all nodes")
		}
		if r.Country == "" {
			bad("backhaul radio requires an explicit country")
		}
		for _, k := range []string{"channel", "band", "disabled", "htmode", "country", "type", "path", "radio"} {
			if _, ok := r.Extra[k]; ok {
				bad("radio.extra cannot override " + k + " on the backhaul radio")
			}
		}
	}
	if !found {
		bad("backhaul radio must exist and be enabled")
	}
	// 显式网桥用于在所有节点启用 STP；仅有无线配置的模板无法配置
	// 有线与 Mesh 回程共存时所需的网桥环路保护。实际效果仍需实机验证。
	if t.Bridge.Name == "" || len(t.Bridge.Ports) == 0 {
		bad("an explicit bridge with at least one Ethernet port is required")
	}
	if len(t.Bridge.VLANs) != 0 || (t.Bridge.VLANFiltering != nil && *t.Bridge.VLANFiltering) {
		bad("VLAN filtering/trunks are not supported over this single-LAN backhaul")
	}
	bridgeFound := false
	for _, iface := range t.Bridge.Interfaces {
		if iface.Name == m.Network && iface.Device == t.Bridge.Name {
			bridgeFound = true
		}
	}
	if !bridgeFound {
		bad("backhaul network must reference the template's untagged bridge")
	}
	for _, s := range t.SSIDs {
		if s.Disabled {
			continue
		}
		if s.Network != m.Network {
			bad("all enabled SSIDs must use the backhaul network")
		}
		if m.Dedicated {
			for _, b := range s.Bands {
				if b == m.Band {
					bad("dedicated backhaul band cannot also host an enabled SSID")
				}
			}
		}
	}
	return errors.Join(errs...)
}
