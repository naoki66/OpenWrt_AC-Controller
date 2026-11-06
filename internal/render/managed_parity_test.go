package render

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/naoki66/ac-controller/internal/model"
)

// The gate for taking over layer 2: take the deployed network config of each
// AP, reverse it into device-level parameters, render it back through the shared
// template, and require the result to match what is actually on the box.
//
// This is the only check that can catch a mismatched port map before it is
// pushed — and a wrong port map is an outage, not a cosmetic bug. It runs over
// both units because they differ exactly where it matters: the ports.
func TestManagedNetworkReproducesDeployedConfig(t *testing.T) {
	tpl := loadGolden(t) // the XR1710G template, bridge included
	if len(tpl.Bridge.Interfaces) == 0 {
		t.Fatal("golden template defines no interfaces; it cannot be used to test layer-2 takeover")
	}

	for _, ap := range []string{"ap250", "ap251"} {
		t.Run(ap, func(t *testing.T) {
			dir := filepath.Join(baselineDir, "..", ap)
			body, err := os.ReadFile(filepath.Join(dir, "network.uci"))
			if err != nil {
				t.Skipf("no deployed network config for %s: %v", ap, err)
			}

			params := deviceNetworkFromUCI(t, string(body))
			eff := tpl.WithDeviceNetwork(params)

			rendered, err := Network(eff, Options{Prefix: ACPrefix})
			if err != nil {
				t.Fatalf("render network: %v", err)
			}

			want := keyed(t, string(body))
			got := keyed(t, string(rendered))
			if diffs := diffSections(want, got); len(diffs) > 0 {
				t.Errorf("%s: rendered network differs from the deployed one in %d place(s):", ap, len(diffs))
				for _, d := range diffs {
					t.Errorf("  %s", d)
				}
				t.Logf("rendered:\n%s", rendered)
			}
		})
	}
}

// deviceNetworkFromUCI reverses a deployed network.uci into the per-unit values
// the template cannot express. Anything the template already covers (a VLAN's
// existence, an interface's proto) is deliberately not extracted.
func deviceNetworkFromUCI(t *testing.T, body string) model.DeviceNetwork {
	t.Helper()

	ov := model.DeviceNetwork{IPAddr: map[string][]string{}}
	for _, s := range parseSections(body) {
		switch s.Type {
		case "device":
			if s.Opts["name"] == "br-lan" {
				ov.BridgePorts = s.Lists["ports"]
				// The deployed units disagree on igmp_snooping: one has it, the
				// other simply omits the line. Record the omission explicitly so
				// the override does not inherit the template's value.
				if v, ok := s.Opts["igmp_snooping"]; ok {
					on := v == "1"
					ov.IGMPSnooping = &on
				} else {
					off := false
					ov.IGMPSnooping = &off
				}
			}
		case "bridge-vlan":
			ports := s.Lists["ports"]
			var vlan int
			if _, err := fmt.Sscanf(s.Opts["vlan"], "%d", &vlan); err == nil {
				ov.VLANs = append(ov.VLANs, model.BridgeVLAN{VLAN: vlan, Ports: ports})
			}
		case "interface":
			// The deployed config writes both styles depending on the section.
			addrs := append([]string(nil), s.Lists["ipaddr"]...)
			if v, ok := s.Opts["ipaddr"]; ok {
				addrs = append(addrs, v)
			}
			if len(addrs) > 0 {
				ov.IPAddr[s.Name] = addrs
			}
		}
	}
	return ov
}
