package store

import (
	"context"
	"testing"
	"time"
)

func seedAdoptedDevice(t *testing.T, st *Store, mac string, now int64) {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertDiscovered(ctx, Device{MAC: mac, Model: "XR1710G"}); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE devices SET state = ?, last_seen = ? WHERE mac = ?`,
		StateAdopted, now, mac); err != nil {
		t.Fatalf("adopt device: %v", err)
	}
}

func TestDueLEDSchedulesRequiresOnlineDevice(t *testing.T) {
	st, ctx := newActionsStore(t)
	const mac = "8c:de:52:11:22:33"
	now := time.Now().Unix()
	seedAdoptedDevice(t, st, mac, now)

	if err := st.SetLEDSchedule(ctx, mac, "07:00", "23:00", now-60, now+3600); err != nil {
		t.Fatalf("set LED schedule: %v", err)
	}
	due, err := st.DueLEDSchedules(ctx, now, 90, 10)
	if err != nil {
		t.Fatalf("due LED schedules: %v", err)
	}
	if len(due) != 1 || due[0].MAC != mac || due[0].LEDOnAt != "07:00" || due[0].LEDOffAt != "23:00" {
		t.Fatalf("due = %+v, want the online device", due)
	}

	// Silent for an hour: the queued action would expire before the AP saw it.
	if _, err := st.db.ExecContext(ctx,
		`UPDATE devices SET last_seen = ? WHERE mac = ?`, now-3600, mac); err != nil {
		t.Fatalf("age device: %v", err)
	}
	if due, _ := st.DueLEDSchedules(ctx, now, 90, 10); len(due) != 0 {
		t.Errorf("offline device still due: %+v", due)
	}

	// Clearing both edges disables the schedule.
	if err := st.SetLEDSchedule(ctx, mac, "", "", 0, 0); err != nil {
		t.Fatalf("disable LED schedule: %v", err)
	}
	d, err := st.GetDevice(ctx, mac)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if d.LEDOnAt != "" || d.LEDOffAt != "" || d.LEDOnNextAt != 0 || d.LEDOffNextAt != 0 {
		t.Errorf("cleared schedule = %+v", d)
	}
}

func TestAdvanceLEDScheduleOnlySelectedEdge(t *testing.T) {
	st, ctx := newActionsStore(t)
	const mac = "8c:de:52:11:22:33"
	now := time.Now().Unix()
	seedAdoptedDevice(t, st, mac, now)

	if err := st.SetLEDSchedule(ctx, mac, "07:00", "23:00", now-60, now-120); err != nil {
		t.Fatalf("set LED schedule: %v", err)
	}
	next := now + 3600
	if err := st.AdvanceLEDSchedule(ctx, mac, true, next); err != nil {
		t.Fatalf("advance on edge: %v", err)
	}

	d, err := st.GetDevice(ctx, mac)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if d.LEDOnNextAt != next {
		t.Errorf("on next = %d, want %d", d.LEDOnNextAt, next)
	}
	if d.LEDOffNextAt != now-120 {
		t.Errorf("off next = %d, want untouched %d", d.LEDOffNextAt, now-120)
	}

	if err := st.AdvanceLEDSchedule(ctx, mac, false, next+60); err != nil {
		t.Fatalf("advance off edge: %v", err)
	}
	d, _ = st.GetDevice(ctx, mac)
	if d.LEDOffNextAt != next+60 || d.LEDOnNextAt != next {
		t.Errorf("after off advance = %+v", d)
	}
}
