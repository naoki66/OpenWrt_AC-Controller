package render

import (
	"strings"
	"testing"

	"github.com/naoki66/ac-controller/internal/model"
)

func networkTemplate() model.Template {
	return model.Template{
		ID:   "net",
		Name: "Net",
		Bridge: model.Bridge{
			Name:  "br-lan",
			Ports: []string{"lan2", "wan"},
			VLANs: []model.BridgeVLAN{{VLAN: 10, Ports: []string{"lan2:u*", "wan:t"}}},
			Interfaces: []model.Interface{
				{Name: "lan", Device: "br-lan.10", Proto: "static", IPAddr: []string{"10.0.10.5/24"}},
			},
		},
		Radios: []model.Radio{{Index: 1, Band: model.Band5G, Channel: "36", HTMode: "EHT160", TxPower: 20}},
		SSIDs: []model.SSID{{
			SSID: "Corp", Bands: []string{model.Band5G}, Network: "lan",
			Security: model.SecWPA3, PSK: "corp-passphrase",
		}},
	}
}

// When the template defines interfaces the controller owns layer 2 — but it
// must still spell the netifd interfaces the way the device does.
//
// Regression guard for a real outage: rendering `config interface 'ac_lan'`
// dropped the interface out of firewall.@zone[0].network, whose default INPUT
// policy is REJECT. The AP kept reaching the controller (OUTPUT is ACCEPT) and
// kept answering ARP (which bypasses netfilter), so it looked like dead
// hardware while it was in fact perfectly healthy and merely unreachable.
// Prefixing the BSS reference the same way is part of the same mistake.
func TestManagedNetworkKeepsInterfaceNames(t *testing.T) {
	tpl := networkTemplate()
	opts := Options{Prefix: ACPrefix}

	net, err := Network(tpl, opts)
	if err != nil {
		t.Fatalf("render network: %v", err)
	}
	if !strings.Contains(string(net), "config interface 'lan'") {
		t.Errorf("interface section lost its device-local name:\n%s", net)
	}
	if strings.Contains(string(net), "config interface 'ac_") {
		t.Errorf("interface section was prefixed:\n%s", net)
	}

	wifi, err := Wireless(tpl, opts)
	if err != nil {
		t.Fatalf("render wireless: %v", err)
	}
	if !strings.Contains(string(wifi), "option network 'lan'") {
		t.Errorf("BSS does not point at the managed interface:\n%s", wifi)
	}
	if strings.Contains(string(wifi), "option network 'ac_") {
		t.Errorf("BSS network reference was prefixed:\n%s", wifi)
	}
}

// The sections the controller does invent keep the prefix, so the AP-side
// merge can still tell them apart from anything authored on the device.
func TestManagedNetworkPrefixesInventedSections(t *testing.T) {
	net, err := Network(networkTemplate(), Options{Prefix: ACPrefix})
	if err != nil {
		t.Fatalf("render network: %v", err)
	}
	for _, want := range []string{"config device 'ac_bridge'", "config bridge-vlan 'ac_bvlan10'"} {
		if !strings.Contains(string(net), want) {
			t.Errorf("missing %q:\n%s", want, net)
		}
	}
}

// A bridge with VLANs must also filter them, otherwise the bridge-vlan
// sections are inert. Turning VLANs on in the editor therefore has to turn on
// vlan_filtering for br-lan in the same package.
func TestNetworkEnablesVLANFilteringWhenVLANsDeclared(t *testing.T) {
	tpl := networkTemplate()
	on := true
	tpl.Bridge.VLANFiltering = &on

	net, err := Network(tpl, Options{Prefix: ACPrefix})
	if err != nil {
		t.Fatalf("render network: %v", err)
	}
	if !strings.Contains(string(net), "option vlan_filtering '1'") {
		t.Errorf("VLANs declared and filtering enabled, but option missing:\n%s", net)
	}
}

// Without VLANs the option stays out, so disabling VLANs restores the
// device's own bridge behaviour instead of writing an explicit zero.
func TestNetworkOmitsVLANFilteringWithoutVLANs(t *testing.T) {
	tpl := networkTemplate()
	on := true
	tpl.Bridge.VLANFiltering = &on
	tpl.Bridge.VLANs = nil

	net, err := Network(tpl, Options{Prefix: ACPrefix})
	if err != nil {
		t.Fatalf("render network: %v", err)
	}
	if strings.Contains(string(net), "vlan_filtering") {
		t.Errorf("vlan_filtering emitted without VLANs:\n%s", net)
	}
}

// Nil is the pre-existing behaviour: no option, so the deployed baseline and
// packages generated before the field existed stay byte-identical.
func TestNetworkOmitsVLANFilteringWhenUnset(t *testing.T) {
	net, err := Network(networkTemplate(), Options{Prefix: ACPrefix})
	if err != nil {
		t.Fatalf("render network: %v", err)
	}
	if strings.Contains(string(net), "vlan_filtering") {
		t.Errorf("vlan_filtering emitted with a nil override:\n%s", net)
	}
}

// A wireless-only template keeps pointing at the device's own interfaces. This
// is the mode both APs run today: adding a prefix here would point every SSID at
// an interface that does not exist.
func TestWirelessOnlyTemplateKeepsLocalNetworkNames(t *testing.T) {
	tpl := networkTemplate()
	tpl.Bridge = model.Bridge{Name: "", Ports: nil, VLANs: nil, Interfaces: nil}

	wifi, err := Wireless(tpl, Options{Prefix: ACPrefix})
	if err != nil {
		t.Fatalf("render wireless: %v", err)
	}
	if !strings.Contains(string(wifi), "option network 'lan'") {
		t.Errorf("local network reference was rewritten:\n%s", wifi)
	}

	net, err := Network(tpl, Options{Prefix: ACPrefix})
	if err != nil {
		t.Fatalf("render network: %v", err)
	}
	if len(net) != 0 {
		t.Errorf("wireless-only template rendered %d bytes of network:\n%s", len(net), net)
	}
}

// Rendering the empty prefix must stay byte-compatible with the deployed
// configuration, which is what the P1 gate compares.
func TestEmptyPrefixAddsNothing(t *testing.T) {
	tpl := networkTemplate()

	net, _ := Network(tpl, Options{})
	if strings.Contains(string(net), "ac_lan") {
		t.Errorf("empty prefix leaked into network:\n%s", net)
	}
	wifi, _ := Wireless(tpl, Options{})
	if !strings.Contains(string(wifi), "option network 'lan'") {
		t.Errorf("empty prefix rewrote the network reference:\n%s", wifi)
	}
}

// maxassoc is the UCI name hostapd reads as max_num_sta; emitting it only when
// set keeps the deployed baseline (which has no limit) byte-identical.
func TestWirelessRendersMaxAssoc(t *testing.T) {
	tpl := networkTemplate()
	tpl.SSIDs[0].MaxSTA = 32
	wifi, err := Wireless(tpl, Options{Prefix: ACPrefix})
	if err != nil {
		t.Fatalf("render wireless: %v", err)
	}
	if !strings.Contains(string(wifi), "option maxassoc '32'") {
		t.Errorf("maxsta not rendered as maxassoc:\n%s", wifi)
	}

	tpl.SSIDs[0].MaxSTA = 0
	wifi, _ = Wireless(tpl, Options{Prefix: ACPrefix})
	if strings.Contains(string(wifi), "maxassoc") {
		t.Errorf("zero maxsta should leave the option out:\n%s", wifi)
	}
}

// hostapd reads macfilter (allow/deny) and maclist; addresses are emitted
// lower-case so the rendered file stays byte-identical across saves.
func TestWirelessRendersMACFilter(t *testing.T) {
	tpl := networkTemplate()
	tpl.SSIDs[0].MACFilter = model.MACFilterAllow
	tpl.SSIDs[0].MACList = []string{"AA:BB:CC:DD:EE:FF", "aa:bb:cc:dd:ee:01"}
	wifi, err := Wireless(tpl, Options{Prefix: ACPrefix})
	if err != nil {
		t.Fatalf("render wireless: %v", err)
	}
	if !strings.Contains(string(wifi), "option macfilter 'allow'") {
		t.Errorf("macfilter not rendered:\n%s", wifi)
	}
	for _, want := range []string{"list maclist 'aa:bb:cc:dd:ee:ff'", "list maclist 'aa:bb:cc:dd:ee:01'"} {
		if !strings.Contains(string(wifi), want) {
			t.Errorf("missing %s:\n%s", want, wifi)
		}
	}

	tpl.SSIDs[0].MACFilter = ""
	tpl.SSIDs[0].MACList = nil
	wifi, _ = Wireless(tpl, Options{Prefix: ACPrefix})
	if strings.Contains(string(wifi), "macfilter") || strings.Contains(string(wifi), "maclist") {
		t.Errorf("no filter should leave both options out:\n%s", wifi)
	}
}
