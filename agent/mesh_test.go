package agent

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/naoki66/ac-controller/shared/bundle"
)

const testMeshIface = `
config wifi-iface 'ac_mesh_backhaul'
	option device 'radio0'
	option mode 'mesh'
	option mesh_id 'test'
	option encryption 'sae'
	option key 'testpassword'
	option network 'lan'
`

func TestMeshPeerProbe(t *testing.T) {
	r := &fakeRunner{statuses: []string{`{"radio1":{"up":true,"interfaces":[{"section":"ac_mesh_backhaul","ifname":"mesh0","config":{"mode":"mesh"}},{"section":"ac_ap","ifname":"ap0","config":{"mode":"ap"}}]}}`}, replies: map[string]string{
		"iw dev mesh0 info":         "Interface mesh0\n addr 02:11:22:33:44:55\n",
		"iw dev mesh0 station dump": "Station 02:aa:bb:cc:dd:ee (on mesh0)\n signal: -61 [-61] dBm\n mesh plink: ESTAB\nStation 02:aa:bb:cc:dd:ff (on mesh0)\n mesh plink: OPN_SNT\n",
	}}
	stats := collectMesh(context.Background(), r)
	if len(stats) != 1 || stats[0].MAC != "02:11:22:33:44:55" || len(stats[0].Peers) != 2 || !stats[0].Peers[0].Established || stats[0].Peers[1].Established || stats[0].Peers[0].Signal != -61 {
		t.Fatalf("bad mesh report: %+v", stats)
	}
	if r.countCalls("iw dev ap0") != 0 {
		t.Fatal("client BSS probed as mesh")
	}
}

func TestMeshUplinkFailureRollsBackWirelessOnly(t *testing.T) {
	for _, reachable := range []bool{false, true} {
		t.Run(map[bool]string{false: "unreachable", true: "reachable"}[reachable], func(t *testing.T) {
			f := newApplyFixture(t, statusWith(true, "ac_ssid_corp_2g", "ac_mesh_backhaul"))
			files := packageFiles()
			files["wireless"] = append(files["wireless"], []byte(testMeshIface)...)
			delete(files, "network") // 仅检查管理地址无法发现这种上联丢失
			f.applier.ACAddr = "192.168.1.1:8090"
			f.applier.DialAC = func(context.Context, net.IP, string) error {
				if reachable {
					return nil
				}
				return errors.New("unreachable")
			}
			_, err := f.applier.Apply(context.Background(), bundle.Manifest{Rev: 1}, files)
			if reachable && err != nil {
				t.Fatal(err)
			}
			if !reachable {
				if err == nil || !strings.Contains(err.Error(), "mesh health check") {
					t.Fatalf("expected uplink rollback, got %v", err)
				}
				if f.read(t, "wireless") != localWirelessFile {
					t.Fatal("wireless config not restored")
				}
			}
		})
	}
}
