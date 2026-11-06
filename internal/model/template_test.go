package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Templates are stored as JSON and never rewritten in place, so an old row keeps
// the shape the model had when it was saved. The channel used to be a number;
// decoding such a row must still work or every template vanishes from the UI
// after an upgrade.
func TestRadioChannelAcceptsLegacyNumber(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"legacy number", `{"index":1,"channel":6}`, "6"},
		{"number as float", `{"index":1,"channel":36.0}`, "36"},
		{"current string", `{"index":1,"channel":"149"}`, "149"},
		{"auto", `{"index":1,"channel":"auto"}`, "auto"},
		{"absent", `{"index":1}`, ""},
		{"null", `{"index":1,"channel":null}`, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var r Radio
			if err := json.Unmarshal([]byte(c.body), &r); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if r.Channel != c.want {
				t.Errorf("channel = %q, want %q", r.Channel, c.want)
			}
			if r.Index != 1 {
				t.Errorf("index = %d, want 1 (other fields must survive)", r.Index)
			}
		})
	}
}

// The custom unmarshaller must not swallow the fields around it.
func TestRadioUnmarshalKeepsEveryField(t *testing.T) {
	body := `{"index":2,"path":"pci0000:00/0000:00:00.0","band":"5g","channel":36,
		"htmode":"EHT160","country":"CN","txpower":23,"disabled":true,
		"he_su_beamformer":false,"extra":{"noscan":"1"}}`

	var r Radio
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.Index != 2 || r.Band != Band5G || r.HTMode != "EHT160" || r.TxPower != 23 {
		t.Errorf("scalar fields lost: %+v", r)
	}
	if !r.Disabled {
		t.Error("disabled lost")
	}
	if r.HESUBeamformer == nil || *r.HESUBeamformer {
		t.Error("tri-state pointer lost or wrong")
	}
	if r.Extra["noscan"] != "1" {
		t.Errorf("extra lost: %v", r.Extra)
	}
	if r.Channel != "36" {
		t.Errorf("channel = %q, want 36", r.Channel)
	}
}

// Round-trip: what the controller writes must decode back to the same value.
func TestRadioChannelRoundTripsAsString(t *testing.T) {
	in := Radio{Index: 1, Band: Band5G, Channel: "149", HTMode: "EHT80", TxPower: 20}
	blob, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out Radio
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Channel != in.Channel {
		t.Errorf("channel = %q, want %q", out.Channel, in.Channel)
	}

	// New rows must be written as strings, so the legacy path stays legacy.
	var raw map[string]any
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if _, ok := raw["channel"].(string); !ok {
		t.Errorf("channel serialised as %T, want string", raw["channel"])
	}
}

// A garbage channel must still be an error rather than silently becoming "".
func TestRadioChannelRejectsGarbage(t *testing.T) {
	var r Radio
	if err := json.Unmarshal([]byte(`{"index":1,"channel":{"a":1}}`), &r); err == nil {
		t.Error("expected an error for an object channel")
	}
}

func netTemplate() Template {
	return Template{
		ID:   "t",
		Name: "T",
		Bridge: Bridge{
			Name:         "br-lan",
			Ports:        []string{"lan2", "lan3"},
			IGMPSnooping: boolPtr(true),
			VLANs: []BridgeVLAN{
				{VLAN: 1, Ports: []string{"wan:u*"}},
				{VLAN: 2, Ports: []string{"lan2:u*", "wan:t"}},
			},
			Interfaces: []Interface{
				{Name: "lan", Device: "br-lan.1", Proto: "static", IPAddr: []string{"10.0.10.9/24"}},
				{Name: "lan2", Device: "br-lan.2", Proto: "static", IPAddr: []string{"10.0.20.9"}},
			},
		},
		Radios: []Radio{{Index: 1, Band: Band5G, Channel: "36", HTMode: "EHT160", TxPower: 20}},
		SSIDs:  []SSID{{SSID: "X", Bands: []string{Band5G}, Network: "lan", Security: SecWPA3, PSK: "passphrase-x"}},
	}
}

func boolPtr(b bool) *bool { return &b }

// Disabling every radio would render an AP that beacons nothing. The reference
// AC enforces "at least one radio enabled", and doing it here keeps the failure
// at save time instead of at the next AP heartbeat.
func TestTemplateRequiresEnabledRadio(t *testing.T) {
	tpl := netTemplate()
	tpl.Radios[0].Disabled = true
	err := tpl.Validate()
	if err == nil || !strings.Contains(err.Error(), "at least one radio must be enabled") {
		t.Fatalf("validate = %v, want the enabled-radio error", err)
	}
}

// The whole point of device-level parameters: one shared template, two units
// with different wiring and different addresses.
func TestWithDeviceNetworkAppliesPerUnitValues(t *testing.T) {
	base := netTemplate()
	ov := DeviceNetwork{
		BridgePorts: []string{"lan3", "lan4", "wan"},
		VLANs: []BridgeVLAN{
			{VLAN: 1, Ports: []string{"wan:u*"}},
			{VLAN: 2, Ports: []string{"lan3:u*", "wan:t"}},
			{VLAN: 7, Ports: []string{"lan4:u*"}},
		},
		IPAddr:       map[string][]string{"lan": {"10.0.10.250/24"}},
		IGMPSnooping: boolPtr(false),
	}

	got := base.WithDeviceNetwork(ov)

	if fmt.Sprint(got.Bridge.Ports) != "[lan3 lan4 wan]" {
		t.Errorf("bridge ports = %v", got.Bridge.Ports)
	}
	// The override replaces the whole VLAN list, because the set of VLANs is
	// itself a property of the unit.
	if len(got.Bridge.VLANs) != 3 {
		t.Fatalf("vlans = %v, want the override list", got.Bridge.VLANs)
	}
	if fmt.Sprint(got.Bridge.VLANs[1].Ports) != "[lan3:u* wan:t]" {
		t.Errorf("vlan 2 ports = %v", got.Bridge.VLANs[1].Ports)
	}
	if got.Bridge.VLANs[2].VLAN != 7 {
		t.Errorf("vlan 7 missing from the override: %v", got.Bridge.VLANs)
	}
	if fmt.Sprint(got.Bridge.Interfaces[0].IPAddr) != "[10.0.10.250/24]" {
		t.Errorf("lan ipaddr = %v", got.Bridge.Interfaces[0].IPAddr)
	}
	// An interface not named by the override keeps the template value.
	if fmt.Sprint(got.Bridge.Interfaces[1].IPAddr) != "[10.0.20.9]" {
		t.Errorf("lan2 ipaddr = %v, want the template value", got.Bridge.Interfaces[1].IPAddr)
	}
	if got.Bridge.IGMPSnooping == nil || *got.Bridge.IGMPSnooping {
		t.Error("igmp_snooping override not applied")
	}
}

// Templates are shared: rendering one device must not change what the next sees.
func TestWithDeviceNetworkDoesNotMutateTemplate(t *testing.T) {
	base := netTemplate()
	_ = base.WithDeviceNetwork(DeviceNetwork{
		BridgePorts: []string{"changed"},
		IPAddr:      map[string][]string{"lan": {"10.0.0.1/24"}},
	})

	if fmt.Sprint(base.Bridge.Ports) != "[lan2 lan3]" {
		t.Errorf("original bridge ports mutated: %v", base.Bridge.Ports)
	}
	if fmt.Sprint(base.Bridge.Interfaces[0].IPAddr) != "[10.0.10.9/24]" {
		t.Errorf("original interface mutated: %v", base.Bridge.Interfaces[0].IPAddr)
	}
}

func TestWithDeviceNetworkZeroIsIdentity(t *testing.T) {
	base := netTemplate()
	got := base.WithDeviceNetwork(DeviceNetwork{})
	if fmt.Sprint(got.Bridge.Ports) != fmt.Sprint(base.Bridge.Ports) {
		t.Error("zero override changed the template")
	}
}

// ManagesNetwork decides whether a per-unit override is even relevant: a
// wireless-only template renders nothing into the network file, so its
// addresses and port maps cannot be resolved against it. Each of the three
// things the renderer can emit has to count on its own — a template that only
// declares VLANs still writes `config bridge-vlan`.
func TestManagesNetwork(t *testing.T) {
	empty := netTemplate()
	empty.Bridge = Bridge{}
	if empty.ManagesNetwork() {
		t.Error("a template with no bridge declares management of the network file")
	}

	ports := netTemplate()
	ports.Bridge.Ports = []string{"lan2"}
	ports.Bridge.VLANs = nil
	ports.Bridge.Interfaces = nil
	if !ports.ManagesNetwork() {
		t.Error("bridge ports alone should count as managing the network")
	}

	vlans := netTemplate()
	vlans.Bridge.Ports = nil
	vlans.Bridge.Interfaces = nil
	if !vlans.ManagesNetwork() {
		t.Error("bridge VLANs alone should count as managing the network")
	}

	if !netTemplate().ManagesNetwork() {
		t.Error("a fully populated bridge is not reported as managed")
	}
}

func TestSSIDMaxSTAValidation(t *testing.T) {
	for _, n := range []int{-1, 513} {
		tpl := netTemplate()
		tpl.SSIDs[0].MaxSTA = n
		if err := tpl.Validate(); err == nil {
			t.Errorf("maxsta %d accepted", n)
		}
	}
	for _, n := range []int{0, 1, 512} {
		tpl := netTemplate()
		tpl.SSIDs[0].MaxSTA = n
		if err := tpl.Validate(); err != nil {
			t.Errorf("maxsta %d rejected: %v", n, err)
		}
	}
}

// A MAC filter is only meaningful with a mode and a non-empty list; an
// accidentally empty whitelist would lock every station out of the BSS.
func TestSSIDMACFilterValidation(t *testing.T) {
	bad := []SSID{
		{MACFilter: MACFilterAllow},
		{MACFilter: MACFilterDeny},
		{MACFilter: "block"},
		{MACList: []string{"aa:bb:cc:dd:ee:ff"}},
		{MACFilter: MACFilterAllow, MACList: []string{"not-a-mac"}},
		{MACFilter: MACFilterDeny, MACList: []string{"AA:BB:CC:DD:EE:FF", "aa:bb:cc:dd:ee:ff"}},
	}
	for _, s := range bad {
		tpl := netTemplate()
		tpl.SSIDs[0].MACFilter = s.MACFilter
		tpl.SSIDs[0].MACList = s.MACList
		if err := tpl.Validate(); err == nil {
			t.Errorf("macfilter %q list %v accepted", s.MACFilter, s.MACList)
		}
	}

	for _, s := range []SSID{
		{},
		{MACFilter: MACFilterAllow, MACList: []string{"AA:BB:CC:DD:EE:FF"}},
		{MACFilter: MACFilterDeny, MACList: []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"}},
	} {
		tpl := netTemplate()
		tpl.SSIDs[0].MACFilter = s.MACFilter
		tpl.SSIDs[0].MACList = s.MACList
		if err := tpl.Validate(); err != nil {
			t.Errorf("macfilter %q list %v rejected: %v", s.MACFilter, s.MACList, err)
		}
	}
}

// Business isolation is expressed by giving the service its own netifd
// interface; two enabled SSIDs sharing one cannot both claim it.
func TestBusinessIsolationRequiresDedicatedNetwork(t *testing.T) {
	tpl := netTemplate()
	tpl.SSIDs = append(tpl.SSIDs, SSID{
		SSID: "Lobby", Bands: []string{Band5G}, Network: "lan",
		Security: SecOpen, BusinessIsolate: true,
	})
	if err := tpl.Validate(); err == nil {
		t.Fatal("two SSIDs shared a network despite business isolation")
	}

	// The same pair passes once the isolated SSID gets its own interface.
	tpl.SSIDs[1].Network = "lan2"
	if err := tpl.Validate(); err != nil {
		t.Errorf("dedicated network rejected: %v", err)
	}
}

func TestSSIDScheduleValidation(t *testing.T) {
	bad := []SSIDSchedule{
		{Days: nil, Start: "08:00", End: "22:00"},
		{Days: []int{0}, Start: "08:00", End: "22:00"},
		{Days: []int{1, 1}, Start: "08:00", End: "22:00"},
		{Days: []int{1}, Start: "8:00", End: "22:00"},
		{Days: []int{1}, Start: "08:00", End: "24:00"},
		{Days: []int{1}, Start: "22:00", End: "08:00"},
		{Days: []int{1}, Start: "08:00", End: "08:00"},
	}
	for _, w := range bad {
		tpl := netTemplate()
		tpl.SSIDs[0].Schedule = &w
		if err := tpl.Validate(); err == nil {
			t.Errorf("schedule %+v accepted", w)
		}
	}

	tpl := netTemplate()
	tpl.SSIDs[0].Schedule = &SSIDSchedule{Days: []int{1, 2, 3, 4, 5}, Start: "07:30", End: "23:15"}
	if err := tpl.Validate(); err != nil {
		t.Errorf("valid schedule rejected: %v", err)
	}
}
