package api

import (
	"time"

	"github.com/naoki66/ac-controller/controller/internal/store"
)

// DefaultOfflineAfter is how long an AP may stay silent before the UI reports it
// offline.
//
// Ninety seconds is three heartbeats (the agent sends one every 20s) plus half a
// beat of margin: short enough that a reflash or a dead agent is visible within a
// minute, long enough that one dropped packet does not flap the device list.
const DefaultOfflineAfter = 90 * time.Second

func (s *Server) offlineAfter() time.Duration {
	if s.OfflineAfter > 0 {
		return s.OfflineAfter
	}
	return DefaultOfflineAfter
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// annotatePresence fills the derived presence fields on the way out of the API
// instead of storing them. Storing "online" would require something to write it
// back when the last heartbeat ages out, and nothing does: the device is simply
// silent. Deriving it means a threshold change, a clock correction or a restored
// database all take effect on the next request with no migration.
func (s *Server) annotatePresence(devices []store.Device) {
	limit := int64(s.offlineAfter() / time.Second)
	now := s.now().Unix()

	for i := range devices {
		age := now - devices[i].LastSeen
		if age < 0 {
			// A device whose clock ran ahead of ours is not "offline since the
			// future"; clamp so the UI cannot print a negative duration.
			age = 0
		}
		devices[i].LastSeenAge = age
		devices[i].Online = devices[i].LastSeen > 0 && age <= limit
	}
}
