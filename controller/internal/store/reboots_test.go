package store

import (
	"testing"
	"time"
)

// A schedule must not fire at an AP the controller cannot reach: the queued
// reboot would age out before the device ever sees it, and the due time is
// already advanced by then.
func TestDueRebootsRequiresOnlineDevice(t *testing.T) {
	st, ctx := newActionsStore(t)
	const mac = "8c:de:52:11:22:33"
	now := time.Now().Unix()

	if err := st.UpsertDiscovered(ctx, Device{MAC: mac, Model: "XR1710G"}); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE devices SET state = ?, last_seen = ? WHERE mac = ?`,
		StateAdopted, now, mac); err != nil {
		t.Fatalf("adopt device: %v", err)
	}
	if err := st.SetRebootSchedule(ctx, mac, 1, "03:00", now-60); err != nil {
		t.Fatalf("set schedule: %v", err)
	}

	due, err := st.DueReboots(ctx, now, 90, 10)
	if err != nil {
		t.Fatalf("due reboots: %v", err)
	}
	if len(due) != 1 || due[0].MAC != mac {
		t.Fatalf("due = %+v, want the online device", due)
	}

	// Silent for an hour: not a candidate any more.
	if _, err := st.db.ExecContext(ctx,
		`UPDATE devices SET last_seen = ? WHERE mac = ?`, now-3600, mac); err != nil {
		t.Fatalf("age device: %v", err)
	}
	if due, _ := st.DueReboots(ctx, now, 90, 10); len(due) != 0 {
		t.Errorf("offline device still due: %+v", due)
	}

	// Disabling the policy clears the due time.
	if err := st.SetRebootSchedule(ctx, mac, 0, "", 0); err != nil {
		t.Fatalf("disable schedule: %v", err)
	}
	if due, _ := st.DueReboots(ctx, now, 90, 10); len(due) != 0 {
		t.Errorf("disabled schedule still due: %+v", due)
	}
}

func TestHasPendingAction(t *testing.T) {
	st, ctx := newActionsStore(t)
	const mac = "8c:de:52:11:22:33"

	has, err := st.HasPendingAction(ctx, mac, "reboot")
	if err != nil {
		t.Fatalf("check pending: %v", err)
	}
	if has {
		t.Fatal("pending found before any action was created")
	}

	a, err := st.CreateAction(ctx, mac, "reboot", nil)
	if err != nil {
		t.Fatalf("create action: %v", err)
	}
	if has, _ = st.HasPendingAction(ctx, mac, "reboot"); !has {
		t.Error("pending reboot not detected")
	}
	if has, _ = st.HasPendingAction(ctx, mac, "wifi_reload"); has {
		t.Error("unrelated action type counted as pending reboot")
	}

	if _, err := st.CompleteAction(ctx, mac, a.ID, ActionDone, ""); err != nil {
		t.Fatalf("complete action: %v", err)
	}
	if has, _ = st.HasPendingAction(ctx, mac, "reboot"); has {
		t.Error("completed action still counted as pending")
	}
}
