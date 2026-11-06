package api

import (
	"testing"
	"time"

	"github.com/naoki66/ac-controller/internal/store"
)

// The failure this guards against: an AP that was reflashed stopped sending
// hellos, but its row still says "adopted" and the device list kept reporting it
// as managed. Adoption and presence are independent axes.
func TestAnnotatePresence(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := &Server{
		OfflineAfter: 90 * time.Second,
		Now:          func() time.Time { return now },
	}

	cases := []struct {
		name     string
		lastSeen int64
		wantOn   bool
		wantAge  int64
	}{
		{name: "just reported", lastSeen: now.Unix() - 5, wantOn: true, wantAge: 5},
		{name: "one heartbeat late", lastSeen: now.Unix() - 21, wantOn: true, wantAge: 21},
		{name: "exactly at the limit", lastSeen: now.Unix() - 90, wantOn: true, wantAge: 90},
		{name: "past the limit", lastSeen: now.Unix() - 91, wantOn: false, wantAge: 91},
		{name: "hours ago", lastSeen: now.Unix() - 4625, wantOn: false, wantAge: 4625},
		{name: "never seen", lastSeen: 0, wantOn: false, wantAge: now.Unix()},
		{name: "clock ran ahead", lastSeen: now.Unix() + 120, wantOn: true, wantAge: 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			devices := []store.Device{{MAC: "00:00:00:00:00:5a", State: store.StateAdopted, LastSeen: c.lastSeen}}
			s.annotatePresence(devices)

			if devices[0].Online != c.wantOn {
				t.Errorf("online = %v, want %v", devices[0].Online, c.wantOn)
			}
			if devices[0].LastSeenAge != c.wantAge {
				t.Errorf("age = %d, want %d", devices[0].LastSeenAge, c.wantAge)
			}
			// Adoption must never be touched by presence.
			if devices[0].State != store.StateAdopted {
				t.Errorf("state was modified: %q", devices[0].State)
			}
		})
	}
}

// Zero means "use the default", so a deployment that never sets
// AC_OFFLINE_AFTER still gets a sane answer instead of "everything offline".
func TestOfflineAfterDefault(t *testing.T) {
	s := &Server{}
	if got := s.offlineAfter(); got != DefaultOfflineAfter {
		t.Errorf("offlineAfter = %v, want %v", got, DefaultOfflineAfter)
	}

	s.OfflineAfter = 5 * time.Minute
	if got := s.offlineAfter(); got != 5*time.Minute {
		t.Errorf("offlineAfter = %v, want 5m", got)
	}
}

// A custom threshold must be honoured: a site with a 5-minute heartbeat cannot
// use the 90s default without declaring everything offline.
func TestAnnotatePresenceHonoursThreshold(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := &Server{OfflineAfter: 5 * time.Minute, Now: func() time.Time { return now }}

	devices := []store.Device{{LastSeen: now.Unix() - 200}} // 3m20s
	s.annotatePresence(devices)

	if !devices[0].Online {
		t.Error("a device inside a 5 minute window must be online")
	}
}
