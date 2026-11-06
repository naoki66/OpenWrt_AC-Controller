package provision

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/naoki66/ac-controller/internal/model"
	"github.com/naoki66/ac-controller/internal/render"
	"github.com/naoki66/ac-controller/internal/store"
)

func managedNetworkTemplate() model.Template {
	return model.Template{
		ID:   "net",
		Name: "Layer 2 managed",
		Bridge: model.Bridge{
			Name:  "br-lan",
			Ports: []string{"lan2", "lan3", "wan"},
			VLANs: []model.BridgeVLAN{
				{VLAN: 10, Ports: []string{"lan2:u*", "wan:t"}},
			},
			Interfaces: []model.Interface{
				{Name: "lan", Device: "br-lan.10", Proto: "static", IPAddr: []string{"10.0.10.1/24"}},
			},
		},
		Radios: []model.Radio{{Index: 1, Band: model.Band5G, Channel: "36", HTMode: "EHT160", TxPower: 20}},
		SSIDs: []model.SSID{{
			SSID: "Corp", Bands: []string{model.Band5G}, Network: "lan",
			Security: model.SecWPA3, PSK: "corp-passphrase",
		}},
	}
}

// The point of device-level parameters: the same template produces different
// network files for two units, and the BSS ends up pointing at the managed
// interface.
func TestPackageUsesDeviceNetworkParams(t *testing.T) {
	svc := newService(t)
	if err := svc.Store.UpsertTemplate(context.Background(), managedNetworkTemplate()); err != nil {
		t.Fatalf("seed template: %v", err)
	}

	ov := model.DeviceNetwork{
		BridgePorts: []string{"lan3", "lan4", "wan"},
		VLANs: []model.BridgeVLAN{
			{VLAN: 10, Ports: []string{"lan3:u*", "wan:t"}},
		},
		IPAddr: map[string][]string{"lan": {"10.0.10.50/24"}},
	}
	blob, _ := json.Marshal(ov)
	if err := svc.Store.SetDeviceNetwork(context.Background(), ap1, string(blob)); err != nil {
		t.Fatalf("set device network: %v", err)
	}

	res, err := svc.Adopt(context.Background(), ap1, "net")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}

	files := extract(t, svc, res.MAC, res.Rev)
	network := string(files[render.FileNetwork])
	if network == "" {
		t.Fatal("no network file in the package")
	}

	for _, want := range []string{
		"list ipaddr '10.0.10.50/24'", // per-unit address
		"list ports 'lan3:u*'",        // per-unit VLAN port map
		"list ports 'lan4'",           // per-unit bridge ports
	} {
		if !strings.Contains(network, want) {
			t.Errorf("network missing %q:\n%s", want, network)
		}
	}
	if strings.Contains(network, "10.0.10.1/24") {
		t.Error("template address leaked through the override")
	}

	// A managed template must also re-point every BSS at the managed interface.
	// The name is the device's own, not ac_lan: /etc/config/firewall zones it by
	// that name and the default INPUT policy is REJECT.
	wireless := string(files[render.FileWireless])
	if !strings.Contains(wireless, "option network 'lan'") {
		t.Errorf("BSS does not reference the managed interface:\n%s", wireless)
	}
	if !strings.Contains(network, "config interface 'lan'") {
		t.Errorf("network does not define the managed interface:\n%s", network)
	}
	if strings.Contains(network, "config interface 'ac_") {
		t.Errorf("network prefixed a netifd interface:\n%s", network)
	}
}

// Radio settings are per-unit too: the AP renders its own channel and power
// instead of the shared template's values.
func TestPackageUsesDeviceRadioOverrides(t *testing.T) {
	svc := newService(t)
	if err := svc.Store.UpsertTemplate(context.Background(), managedNetworkTemplate()); err != nil {
		t.Fatalf("seed template: %v", err)
	}

	power := 17
	ov := model.DeviceRadioOverrides{Radios: []model.DeviceRadio{
		{Index: 1, Channel: "100", TxPower: &power},
	}}
	blob, _ := json.Marshal(ov)
	if err := svc.Store.SetDeviceRadios(context.Background(), ap1, string(blob)); err != nil {
		t.Fatalf("set device radios: %v", err)
	}

	res, err := svc.Adopt(context.Background(), ap1, "net")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	wireless := string(extract(t, svc, res.MAC, res.Rev)[render.FileWireless])
	for _, want := range []string{"option channel '100'", "option txpower '17'"} {
		if !strings.Contains(wireless, want) {
			t.Errorf("wireless missing %q:\n%s", want, wireless)
		}
	}
	if strings.Contains(wireless, "option channel '36'") {
		t.Errorf("template channel leaked through the override:\n%s", wireless)
	}
}

// Two units, one template: the rendered network must differ where the hardware
// does, and match where it does not.
func TestTwoDevicesShareTemplateWithDifferentNetwork(t *testing.T) {
	svc := newService(t)
	if err := svc.Store.UpsertTemplate(context.Background(), managedNetworkTemplate()); err != nil {
		t.Fatalf("seed template: %v", err)
	}

	set := func(mac string, ov model.DeviceNetwork) {
		blob, _ := json.Marshal(ov)
		if err := svc.Store.SetDeviceNetwork(context.Background(), mac, string(blob)); err != nil {
			t.Fatalf("set %s: %v", mac, err)
		}
	}
	set(ap1, model.DeviceNetwork{
		BridgePorts: []string{"lan2", "lan3", "wan"},
		IPAddr:      map[string][]string{"lan": {"10.0.10.251/24"}},
	})
	set(ap2, model.DeviceNetwork{
		BridgePorts: []string{"lan3", "lan4", "wan"},
		IPAddr:      map[string][]string{"lan": {"10.0.10.250/24"}},
	})

	r1, err := svc.Adopt(context.Background(), ap1, "net")
	if err != nil {
		t.Fatalf("adopt ap1: %v", err)
	}
	r2, err := svc.Adopt(context.Background(), ap2, "net")
	if err != nil {
		t.Fatalf("adopt ap2: %v", err)
	}

	n1 := string(extract(t, svc, r1.MAC, r1.Rev)[render.FileNetwork])
	n2 := string(extract(t, svc, r2.MAC, r2.Rev)[render.FileNetwork])

	if !strings.Contains(n1, "10.0.10.251/24") || !strings.Contains(n1, "list ports 'lan2'") {
		t.Errorf("ap1 network wrong:\n%s", n1)
	}
	if !strings.Contains(n2, "10.0.10.250/24") || !strings.Contains(n2, "list ports 'lan4'") {
		t.Errorf("ap2 network wrong:\n%s", n2)
	}

	// The wireless side is shared and must be identical for both units.
	w1 := string(extract(t, svc, r1.MAC, r1.Rev)[render.FileWireless])
	w2 := string(extract(t, svc, r2.MAC, r2.Rev)[render.FileWireless])
	if w1 != w2 {
		t.Error("wireless differs between devices sharing a template")
	}
}

// Without an override a managed template still renders from the template alone,
// which is how a newly adopted AP behaves before anyone fills in its wiring.
func TestPackageFallsBackToTemplateWithoutOverride(t *testing.T) {
	svc := newService(t)
	if err := svc.Store.UpsertTemplate(context.Background(), managedNetworkTemplate()); err != nil {
		t.Fatalf("seed template: %v", err)
	}

	res, err := svc.Adopt(context.Background(), ap1, "net")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	network := string(extract(t, svc, res.MAC, res.Rev)[render.FileNetwork])
	if !strings.Contains(network, "10.0.10.1/24") {
		t.Errorf("template address missing:\n%s", network)
	}
}

// Release keeps the per-unit layer-2 override: it records how the box is wired,
// which is a property of the unit and not of the last push. The consequence is
// that a re-adopt has to use a template whose interface set matches the
// override. Adopting the placeholder "default" template is refused instead of
// rendered — a rendered package would drop the management address and lock the
// operator out of the AP.
//
// This is the shape of the real failure: 解绑, then 采纳 from the UI, which used
// to post template_id "default" unconditionally.
func TestReadoptAfterReleaseRequiresMatchingTemplate(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	for _, tpl := range []model.Template{managedNetworkTemplate(), DefaultTemplate()} {
		if err := svc.Store.UpsertTemplate(ctx, tpl); err != nil {
			t.Fatalf("seed template %s: %v", tpl.ID, err)
		}
	}

	blob, _ := json.Marshal(model.DeviceNetwork{
		IPAddr: map[string][]string{"lan": {"10.0.10.251/24"}},
	})
	if err := svc.Store.SetDeviceNetwork(ctx, ap1, string(blob)); err != nil {
		t.Fatalf("set device network: %v", err)
	}
	first, err := svc.Adopt(ctx, ap1, "net")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}

	if err := svc.Store.Release(ctx, ap1); err != nil {
		t.Fatalf("release: %v", err)
	}
	d, err := svc.Store.GetDevice(ctx, ap1)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if d.State != store.StatePending || d.TemplateID != "" {
		t.Errorf("release left state=%q template=%q, want pending with no template", d.State, d.TemplateID)
	}
	if d.NetworkParams == "" {
		t.Fatal("release dropped the layer-2 override; it describes the unit, not the push")
	}

	_, err = svc.Adopt(ctx, ap1, "default")
	if err == nil {
		t.Fatal("adopting a template that does not define 'lan' was allowed")
	}
	if !strings.Contains(err.Error(), "no such interface") || !strings.Contains(err.Error(), "default") {
		t.Errorf("error should name the unmanaged interface and the template, got: %v", err)
	}

	// The matching template still adopts, and the revision keeps climbing: a
	// release is not a rollback of the package history.
	again, err := svc.Adopt(ctx, ap1, "net")
	if err != nil {
		t.Fatalf("re-adopt with the matching template: %v", err)
	}
	if again.Rev <= first.Rev {
		t.Errorf("rev went from %d to %d, want strictly increasing", first.Rev, again.Rev)
	}
}

// wirelessOnlyTemplate is a template that renders nothing into the network
// file: the AP keeps whatever layer-2 configuration it was flashed with. Its
// BSSes may still name a device-local interface, which is why Validate accepts
// an unknown network when the template declares no interfaces.
func wirelessOnlyTemplate() model.Template {
	t := managedNetworkTemplate()
	t.ID = "wifi"
	t.Name = "Wireless only"
	t.Bridge = model.Bridge{}
	return t
}

// 只接管无线的模板不渲染 network 文件。设备上为另一个模板写下的覆盖
// （lan..lan6 的地址与端口表）此时既不可用、也不算错：照它渲染会凭空生成
// 模板没要求的 bridge 段，照它校验则会把"从全接管退回只接管无线"这条路堵死。
// 覆盖本身要留着——那台机器的接线和地址没变，切回全接管时不该让操作员重录。
func TestWirelessOnlyTemplateIgnoresDeviceNetwork(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	for _, tpl := range []model.Template{managedNetworkTemplate(), wirelessOnlyTemplate()} {
		if err := svc.Store.UpsertTemplate(ctx, tpl); err != nil {
			t.Fatalf("seed template %s: %v", tpl.ID, err)
		}
	}

	blob, _ := json.Marshal(model.DeviceNetwork{
		BridgePorts: []string{"lan2", "lan3", "lan4", "wan"},
		IPAddr: map[string][]string{
			"lan": {"10.0.10.250/24"},
		},
	})
	if err := svc.Store.SetDeviceNetwork(ctx, ap2, string(blob)); err != nil {
		t.Fatalf("set device network: %v", err)
	}

	res, err := svc.Adopt(ctx, ap2, "wifi")
	if err != nil {
		t.Fatalf("adopt with a wireless-only template must not be blocked by the override: %v", err)
	}

	files := extract(t, svc, res.MAC, res.Rev)
	if network := files[render.FileNetwork]; len(network) != 0 {
		t.Errorf("wireless-only template rendered a network file:/n%s", network)
	}
	wireless := string(files[render.FileWireless])
	if !strings.Contains(wireless, "config wifi-iface") {
		t.Errorf("wireless file missing:/n%s", wireless)
	}

	// The override survives, so switching back needs no re-entry.
	d, err := svc.Store.GetDevice(ctx, ap2)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if d.TemplateID != "wifi" {
		t.Errorf("template_id = %q, want wifi", d.TemplateID)
	}
	if d.NetworkParams == "" {
		t.Error("the layer-2 override was dropped by a wireless-only adopt")
	}

	// And going back to the template it was written for still uses it.
	back, err := svc.Adopt(ctx, ap2, "net")
	if err != nil {
		t.Fatalf("adopt back to the network template: %v", err)
	}
	network := string(extract(t, svc, back.MAC, back.Rev)[render.FileNetwork])
	if !strings.Contains(network, "list ipaddr '10.0.10.250/24'") {
		t.Errorf("override not applied after switching back:/n%s", network)
	}
}
