package agent

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Candidate quality filtering.
//
// A BTM is only worth sending if the client has somewhere better to go. Until
// now the loop offered every known BSS on the SSID, including the one the
// client already sits on and including BSSes on the very same channel — a
// "roam" between two BSSes sharing an operating class and channel keeps the
// client on the same interfered airtime while costing it a reassociation.
//
// The 802.11k Neighbor Report body carries exactly what is needed and the
// agent already holds it: hostapd's `rrm_nr_get_own` returns the body with the
// Element ID and Length stripped, so the fixed part starts at offset 0.
//
// What this cannot do is measure the candidate. Beacon report requests were
// evaluated and rejected: hostapd refuses them for clients that do not declare
// active beacon measurement ("does not support active beacon report", observed
// on 10.0.30.250 for the only client on the site that advertised it), so the
// data would simply never arrive. Ranking by band and channel uses what the
// APs already know instead of waiting for measurements that will not come.

// Offsets into the Neighbor Report body (Element ID and Length excluded):
//
//	BSSID (6) | BSSID Info (4) | Operating Class (1) | Channel (1) | PHY Type (1)
//
// followed by optional subelements, which are not interpreted here.
const (
	nrBSSIDLen   = 6
	nrBodyMinLen = 13
	nrOpClassOff = 10
	nrChannelOff = 11
	nrPHYOff     = 12
)

// neighborInfo is the fixed part of a Neighbor Report body.
type neighborInfo struct {
	BSSID   string
	OpClass uint8
	Channel uint8
	PHYType uint8
}

// valid reports whether the struct carries a real operating point. A zero
// value means "this BSS's own report has not arrived yet", which the caller
// must treat as "unknown", not as channel 0 of class 0.
func (n neighborInfo) valid() bool {
	return n.BSSID != "" && n.OpClass != 0
}

// parseNeighborReport decodes one report body as hostapd stores it.
func parseNeighborReport(body string) (neighborInfo, bool) {
	raw, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(body)))
	if err != nil || len(raw) < nrBodyMinLen {
		return neighborInfo{}, false
	}
	bssid := make([]string, nrBSSIDLen)
	for i := 0; i < nrBSSIDLen; i++ {
		bssid[i] = fmt.Sprintf("%02x", raw[i])
	}
	return neighborInfo{
		BSSID:   strings.Join(bssid, ":"),
		OpClass: raw[nrOpClassOff],
		Channel: raw[nrChannelOff],
		PHYType: raw[nrPHYOff],
	}, true
}

// bandOfOpClass maps a global operating class to its band. Only the classes
// that appear in a 2.4/5/6 GHz deployment are covered; anything else returns
// "" and is treated as unknown rather than guessed at.
func bandOfOpClass(op uint8) string {
	switch {
	case op >= 81 && op <= 84:
		return "2g"
	case op >= 115 && op <= 130:
		return "5g"
	case op >= 131 && op <= 137:
		return "6g"
	}
	return ""
}

// bandRank orders the bands by capacity, which is what makes a cross-band
// suggestion worth taking. A 5 GHz client is better off on 6 GHz than dropping
// to 2.4 GHz even if the 2.4 GHz candidate happens to use a lower channel
// number, so channel alone is the wrong tie-break.
func bandRank(op uint8) int {
	switch bandOfOpClass(op) {
	case "6g":
		return 3
	case "5g":
		return 2
	case "2g":
		return 1
	}
	return 0
}

// ranked is one candidate with the sort keys derived from its report.
type ranked struct {
	entry     []string
	sameBand  bool
	coChannel bool
	band      int
	channel   uint8
	order     int
}

// rankCandidates removes candidates that cannot be an improvement and orders
// the rest, same band first and then by channel. Entries are returned in their
// original [bssid, ssid, report] shape so the BTM path is unchanged.
//
// A candidate whose report cannot be parsed is kept and treated as same-band:
// dropping it would silently shrink the candidate set on a driver or hostapd
// build that encodes reports differently, which is a worse failure than
// offering a mediocre option.
func rankCandidates(cands [][]string, self neighborInfo) [][]string {
	return rankCandidateList(cands, self, false)
}

func rankCandidatesForAdvice(cands [][]string, self neighborInfo) [][]string {
	return rankCandidateList(cands, self, true)
}

func rankCandidateList(cands [][]string, self neighborInfo, allowCoChannel bool) [][]string {
	if len(cands) == 0 {
		return nil
	}
	selfBand := bandOfOpClass(self.OpClass)

	out := make([]ranked, 0, len(cands))
	seen := make(map[string]bool, len(cands))
	for i, entry := range cands {
		if len(entry) < 3 || entry[0] == "" || entry[2] == "" {
			continue
		}
		key := strings.ToLower(entry[0])
		if seen[key] {
			continue
		}
		seen[key] = true
		if self.valid() && strings.EqualFold(entry[0], self.BSSID) {
			// Never offer the BSS the client is already on.
			continue
		}
		info, ok := parseNeighborReport(entry[2])
		r := ranked{entry: entry, sameBand: true, order: i}
		if ok {
			r.channel = info.Channel
			r.band = bandRank(info.OpClass)
			if selfBand != "" {
				if b := bandOfOpClass(info.OpClass); b != "" {
					r.sameBand = b == selfBand
				}
			}
			// Same class and channel: the client would keep the same airtime
			// and the same interference, and pay a reassociation for it.
			r.coChannel = self.valid() && info.OpClass == self.OpClass && info.Channel == self.Channel
			if r.coChannel && !allowCoChannel {
				continue
			}
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].coChannel != out[j].coChannel {
			return !out[i].coChannel
		}
		if out[i].sameBand != out[j].sameBand {
			return out[i].sameBand
		}
		if out[i].band != out[j].band {
			return out[i].band > out[j].band
		}
		if out[i].channel != out[j].channel {
			return out[i].channel < out[j].channel
		}
		return out[i].order < out[j].order
	})
	result := make([][]string, 0, len(out))
	for _, r := range out {
		result = append(result, r.entry)
	}
	return result
}
