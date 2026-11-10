package render

import (
	"strings"
	"testing"

	"github.com/naoki66/ac-controller/shared/model"
	"github.com/naoki66/ac-controller/shared/ucimerge"
)

func meshTemplate() model.Template {
	t := sampleTemplate()
	t.Bridge = model.Bridge{Name: "br-lan", Ports: []string{"lan1"}, Interfaces: []model.Interface{{Name: "lan", Device: "br-lan", Proto: "static", IPAddr: []string{"192.168.1.2/24"}}}}
	t.SSIDs[0].Network = "lan"
	t.Radios[1].Country = "CN"
	t.Mesh = &model.MeshBackhaul{Mode: "80211s", ID: "XR-Mesh", Key: "test-mesh-secret", Band: "5g", Network: "lan"}
	return t
}

func TestMeshRenderAndRetirement(t *testing.T) {
	tpl := meshTemplate()
	w, err := Wireless(tpl, Options{Prefix: ACPrefix})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"config wifi-iface 'ac_mesh_backhaul'", "option mode 'mesh'", "option encryption 'sae'", "option mesh_id 'XR-Mesh'", "option device 'radio1'", "option network 'lan'"} {
		if !strings.Contains(string(w), want) {
			t.Fatalf("missing %s", want)
		}
	}
	n, err := Network(tpl, Options{Prefix: ACPrefix})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(n), "option stp '1'") {
		t.Fatal("mesh bridge has no STP")
	}
	if len(ucimerge.ACWifiInterfaces(string(w))) != 3 {
		t.Fatal("mesh interface missing from health checks")
	}
	tpl.Mesh = &model.MeshBackhaul{Mode: "wired"}
	wired, err := Wireless(tpl, Options{Prefix: ACPrefix})
	if err != nil {
		t.Fatal(err)
	}
	merged := ucimerge.Merge(string(w), string(wired), ucimerge.Options{})
	if strings.Contains(merged, "ac_mesh_backhaul") {
		t.Fatal("retired wireless backhaul survived wired switch")
	}
	tpl.Mesh = nil
	legacy, _ := Wireless(tpl, Options{Prefix: ACPrefix})
	if string(legacy) != string(wired) {
		t.Fatal("wired mode changed legacy output")
	}
}

func TestRejectUnsafeMeshPlans(t *testing.T) {
	cases := map[string]func(*model.Template){
		"auto channel":          func(t *model.Template) { t.Radios[1].Channel = "auto" },
		"missing country":       func(t *model.Template) { t.Radios[1].Country = "" },
		"weak key":              func(t *model.Template) { t.Mesh.Key = "short" },
		"newline key":           func(t *model.Template) { t.Mesh.Key = "abcdefgh\n" },
		"long ID":               func(t *model.Template) { t.Mesh.ID = strings.Repeat("中", 11) },
		"6 GHz":                 func(t *model.Template) { t.Mesh.Band = "6g" },
		"VLAN trunk":            func(t *model.Template) { t.Bridge.VLANs = []model.BridgeVLAN{{VLAN: 10}} },
		"no bridge":             func(t *model.Template) { t.Bridge = model.Bridge{} },
		"wrong bridge":          func(t *model.Template) { t.Bridge.Interfaces[0].Device = "eth0" },
		"dedicated collision":   func(t *model.Template) { t.Mesh.Dedicated = true },
		"radio override bypass": func(t *model.Template) { t.Radios[1].Extra = map[string]string{"channel": "auto"} },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			tpl := meshTemplate()
			change(&tpl)
			if tpl.Validate() == nil {
				t.Fatal("unsafe plan accepted")
			}
		})
	}
}
