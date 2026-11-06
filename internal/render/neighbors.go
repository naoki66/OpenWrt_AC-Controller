// 11k neighbour lists: the controller's answer to a gap the access points
// cannot fill on their own.
//
// hostapd knows how to put a Neighbor Report in the air, but it only advertises
// what it is told to advertise: rrm_nr_list starts empty on every BSS and stays
// empty unless something feeds it. Projects that solve this (nrsyncd, DAWN)
// install a daemon on each AP so the APs gossip with each other. This controller
// already receives every BSS identity from every adopted AP, so it can build the
// same data centrally and hand each AP only its own list — the design premise
// (control plane central, data plane local) applied to 802.11k.
package render

import (
	"encoding/json"
	"sort"
)

// NeighborEntry is one BSS of one AP as that AP reported it. Report is opaque.
type NeighborEntry struct {
	APMAC  string
	Ifname string
	BSSID  string
	SSID   string
	Report string
}

// Neighbors renders the per-interface list for the AP identified by selfAP.
//
// A BSS only advertises siblings that serve the same SSID: a station associated
// to SSID-A must not be offered a neighbour that only exists on SSID-A_6G, because
// the transition request would fail and the client learns the list is junk.
// Its own entry is excluded — hostapd appends that itself when it answers a
// Neighbor Report request.
//
// Returns nil when there is nothing to advertise, which is the normal case for
// the first AP on a network; the caller then leaves the file out entirely and
// the agent never touches hostapd.
func Neighbors(entries []NeighborEntry, selfAP string) []byte {
	bySSID := make(map[string][]NeighborEntry, len(entries))
	for _, e := range entries {
		bySSID[e.SSID] = append(bySSID[e.SSID], e)
	}

	out := make(map[string][][]string)
	for _, self := range entries {
		if self.APMAC != selfAP {
			continue
		}
		var list [][]string
		for _, peer := range bySSID[self.SSID] {
			if peer.BSSID == self.BSSID {
				continue
			}
			list = append(list, []string{peer.BSSID, peer.SSID, peer.Report})
		}
		if len(list) == 0 {
			continue
		}
		// Deterministic order: two APs must never produce a different package
		// for the same inputs, or the revision churns on nothing.
		sort.Slice(list, func(i, j int) bool { return list[i][0] < list[j][0] })
		out[self.Ifname] = list
	}
	if len(out) == 0 {
		return nil
	}
	// encoding/json sorts map keys, so the output is stable across runs.
	blob, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return append(blob, '\n')
}
