package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newActionsStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "ac.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st, ctx
}

func TestActionLifecycle(t *testing.T) {
	st, ctx := newActionsStore(t)
	const mac = "8c:de:52:11:22:33"

	a, err := st.CreateAction(ctx, mac, "wifi_reload", map[string]string{"ifname": "phy0.1-ap0"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(a.ID) != 16 || a.State != ActionPending {
		t.Fatalf("action = %+v, want 16-char pending id", a)
	}

	pending, err := st.PendingActions(ctx, mac, 8)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != a.ID {
		t.Fatalf("pending = %+v, want the new action", pending)
	}
	if pending[0].Args["ifname"] != "phy0.1-ap0" {
		t.Errorf("args = %v, want ifname", pending[0].Args)
	}

	if err := st.MarkActionsDispatched(ctx, []string{a.ID}); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}
	again, err := st.PendingActions(ctx, mac, 8)
	if err != nil {
		t.Fatalf("pending after dispatch: %v", err)
	}
	if len(again) != 1 {
		t.Fatalf("dispatched action must stay pending, got %+v", again)
	}

	ok, err := st.CompleteAction(ctx, mac, a.ID, ActionDone, "ok")
	if err != nil || !ok {
		t.Fatalf("complete = %v/%v, want true", ok, err)
	}
	if pending, _ := st.PendingActions(ctx, mac, 8); len(pending) != 0 {
		t.Errorf("pending after result = %+v, want none", pending)
	}
	if _, err := st.CompleteAction(ctx, mac, a.ID, ActionDone, "ok"); err != nil {
		t.Errorf("duplicate result should be tolerated, got %v", err)
	}
	if _, err := st.CompleteAction(ctx, mac, a.ID, "bogus", ""); err == nil {
		t.Error("invalid result state must be rejected")
	}

	list, err := st.ListActions(ctx, mac, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].State != ActionDone || list[0].Message != "ok" {
		t.Fatalf("list = %+v, want the done action", list)
	}
}

// An action that was handed to an offline-then-silent AP must not stay pending
// forever: after the dispatch deadline it is expired and no longer re-sent.
func TestPendingActionsExpire(t *testing.T) {
	st, ctx := newActionsStore(t)
	const mac = "8c:de:52:11:22:33"

	a, err := st.CreateAction(ctx, mac, "reboot", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.MarkActionsDispatched(ctx, []string{a.ID}); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}
	old := time.Now().Unix() - int64(ActionDispatchTimeout.Seconds()) - 60
	if _, err := st.db.ExecContext(ctx,
		`UPDATE device_actions SET dispatched_at = ? WHERE id = ?`, old, a.ID); err != nil {
		t.Fatalf("age action: %v", err)
	}

	pending, err := st.PendingActions(ctx, mac, 8)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expired action still pending: %+v", pending)
	}
	list, _ := st.ListActions(ctx, mac, 10)
	if len(list) != 1 || list[0].State != ActionExpired {
		t.Fatalf("list = %+v, want expired", list)
	}
}
