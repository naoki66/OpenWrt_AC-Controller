package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/naoki66/ac-controller/internal/proto"
	"github.com/naoki66/ac-controller/internal/store"
)

// uploadRow POSTs an image with its metadata in the query string, which is the
// contract the endpoint defines (body = image, no multipart parser).
func uploadRow(t *testing.T, f *fixture, token, model, version string, blob []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		f.server.URL+"/api/v1/firmwares?model="+model+"&version="+version, bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	return resp
}

// hello sends one hello through the AP channel and decodes the reply.
func (f *fixture) hello(t *testing.T, h proto.Hello) (*http.Response, proto.HelloReply) {
	t.Helper()
	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("marshal hello: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/api/v1/ap/hello", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return resp, proto.HelloReply{}
	}
	var reply proto.HelloReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	resp.Body.Close()
	return resp, reply
}

func TestFirmwareRepositoryCRUD(t *testing.T) {
	f := newFixture(t)
	blob := []byte("XR1710G firmware image bytes")
	sum := sha256.Sum256(blob)

	resp := uploadRow(t, f, "", "XR1710G", "20261026", blob)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d", resp.StatusCode)
	}
	stored := decode[store.Firmware](t, resp)
	if stored.SHA256 != hex.EncodeToString(sum[:]) || stored.Size != int64(len(blob)) {
		t.Errorf("stored metadata = %+v", stored)
	}

	if resp := uploadRow(t, f, "", "XR1710G", "20261026", blob); resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate upload status = %d, want 409", resp.StatusCode)
		resp.Body.Close()
	}

	list := decode[struct {
		Firmwares []store.Firmware `json:"firmwares"`
	}](t, f.do(http.MethodGet, "/api/v1/firmwares", nil))
	if len(list.Firmwares) != 1 || list.Firmwares[0].Version != "20261026" {
		t.Errorf("list = %+v", list.Firmwares)
	}

	// The image endpoint has to return exactly the uploaded bytes.
	img := f.do(http.MethodGet, "/api/v1/firmwares/image?model=XR1710G&version=20261026", nil)
	if img.StatusCode != http.StatusOK {
		t.Fatalf("image status = %d", img.StatusCode)
	}
	if got := img.Header.Get("X-Ac-Sha256"); got != stored.SHA256 {
		t.Errorf("X-Ac-Sha256 = %q", got)
	}
	body, _ := io.ReadAll(img.Body)
	img.Body.Close()
	if !bytes.Equal(body, blob) {
		t.Error("served image differs from the upload")
	}

	if resp := f.doAuth("", http.MethodDelete, "/api/v1/firmwares?model=XR1710G&version=20261026", nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete status = %d", resp.StatusCode)
	}
	if _, _, err := f.store.GetFirmware(context.Background(), "XR1710G", "20261026"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("image still present after delete: %v", err)
	}
}

func TestFirmwareUploadRequiresMetadata(t *testing.T) {
	f := newFixture(t)
	resp := uploadRow(t, f, "", "", "20261026", []byte("x"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for missing model", resp.StatusCode)
	}
	resp = uploadRow(t, f, "", "XR1710G", "", []byte("x"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for missing version", resp.StatusCode)
	}
}

func TestFirmwareAPIRequiresAdminToken(t *testing.T) {
	f := newFixtureWithAuth(t, "admin-token", "")

	if resp := uploadRow(t, f, "", "XR1710G", "20261026", []byte("x")); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("upload without token = %d, want 401", resp.StatusCode)
		resp.Body.Close()
	}
	if resp := f.do(http.MethodGet, "/api/v1/firmwares", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("list without token = %d, want 401", resp.StatusCode)
		resp.Body.Close()
	}
	if resp := uploadRow(t, f, "admin-token", "XR1710G", "20261026", []byte("x")); resp.StatusCode != http.StatusCreated {
		t.Errorf("upload with token = %d, want 201", resp.StatusCode)
		resp.Body.Close()
	}
}

// The image endpoint is shared: the AP pulls it with the shared secret, which is
// the whole point of adminOrAP.
func TestFirmwareImageAcceptsAPSecret(t *testing.T) {
	f := newFixtureWithAuth(t, "admin-token", "ap-secret")
	uploadRow(t, f, "admin-token", "XR1710G", "20261026", []byte("image")).Body.Close()

	if resp := f.doAuth("", http.MethodGet, "/api/v1/firmwares/image?model=XR1710G&version=20261026", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("image without credential = %d, want 401", resp.StatusCode)
		resp.Body.Close()
	}
	if resp := f.doAuth("ap-secret", http.MethodGet, "/api/v1/firmwares/image?model=XR1710G&version=20261026", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("image with AP secret = %d, want 200", resp.StatusCode)
		resp.Body.Close()
	}
}

func TestFirmwareTargetDrivesHello(t *testing.T) {
	f := newFixture(t)
	blob := []byte("target image")
	sum := sha256.Sum256(blob)
	uploadRow(t, f, "", "XR1710G", "20261026", blob).Body.Close()

	if resp := f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/adopt", map[string]string{}); resp.StatusCode != http.StatusOK {
		t.Fatalf("adopt status = %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// An unknown version is refused before it reaches the device row.
	if resp := f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/firmware", map[string]string{"version": "nope"}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown version status = %d, want 400", resp.StatusCode)
		resp.Body.Close()
	}

	d := decode[store.Device](t,
		f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/firmware", map[string]string{"version": "20261026"}))
	if d.FwTarget != "20261026" {
		t.Errorf("fw_target = %q, want 20261026", d.FwTarget)
	}

	_, reply := f.hello(t, proto.Hello{MAC: testMAC, Model: "XR1710G", Firmware: "20260101"})
	if reply.Firmware == nil {
		t.Fatal("hello reply carried no firmware instruction")
	}
	if reply.Firmware.Version != "20261026" || reply.Firmware.Size != int64(len(blob)) {
		t.Errorf("firmware ref = %+v", reply.Firmware)
	}
	if reply.Firmware.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("digest = %q", reply.Firmware.SHA256)
	}

	// "downloading" is in flight, not a verdict: the instruction must stay so an
	// AP that restarts mid-download can pick the image up again.
	f.hello(t, proto.Hello{MAC: testMAC, Firmware: "20260101", FirmwareState: proto.FirmwareDownloading})
	if _, reply := f.hello(t, proto.Hello{MAC: testMAC, Firmware: "20260101"}); reply.Firmware == nil {
		t.Error("controller stopped sending while the AP was still downloading")
	}

	// The AP reports the outcome; the controller then stops asking.
	f.hello(t, proto.Hello{MAC: testMAC, Firmware: "20261026",
		FirmwareState: proto.FirmwareApplied, FirmwareMessage: "running 20261026"})

	d, err := f.store.GetDevice(context.Background(), testMAC)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if d.FWState != proto.FirmwareApplied || d.FWMessage == "" || d.FWAt == 0 {
		t.Errorf("firmware report not stored: %+v", d)
	}
	if _, reply := f.hello(t, proto.Hello{MAC: testMAC, Firmware: "20261026"}); reply.Firmware != nil {
		t.Error("controller re-sent an image that already applied")
	}
}

func TestFirmwareTargetNotSentToPendingDevice(t *testing.T) {
	f := newFixture(t)
	uploadRow(t, f, "", "XR1710G", "20261026", []byte("image")).Body.Close()

	// Setting a target on a pending device is allowed (the operator may plan
	// ahead), but it must never reach the AP as an instruction.
	if resp := f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/firmware", map[string]string{"version": "20261026"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("set target status = %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	if _, reply := f.hello(t, proto.Hello{MAC: testMAC}); reply.Firmware != nil {
		t.Error("pending device received a firmware instruction")
	}
}

// Clearing the target is how an operator cancels an upgrade, including one that
// is stuck in "upgrading".
func TestClearFirmwareTarget(t *testing.T) {
	f := newFixture(t)
	uploadRow(t, f, "", "XR1710G", "20261026", []byte("image")).Body.Close()
	f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/adopt", map[string]string{}).Body.Close()
	f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/firmware", map[string]string{"version": "20261026"}).Body.Close()
	f.hello(t, proto.Hello{MAC: testMAC, FirmwareState: proto.FirmwareUpgrading})

	d := decode[store.Device](t,
		f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/firmware", map[string]string{"version": ""}))
	if d.FwTarget != "" || d.FWState != "" {
		t.Errorf("target was not cleared: %+v", d)
	}
	if _, reply := f.hello(t, proto.Hello{MAC: testMAC}); reply.Firmware != nil {
		t.Error("cleared target still produced a firmware instruction")
	}
}
