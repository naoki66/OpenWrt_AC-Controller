package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/naoki66/ac-controller/internal/proto"
	"github.com/naoki66/ac-controller/internal/store"
)

func TestLEDScheduleValidation(t *testing.T) {
	f := newFixture(t)

	// Pending devices are not managed yet, so they have no schedule to set.
	resp := f.do(http.MethodPut, "/api/v1/devices/"+testMAC+"/led-schedule",
		map[string]string{"on_at": "07:00", "off_at": "23:00"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("pending status = %d, want 409", resp.StatusCode)
	}

	resp = f.do(http.MethodPut, "/api/v1/devices/not-a-mac/led-schedule",
		map[string]string{"on_at": "07:00"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed MAC status = %d, want 400", resp.StatusCode)
	}

	f.adopt()

	resp = f.do(http.MethodPut, "/api/v1/devices/"+testMAC+"/led-schedule",
		map[string]string{"on_at": "7am", "off_at": "23:00"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad time status = %d, want 400", resp.StatusCode)
	}

	resp = f.do(http.MethodPut, "/api/v1/devices/"+testMAC+"/led-schedule",
		map[string]string{"on_at": "07:00", "off_at": "07:00"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("equal times status = %d, want 400", resp.StatusCode)
	}

	d := decode[store.Device](t, f.do(http.MethodPut,
		"/api/v1/devices/"+testMAC+"/led-schedule",
		map[string]string{"on_at": "07:00", "off_at": "23:00"}))
	if d.LEDOnAt != "07:00" || d.LEDOffAt != "23:00" {
		t.Errorf("schedule = %+v", d)
	}
	if d.LEDOnNextAt <= time.Now().Unix() || d.LEDOffNextAt <= time.Now().Unix() {
		t.Errorf("next times = %d/%d, want in the future", d.LEDOnNextAt, d.LEDOffNextAt)
	}

	// Empty times disable the schedule and clear the due times.
	d = decode[store.Device](t, f.do(http.MethodPut,
		"/api/v1/devices/"+testMAC+"/led-schedule", map[string]string{}))
	if d.LEDOnAt != "" || d.LEDOffAt != "" || d.LEDOnNextAt != 0 || d.LEDOffNextAt != 0 {
		t.Errorf("cleared schedule = %+v", d)
	}
}

func TestLEDActionValidation(t *testing.T) {
	f := newFixture(t)
	f.adopt()

	resp := f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/actions",
		map[string]any{"type": proto.ActionLEDSet, "args": map[string]string{"state": "on"}})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("led_set status = %d, want 202", resp.StatusCode)
	}
	created := decode[store.Action](t, resp)
	if created.Type != proto.ActionLEDSet || created.Args["state"] != "on" {
		t.Errorf("created action = %+v", created)
	}
	resp.Body.Close()

	resp = f.do(http.MethodPost, "/api/v1/devices/"+testMAC+"/actions",
		map[string]any{"type": proto.ActionLEDSet, "args": map[string]string{"state": "blink"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad state status = %d, want 400", resp.StatusCode)
	}
}

func TestRunDueLEDsQueuesOnce(t *testing.T) {
	f := newFixture(t)
	f.adopt()
	ctx := context.Background()
	now := time.Now().Unix()
	if err := f.store.SetLEDSchedule(ctx, testMAC, "07:00", "", now-60, 0); err != nil {
		t.Fatalf("set LED schedule: %v", err)
	}

	f.srv.runDueLEDs(ctx)
	list, err := f.store.ListActions(ctx, testMAC, 10)
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(list) != 1 || list[0].Type != proto.ActionLEDSet || list[0].State != store.ActionPending {
		t.Fatalf("actions = %+v, want one pending led_set", list)
	}
	if list[0].Args["state"] != "on" {
		t.Errorf("led_set args = %+v, want state on", list[0].Args)
	}

	d, err := f.store.GetDevice(ctx, testMAC)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if d.LEDOnNextAt <= now {
		t.Errorf("next_at = %d, want advanced past %d", d.LEDOnNextAt, now)
	}

	// The schedule must not fire twice for the same occurrence.
	f.srv.runDueLEDs(ctx)
	list, _ = f.store.ListActions(ctx, testMAC, 10)
	if len(list) != 1 {
		t.Errorf("actions after second pass = %+v, want one", list)
	}
}

func TestRunDueLEDsSkipsWhenOneIsPending(t *testing.T) {
	f := newFixture(t)
	f.adopt()
	ctx := context.Background()
	now := time.Now().Unix()

	if _, err := f.store.CreateAction(ctx, testMAC, proto.ActionLEDSet, map[string]string{"state": "off"}); err != nil {
		t.Fatalf("create manual action: %v", err)
	}
	if err := f.store.SetLEDSchedule(ctx, testMAC, "07:00", "", now-60, 0); err != nil {
		t.Fatalf("set LED schedule: %v", err)
	}

	f.srv.runDueLEDs(ctx)
	list, _ := f.store.ListActions(ctx, testMAC, 10)
	if len(list) != 1 {
		t.Errorf("actions = %+v, want the manual one only", list)
	}
	// The due time still advances, otherwise the next tick would keep retrying.
	d, _ := f.store.GetDevice(ctx, testMAC)
	if d.LEDOnNextAt <= now {
		t.Errorf("next_at = %d, want advanced", d.LEDOnNextAt)
	}
}

func TestEarliestLEDEvent(t *testing.T) {
	now := int64(100)
	d := store.Device{LEDOnAt: "07:00", LEDOffAt: "23:00", LEDOnNextAt: 80, LEDOffNextAt: 90}
	ev, ok := earliestLEDEvent(d, now)
	if !ok || !ev.on || ev.at != 80 {
		t.Errorf("earliest = %+v ok=%v, want on edge at 80", ev, ok)
	}

	d.LEDOnNextAt = 90
	d.LEDOffNextAt = 50
	ev, ok = earliestLEDEvent(d, now)
	if !ok || ev.on || ev.at != 50 {
		t.Errorf("earliest = %+v ok=%v, want off edge at 50", ev, ok)
	}

	d.LEDOnNextAt, d.LEDOffNextAt = 0, 0
	if _, ok := earliestLEDEvent(d, now); ok {
		t.Error("event reported with no due edge")
	}
}
