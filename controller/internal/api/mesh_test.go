package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/naoki66/ac-controller/shared/bundle"
	"github.com/naoki66/ac-controller/shared/model"
	"github.com/naoki66/ac-controller/shared/proto"
)

func TestMeshAdoptionSignedBundle(t *testing.T) {
	f := newFixture(t)
	tpl := model.Template{
		ID: "mesh-test", Name: "Mesh test",
		Bridge: model.Bridge{Name: "br-lan", Ports: []string{"lan1"}, Interfaces: []model.Interface{{Name: "lan", Device: "br-lan", Proto: "static", IPAddr: []string{"192.168.1.2/24"}}}},
		Radios: []model.Radio{{Index: 0, Band: "2g", Channel: "6", HTMode: "HE20", Country: "CN"}, {Index: 1, Band: "5g", Channel: "36", HTMode: "HE80", Country: "CN"}},
		SSIDs:  []model.SSID{{SSID: "Mesh test", Bands: []string{"2g"}, Network: "lan", Security: "wpa2", PSK: "client-password"}},
		Mesh:   &model.MeshBackhaul{Mode: "80211s", ID: "Test backhaul", Key: "backhaul-secret", Band: "5g", Network: "lan", Dedicated: true},
	}
	r := f.do("POST", "/api/v1/templates", tpl)
	if r.StatusCode != 200 && r.StatusCode != 201 {
		t.Fatalf("create mesh template: %d", r.StatusCode)
	}
	r.Body.Close()
	r = f.do("POST", "/api/v1/devices/"+testMAC+"/adopt", map[string]string{"template_id": tpl.ID})
	if r.StatusCode != 200 {
		body, _ := io.ReadAll(r.Body)
		t.Fatalf("adopt: %d %s", r.StatusCode, body)
	}
	r.Body.Close()
	r = f.do("GET", "/api/v1/devices/"+testMAC+"/package", nil)
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	manifest, files, err := bundle.Extract(body, f.key.Public)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.TemplateID != tpl.ID || !strings.Contains(string(files["wireless"]), "option mode 'mesh'") || !strings.Contains(string(files["network"]), "option stp '1'") {
		t.Fatal("signed provisioning did not carry mesh configuration")
	}
}

func TestMeshPartialUpdateAndAuthentication(t *testing.T) {
	f := newFixture(t)
	path := "/api/v1/templates/default/mesh"
	r := f.doAuth("", "PUT", path, map[string]any{"mesh": map[string]any{"mode": "wired"}})
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated mesh update: %d", r.StatusCode)
	}
	r.Body.Close()
	before, _ := f.store.GetTemplate(context.Background(), "default")
	r = f.do("PUT", path, map[string]any{"mesh": map[string]any{"mode": "wired"}})
	if r.StatusCode != 200 {
		t.Fatalf("mesh update: %d", r.StatusCode)
	}
	r.Body.Close()
	after, _ := f.store.GetTemplate(context.Background(), "default")
	if len(before.Radios) != len(after.Radios) || before.SSIDs[0].SSID != after.SSIDs[0].SSID || after.Mesh.Mode != "wired" {
		t.Fatal("partial update changed other template fields")
	}
	r = f.do("PUT", path, map[string]any{"mesh": model.MeshBackhaul{Mode: "80211s", Key: "short"}})
	if r.StatusCode != 400 {
		t.Fatalf("unsafe mesh plan: %d", r.StatusCode)
	}
	r.Body.Close()
	after, _ = f.store.GetTemplate(context.Background(), "default")
	if after.Mesh.Mode != "wired" {
		t.Fatal("invalid update persisted")
	}
}

func TestMeshHelloStateReplacedAndCleared(t *testing.T) {
	f := newFixture(t)
	h := proto.Hello{MAC: testMAC, Mesh: []proto.MeshStat{{Radio: "radio1", Ifname: "mesh0", Up: true, Peers: []proto.MeshPeer{{MAC: "02:aa:bb:cc:dd:ee", Established: true}}}}}
	r := f.do("POST", "/api/v1/ap/hello", h)
	if r.StatusCode != 200 {
		t.Fatalf("hello: %d", r.StatusCode)
	}
	r.Body.Close()
	rows, err := f.store.LatestMesh(context.Background())
	if err != nil || len(rows) != 1 || len(rows[0].Interfaces) != 1 {
		t.Fatalf("mesh state: %+v %v", rows, err)
	}
	h.Mesh = []proto.MeshStat{}
	r = f.do("POST", "/api/v1/ap/hello", h)
	r.Body.Close()
	rows, _ = f.store.LatestMesh(context.Background())
	if len(rows) != 1 || len(rows[0].Interfaces) != 0 {
		t.Fatal("old peers survived empty report")
	}
	h.Mesh = nil
	r = f.do("POST", "/api/v1/ap/hello", h)
	r.Body.Close()
	rows, _ = f.store.LatestMesh(context.Background())
	if len(rows) != 0 {
		t.Fatal("old report survived missing telemetry")
	}
}
