package store

import (
	"testing"
)

func TestGroupCRUDAndMembership(t *testing.T) {
	st, ctx := newActionsStore(t)
	const mac = "8c:de:52:11:22:33"

	if err := st.UpsertDiscovered(ctx, Device{MAC: mac, Model: "XR1710G"}); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	g, err := st.CreateGroup(ctx, "一层办公区", "default")
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if len(g.ID) != 16 || g.TemplateID != "default" {
		t.Fatalf("group = %+v, want a 16-char id bound to default", g)
	}

	list, err := st.ListGroups(ctx)
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	if len(list) != 1 || list[0].DeviceCount != 0 {
		t.Fatalf("groups = %+v, want one empty group", list)
	}

	if err := st.SetDeviceGroup(ctx, mac, g.ID); err != nil {
		t.Fatalf("bind device: %v", err)
	}
	got, err := st.GetGroup(ctx, g.ID)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if got.DeviceCount != 1 {
		t.Errorf("device_count = %d, want 1", got.DeviceCount)
	}
	d, err := st.GetDevice(ctx, mac)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if d.GroupID != g.ID {
		t.Errorf("group_id = %q, want %q", d.GroupID, g.ID)
	}

	if err := st.UpdateGroup(ctx, g.ID, "二层办公区", "corp"); err != nil {
		t.Fatalf("update group: %v", err)
	}
	got, _ = st.GetGroup(ctx, g.ID)
	if got.Name != "二层办公区" || got.TemplateID != "corp" {
		t.Errorf("after update = %+v", got)
	}

	// Deleting while members remain is an API policy, but the store must still
	// report the membership so the handler can refuse.
	if n, err := st.CountGroupDevices(ctx, g.ID); err != nil || n != 1 {
		t.Errorf("CountGroupDevices = %d, %v; want 1, nil", n, err)
	}

	if err := st.DeleteGroup(ctx, "nope"); err != ErrNotFound {
		t.Errorf("delete missing group = %v, want ErrNotFound", err)
	}
	if err := st.SetDeviceGroup(ctx, "aa:bb:cc:dd:ee:ff", g.ID); err != ErrNotFound {
		t.Errorf("bind missing device = %v, want ErrNotFound", err)
	}
}

func TestCreateGroupRequiresName(t *testing.T) {
	st, ctx := newActionsStore(t)
	if _, err := st.CreateGroup(ctx, "", "default"); err == nil {
		t.Fatal("empty name accepted")
	}
	if err := st.UpdateGroup(ctx, "nope", "", "default"); err == nil {
		t.Fatal("empty name accepted on update")
	}
}
