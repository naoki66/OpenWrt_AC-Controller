package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/naoki66/ac-controller/controller/internal/store"
	"github.com/naoki66/ac-controller/shared/proto"
)

func TestRebootScheduleValidation(t *testing.T) {
	f := newFixture(t)
	body := map[string]any{"period_days": 1, "at": "03:00"}

	// Pending devices are not managed yet, so they have no schedule to set.
	resp := f.do(http.MethodPut, "/api/v1/devices/"+testMAC+"/reboot-schedule", body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("pending status = %d, want 409", resp.StatusCode)
	}

	resp = f.do(http.MethodPut, "/api/v1/devices/not-a-mac/reboot-schedule", body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed MAC status = %d, want 400", resp.StatusCode)
	}

	f.adopt()

	for _, c := range []struct {
		name string
		body map[string]any
	}{
		{"bad time", map[string]any{"period_days": 1, "at": "3am"}},
		{"too many days", map[string]any{"period_days": 400, "at": "03:00"}},
		{"negative days", map[string]any{"period_days": -1, "at": ""}},
	} {
		resp := f.do(http.MethodPut, "/api/v1/devices/"+testMAC+"/reboot-schedule", c.body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", c.name, resp.StatusCode)
		}
	}

	d := decode[store.Device](t, f.do(http.MethodPut,
		"/api/v1/devices/"+testMAC+"/reboot-schedule",
		map[string]any{"period_days": 2, "at": "03:00"}))
	if d.RebootPeriodDays != 2 || d.RebootAt != "03:00" {
		t.Errorf("schedule = %+v", d)
	}
	if d.RebootNextAt <= time.Now().Unix() {
		t.Errorf("next_at = %d, want in the future", d.RebootNextAt)
	}

	// Zero disables and clears the clock fields.
	d = decode[store.Device](t, f.do(http.MethodPut,
		"/api/v1/devices/"+testMAC+"/reboot-schedule", map[string]any{"period_days": 0}))
	if d.RebootPeriodDays != 0 || d.RebootAt != "" || d.RebootNextAt != 0 {
		t.Errorf("cleared schedule = %+v", d)
	}
}

func TestRunDueRebootsQueuesOnce(t *testing.T) {
	f := newFixture(t)
	f.adopt()
	ctx := context.Background()
	now := time.Now().Unix()
	if err := f.store.SetRebootSchedule(ctx, testMAC, 1, "03:00", now-60); err != nil {
		t.Fatalf("set schedule: %v", err)
	}

	f.srv.runDueReboots(ctx)
	list, err := f.store.ListActions(ctx, testMAC, 10)
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(list) != 1 || list[0].Type != proto.ActionReboot || list[0].State != store.ActionPending {
		t.Fatalf("actions = %+v, want one pending reboot", list)
	}

	d, err := f.store.GetDevice(ctx, testMAC)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if d.RebootNextAt <= now {
		t.Errorf("next_at = %d, want advanced past %d", d.RebootNextAt, now)
	}

	// The schedule must not fire twice for the same occurrence.
	f.srv.runDueReboots(ctx)
	list, _ = f.store.ListActions(ctx, testMAC, 10)
	if len(list) != 1 {
		t.Errorf("actions after second pass = %+v, want one", list)
	}
}

func TestRunDueRebootsSkipsWhenOneIsPending(t *testing.T) {
	f := newFixture(t)
	f.adopt()
	ctx := context.Background()
	now := time.Now().Unix()

	if _, err := f.store.CreateAction(ctx, testMAC, proto.ActionReboot, nil); err != nil {
		t.Fatalf("create manual action: %v", err)
	}
	if err := f.store.SetRebootSchedule(ctx, testMAC, 1, "03:00", now-60); err != nil {
		t.Fatalf("set schedule: %v", err)
	}

	f.srv.runDueReboots(ctx)
	list, _ := f.store.ListActions(ctx, testMAC, 10)
	if len(list) != 1 {
		t.Errorf("actions = %+v, want the manual one only", list)
	}
	// The due time still advances, otherwise the next tick would keep retrying.
	d, _ := f.store.GetDevice(ctx, testMAC)
	if d.RebootNextAt <= now {
		t.Errorf("next_at = %d, want advanced", d.RebootNextAt)
	}
}

func TestNextClockOccurrence(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 11, 2, 2, 59, 0, 0, loc)

	got, err := nextClockOccurrence(now, "03:00")
	if err != nil {
		t.Fatalf("next occurrence: %v", err)
	}
	if want := time.Date(2026, 11, 2, 3, 0, 0, 0, loc); !got.Equal(want) {
		t.Errorf("same-day next = %v, want %v", got, want)
	}

	got, err = nextClockOccurrence(now, "02:00")
	if err != nil {
		t.Fatalf("next occurrence: %v", err)
	}
	if want := time.Date(2026, 11, 3, 2, 0, 0, 0, loc); !got.Equal(want) {
		t.Errorf("next-day = %v, want %v", got, want)
	}

	if _, err := nextClockOccurrence(now, "3am"); err == nil {
		t.Error("bad clock accepted")
	}
}

func TestAdvanceRebootSkipsMissedPeriods(t *testing.T) {
	due := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	now := time.Date(2026, 11, 2, 4, 0, 0, 0, time.UTC)
	next := advanceReboot(due, 1, now)
	if next.Hour() != 3 || !next.After(now) {
		t.Errorf("next = %v, want 03:00 after %v", next, now)
	}
	if next.Sub(now) > 24*time.Hour {
		t.Errorf("next = %v, want within one period", next)
	}
}
