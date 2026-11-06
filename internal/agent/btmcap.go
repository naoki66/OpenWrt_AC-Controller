package agent

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Client capability gating for BTM.
//
// The neighbour list a BTM carries is a sequence of 802.11k Neighbor Report
// elements. A station that does not implement 11k cannot parse them, so for
// that station the request is not "advice it chose to ignore" — it is advice it
// cannot read. The live site is mostly in that state: of thirteen associated
// stations, eleven reported rrm=[0,0,0,0,0] with no MBO, including
// 02:00:00:00:00:9a, the client that ignored every suggestion for hours.
//
// hostapd already knows this. `get_clients` carries each station's RM Enabled
// Capabilities bitmap and MBO flag, both copied from its association request.
// Consulting it before the first suggestion turns "send three BTMs, wait for
// them to be ignored, then stay quiet for half an hour" into "do not send one".
//
// The check is deliberately at the decision point and cached per association:
// one ubus call per weak client, not one per tick, and none at all for the
// clients that never reach a suggestion.

const (
	// btmCapTTL bounds how long a capability verdict is trusted. Stations do
	// not change their advertised capabilities while associated, so this only
	// exists to keep the table from growing without bound on a busy AP.
	btmCapTTL = 30 * time.Minute
	// btmCapLimit bounds the cache.
	btmCapLimit = 256
)

// stationCaps is the slice of `hostapd.<bss> get_clients` the gate needs.
type stationCaps struct {
	RRM []int `json:"rrm"`
	MBO bool  `json:"mbo"`
}

type clientsReply struct {
	Clients map[string]stationCaps `json:"clients"`
}

// capVerdict is a cached answer, tied to one association.
type capVerdict struct {
	supports bool
	assocAt  time.Time
	at       time.Time
}

// btmCapCache remembers capability verdicts per client.
type btmCapCache struct {
	mu    sync.Mutex
	items map[string]capVerdict
}

func newBTMCapCache() *btmCapCache {
	return &btmCapCache{items: map[string]capVerdict{}}
}

func (c *btmCapCache) get(mac string, assocAt time.Time, now time.Time) (bool, bool) {
	if c == nil {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[mac]
	if !ok || now.Sub(v.at) > btmCapTTL {
		return false, false
	}
	// A new association may present a different capability set (a reboot into
	// a different firmware, a different link). Do not carry the old verdict.
	if !assocAt.IsZero() && !v.assocAt.IsZero() && !assocAt.Equal(v.assocAt) {
		return false, false
	}
	return v.supports, true
}

func (c *btmCapCache) put(mac string, assocAt time.Time, supports bool, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.items) >= btmCapLimit {
		for k, v := range c.items {
			if now.Sub(v.at) > btmCapTTL {
				delete(c.items, k)
			}
		}
		if len(c.items) >= btmCapLimit {
			c.items = map[string]capVerdict{}
		}
	}
	c.items[mac] = capVerdict{supports: supports, assocAt: assocAt, at: now}
}

// stationSupportsBTM reports whether a station can be expected to act on a BTM
// that carries Neighbor Report elements.
//
// The test is deliberately generous: any RM capability bit at all, or MBO,
// counts as "yes". Every capability in that bitmap implies the station parses
// management elements from the 11k/11v family, and a false "no" would silently
// disable steering for a client that would have moved.
func stationSupportsBTM(c stationCaps) bool {
	if c.MBO {
		return true
	}
	for _, v := range c.RRM {
		if v != 0 {
			return true
		}
	}
	return false
}

// clientSupportsBTM asks hostapd what the station can do.
//
// known is false whenever the answer could not be obtained — no ubus, a parse
// failure, or the station is not in the reply. Callers must treat unknown as
// "go ahead": refusing to steer on a failed lookup would let a transient ubus
// error silence the loop.
func (s *SteerLoop) clientSupportsBTM(ctx context.Context, ifname, mac string, assocAt time.Time) (supports bool, known bool) {
	if s.Agent == nil || s.Agent.Runner == nil || ifname == "" || mac == "" {
		return true, false
	}
	now := s.Now()
	if v, ok := s.caps.get(mac, assocAt, now); ok {
		return v, true
	}
	out, err := s.Agent.Runner.Run(ctx, "ubus", "call", "hostapd."+ifname, "get_clients")
	if err != nil {
		return true, false
	}
	var reply clientsReply
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		return true, false
	}
	caps, ok := reply.Clients[mac]
	if !ok {
		// hostapd lists associated stations by lower-case MAC; a miss means the
		// station left between the sample and this call.
		return true, false
	}
	supports = stationSupportsBTM(caps)
	s.caps.put(mac, assocAt, supports, now)
	return supports, true
}
