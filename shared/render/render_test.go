package render

import (
	"strings"
	"testing"

	"github.com/naoki66/ac-controller/shared/model"
	"github.com/naoki66/ac-controller/shared/ucimerge"
)

func sampleTemplate() model.Template {
	return model.Template{
		ID:   "t",
		Name: "t",
		Bridge: model.Bridge{
			Name:  "br-lan",
			VLANs: []model.BridgeVLAN{{VLAN: 10, Ports: []string{"wan:t"}}},
			Interfaces: []model.Interface{
				{Name: "ac_vlan10", Device: "br-lan.10", Proto: "none"},
			},
		},
		Radios: []model.Radio{
			{Index: 0, Band: model.Band2G, Channel: "6", HTMode: "HE20", TxPower: 20},
			{Index: 1, Band: model.Band5G, Channel: "36", HTMode: "EHT160", TxPower: 23},
		},
		SSIDs: []model.SSID{{
			SSID:     "SSID-A",
			Bands:    []string{model.Band2G, model.Band5G},
			Network:  "ac_vlan10",
			Security: model.SecWPA2WPA3,
			PSK:      "correct horse battery",
		}},
	}
}

// Rendered output must not depend on the order SSIDs or radios were submitted
// in, otherwise re-saving a template would produce a new revision with
// identical content.
func TestRenderIsDeterministic(t *testing.T) {
	base := sampleTemplate()
	a, err := Wireless(base, Options{Prefix: ACPrefix})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	shuffled := sampleTemplate()
	shuffled.Radios = []model.Radio{base.Radios[1], base.Radios[0]}
	shuffled.SSIDs = append([]model.SSID{{
		SSID:     "Corp",
		Bands:    []string{model.Band5G, model.Band2G},
		Network:  "ac_vlan10",
		Security: model.SecWPA3,
		PSK:      "corp-passphrase",
	}}, base.SSIDs...)
	b, err := Wireless(shuffled, Options{Prefix: ACPrefix})
	if err != nil {
		t.Fatalf("render shuffled: %v", err)
	}
	// SSID-A on 2 bands + Corp on 2 bands.
	if n := strings.Count(string(b), "config wifi-iface"); n != 4 {
		t.Fatalf("expected 4 BSS, got %d:\n%s", n, b)
	}

	again, err := Wireless(shuffled, Options{Prefix: ACPrefix})
	if err != nil {
		t.Fatalf("render again: %v", err)
	}
	if string(b) != string(again) {
		t.Error("renderer is not deterministic")
	}
	if string(a) == string(b) {
		t.Error("different templates produced identical output")
	}
}

// UCI section names are unique per file regardless of type. The bridge-vlan and
// the interface on top of it must therefore not share a name — uci would silently
// rename one of them on commit.
func TestSectionNamesAreUnique(t *testing.T) {
	out, err := Network(sampleTemplate(), Options{Prefix: ACPrefix})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := ucimerge.Validate(string(out)); err != nil {
		t.Errorf("rendered network fails validation: %v\n%s", err, out)
	}

	seen := map[string]bool{}
	for _, s := range ucimerge.Parse(string(out)).Sections {
		if s.Name == "" {
			continue
		}
		if seen[s.Name] {
			t.Errorf("duplicate section name %q\n%s", s.Name, out)
		}
		seen[s.Name] = true
	}
}

// The BSSes carry the controller prefix; the radios do not. A wifi-device name
// is referenced from outside the rendered file — by the wifi-iface's
// `option device`, by LuCI's radio page and by the vendor's documentation — and
// it is what a freshly flashed AP already contains, so it stays radio0..radio2.
// Ownership does not depend on the prefix: the merge replaces whatever section
// the rendered set claims by type and name.
func TestManagedSectionNames(t *testing.T) {
	w, err := Wireless(sampleTemplate(), Options{Prefix: ACPrefix})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"config wifi-device 'radio0'",
		"config wifi-device 'radio1'",
		"config wifi-iface 'ac_ssid_ssid_a_2g'",
		"config wifi-iface 'ac_ssid_ssid_a_5g'",
		// The BSS must point at the very section that was just declared.
		"option device 'radio0'",
		"option device 'radio1'",
	} {
		if !strings.Contains(string(w), want) {
			t.Errorf("missing %q\n%s", want, w)
		}
	}
	if strings.Contains(string(w), "ac_radio") {
		t.Errorf("wifi-device name still carries the controller prefix\n%s", w)
	}

	n, err := Network(sampleTemplate(), Options{Prefix: ACPrefix})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// Interfaces keep their own names: SSIDs reference them and the site
	// addresses them, so a prefix would break both.
	if !strings.Contains(string(n), "config interface 'ac_vlan10'") {
		t.Errorf("interface name was prefixed\n%s", n)
	}
}

// The naming change is a migration, not just a rename: an AP that already runs
// the prefixed sections must end up with exactly one radio0 and no ac_radio0,
// and the surviving radio0 must be the enabled one.
func TestMigrationRetiresPrefixedRadios(t *testing.T) {
	deployed := `config wifi-device 'ac_radio0'
	option type 'mac80211'
	option band '2g'
	option disabled '0'

config wifi-device 'radio0'
	option type 'mac80211'
	option band '2g'
	option disabled '1'

config wifi-iface 'ac_ssid_home_2g'
	option device 'ac_radio0'
	option ssid 'SSID-A'
`

	rendered, err := Wireless(sampleTemplate(), Options{Prefix: ACPrefix})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	merged := ucimerge.Merge(deployed, string(rendered), ucimerge.Options{
		DisableLocal: true,
		DisableTypes: []string{"wifi-device", "wifi-iface"},
	})

	if strings.Contains(merged, "ac_radio0") {
		t.Errorf("the outdated prefixed radio survived the merge:\n%s", merged)
	}

	// Exactly one wifi-device per index, and it must not be a disabled leftover
	// of the factory config.
	blocks := strings.Split(merged, "config ")
	for _, b := range blocks {
		if !strings.HasPrefix(b, "wifi-device 'radio0'") {
			continue
		}
		if strings.Contains(b, "option disabled '1'") {
			t.Errorf("radio0 is still the disabled factory section:\n%s", merged)
		}
	}
	if n := strings.Count(merged, "config wifi-device 'radio0'"); n != 1 {
		t.Errorf("radio0 appears %d times, want 1:\n%s", n, merged)
	}

	// The BSS must follow the radio it is bound to.
	if !strings.Contains(merged, "option device 'radio0'") {
		t.Errorf("wifi-iface still points at a radio name that no longer exists:\n%s", merged)
	}
}

func TestQuoteEscaping(t *testing.T) {
	tpl := sampleTemplate()
	tpl.SSIDs[0].SSID = "Bob's WiFi"
	tpl.SSIDs[0].PSK = "pa'ss phrase"
	out, err := Wireless(tpl, Options{Prefix: ACPrefix})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(out), `option ssid 'Bob'\''s WiFi'`) {
		t.Errorf("ssid not escaped:\n%s", out)
	}
	if !strings.Contains(string(out), `option key 'pa'\''ss phrase'`) {
		t.Errorf("key not escaped:\n%s", out)
	}
}

func TestSectionNameCollision(t *testing.T) {
	tpl := sampleTemplate()
	tpl.SSIDs = []model.SSID{
		{SSID: "Home 5G", Bands: []string{model.Band2G}, Network: "ac_vlan10", Security: model.SecOpen},
		{SSID: "Home-5G", Bands: []string{model.Band2G}, Network: "ac_vlan10", Security: model.SecOpen},
	}
	out, err := Wireless(tpl, Options{Prefix: ACPrefix})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(out), "config wifi-iface 'ac_ssid_home_5g_2g'") ||
		!strings.Contains(string(out), "config wifi-iface 'ac_ssid_home_5g_2g_2'") {
		t.Errorf("collision not disambiguated:\n%s", out)
	}
}

func TestBandWithoutRadioIsRejected(t *testing.T) {
	tpl := sampleTemplate()
	tpl.SSIDs[0].Bands = []string{model.Band6G}
	if _, err := Wireless(tpl, Options{Prefix: ACPrefix}); err == nil {
		t.Fatal("expected validation error for 6g band without a 6g radio")
	}
}

func TestUndefinedNetworkIsRejected(t *testing.T) {
	tpl := sampleTemplate()
	tpl.SSIDs[0].Network = "ac_vlan99"
	if _, err := Wireless(tpl, Options{Prefix: ACPrefix}); err == nil {
		t.Fatal("expected validation error for an SSID pointing at an undefined interface")
	}
}

func TestHTModeFor(t *testing.T) {
	cases := []struct {
		band  string
		width int
		want  string
	}{
		{model.Band2G, 20, "HE20"},
		{model.Band2G, 40, "HE40"},
		{model.Band5G, 80, "EHT80"},
		{model.Band5G, 160, "EHT160"},
		{model.Band6G, 320, "EHT320"},
	}
	for _, c := range cases {
		got, err := model.HTModeFor(c.band, c.width)
		if err != nil {
			t.Fatalf("HTModeFor(%s,%d): %v", c.band, c.width, err)
		}
		if got != c.want {
			t.Errorf("HTModeFor(%s,%d) = %q, want %q", c.band, c.width, got, c.want)
		}
	}
	if _, err := model.HTModeFor(model.Band2G, 80); err == nil {
		t.Error("2g + 80 MHz should be rejected")
	}
	if _, err := model.HTModeFor(model.Band5G, 320); err == nil {
		t.Error("5g + 320 MHz should be rejected")
	}
}

func TestEncryptionMapping(t *testing.T) {
	cases := map[string]string{
		model.SecWPA2:     "psk2+ccmp",
		model.SecWPA3:     "sae+ccmp",
		model.SecWPA2WPA3: "sae-mixed",
		model.SecOWE:      "owe",
		model.SecOpen:     "none",
	}
	for in, want := range cases {
		if got := Encryption(in); got != want {
			t.Errorf("Encryption(%q) = %q, want %q", in, got, want)
		}
	}
}

// A template written against the pre-P1 model has no radio index, so all three
// radios decode to index 0 and render three sections with the same name. That
// must be rejected at validation rather than shipped to an AP.
func TestDuplicateRadioIndexIsRejected(t *testing.T) {
	tpl := sampleTemplate()
	tpl.Radios[1].Index = 0
	if _, err := Wireless(tpl, Options{Prefix: ACPrefix}); err == nil {
		t.Fatal("expected validation error for duplicate radio index")
	}
}

// 802.11k: the deployed firmware ignores `option ieee80211k`, so the real keys
// must be emittable. Nil means "leave it to the platform".
func TestRRMKeys(t *testing.T) {
	tpl := sampleTemplate()
	if out, err := Wireless(tpl, Options{Prefix: ACPrefix}); err != nil {
		t.Fatalf("render: %v", err)
	} else if strings.Contains(string(out), "rrm_neighbor_report") {
		t.Errorf("RRM emitted without being requested:/n%s", out)
	}

	on := true
	tpl.SSIDs[0].RRM = &on
	out, err := Wireless(tpl, Options{Prefix: ACPrefix})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"option rrm_neighbor_report '1'",
		"option rrm_beacon_report '1'",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q\n%s", want, out)
		}
	}
}

func TestSteeringFile(t *testing.T) {
	tpl := sampleTemplate()
	if got := Steering(tpl); got != nil {
		t.Errorf("unmanaged steering should render nothing, got %q", got)
	}

	tpl.Steering = model.SteeringNRSyncd
	if got := string(Steering(tpl)); got != "nrsyncd\n" {
		t.Errorf("steering = %q", got)
	}
}

func TestUnknownSteeringRejected(t *testing.T) {
	tpl := sampleTemplate()
	tpl.Steering = "magic"
	if _, err := Wireless(tpl, Options{Prefix: ACPrefix}); err == nil {
		t.Fatal("expected validation error for an unknown steering daemon")
	}
}

// The steer-policy file only exists when the template carries a policy; its
// content is the normalized JSON the agent's loop parses back.
func TestSteerPolicyFile(t *testing.T) {
	tpl := model.Template{}
	if got := SteerPolicy(tpl); got != nil {
		t.Fatalf("no policy in template, got %s", got)
	}

	tpl.SteerPolicy = &model.SteerPolicy{BtmSNR: -75, BtmConsecutive: 2, ForceAfterMS: 5000}
	got := string(SteerPolicy(tpl))
	for _, want := range []string{
		`"btm_snr":-75`,
		`"btm_consecutive":2`,
		`"force_after_ms":5000`,
		// Normalization must land in the file, not just in the agent.
		`"ban_ms":30000`,
		`"min_connected_ms":30000`,
		`"cooldown_ms":120000`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("steer-policy file missing %s:/n%s", want, got)
		}
	}
}
