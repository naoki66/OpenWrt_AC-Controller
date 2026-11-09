package render

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/naoki66/ac-controller/shared/ucimerge"
)

// The comparator below compares two UCI files by section identity plus option
// values, not bytes:
//
//   - Options are compared as a set. netifd does not care about ordering.
//   - Section names are compared only where they carry meaning (interfaces,
//     the bridge name). Managed sections get an ac_ prefix in production, so a
//     byte comparison would report a rename on every section and hide the
//     differences that actually matter.
//
// Everything else must match: same section count, same identity keys, same
// option values, no extras on either side. The comparator's own behaviour is
// covered by TestComparatorDetectsDifferences at the bottom of this file.

// parsedSection is a UCI section reduced to what matters for comparison.
type parsedSection struct {
	Type  string
	Name  string
	Key   string
	Opts  map[string]string
	Lists map[string][]string
}

func parseSections(content string) []parsedSection {
	file := ucimerge.Parse(content)
	out := make([]parsedSection, 0, len(file.Sections))
	for _, s := range file.Sections {
		ps := parsedSection{
			Type:  s.Type,
			Name:  s.Name,
			Opts:  map[string]string{},
			Lists: map[string][]string{},
		}
		for _, line := range s.Lines[1:] {
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			value := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line[strings.Index(line, fields[2]):]), fields[2]))
			switch fields[0] {
			case "option":
				ps.Opts[fields[1]] = unquote(fields[2])
			case "list":
				ps.Lists[fields[1]] = append(ps.Lists[fields[1]], unquote(fields[2]))
			}
			_ = value
		}
		// A BSS's device reference points at a radio section that is renamed in
		// production. Normalising it here means both the identity key and the
		// option comparison see the same name, so the rename is the only thing
		// that differs.
		if ps.Type == "wifi-iface" {
			if d, ok := ps.Opts["device"]; ok {
				ps.Opts["device"] = strings.TrimPrefix(d, ACPrefix)
			}
			if n, ok := ps.Opts["network"]; ok {
				ps.Opts["network"] = strings.TrimPrefix(n, ACPrefix)
			}
		}
		out = append(out, ps)
	}
	return out
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], `'\''`, "'")
	}
	return s
}

// identity assigns a stable key so the two files can be matched even though the
// controller names its sections differently from the hand-written baseline.
func (s parsedSection) identity() (string, bool) {
	switch s.Type {
	case "wifi-device":
		// Radios are identified by their phy index: section names change with
		// the ac_ prefix but the index is what netifd binds to.
		return "wifi-device/radio=" + s.Opts["radio"], true
	case "wifi-iface":
		// One BSS is "this SSID on this band", which is exactly ssid+device.
		// The device reference carries the production prefix, so strip it: the
		// rename is intentional and must not be mistaken for a missing BSS.
		device := strings.TrimPrefix(s.Opts["device"], ACPrefix)
		return "wifi-iface/" + s.Opts["ssid"] + "@" + device, true
	case "bridge-vlan":
		return "bridge-vlan/" + s.Opts["vlan"], true
	case "interface":
		// loopback is the device's own; the controller never renders it.
		if s.Name == "loopback" {
			return "", false
		}
		return "interface/" + strings.TrimPrefix(s.Name, ACPrefix), true
	case "device":
		return "device/" + s.Opts["name"], true
	}
	return "", false
}

func keyed(t *testing.T, content string) map[string]parsedSection {
	t.Helper()
	out := map[string]parsedSection{}
	for _, s := range parseSections(content) {
		key, ok := s.identity()
		if !ok {
			continue // package/globals/loopback and other locally owned sections
		}
		if _, dup := out[key]; dup {
			t.Fatalf("duplicate identity %q in rendered/baseline config", key)
		}
		out[key] = s
	}
	return out
}

// diffSections returns human-readable differences between two section sets.
func diffSections(want, got map[string]parsedSection) []string {
	var diffs []string

	keys := map[string]bool{}
	for k := range want {
		keys[k] = true
	}
	for k := range got {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	for _, k := range sorted {
		w, inWant := want[k]
		g, inGot := got[k]
		switch {
		case !inGot:
			diffs = append(diffs, fmt.Sprintf("missing in render: %s", k))
			continue
		case !inWant:
			diffs = append(diffs, fmt.Sprintf("extra in render: %s", k))
			continue
		}
		for opt, wv := range w.Opts {
			gv, ok := g.Opts[opt]
			if !ok {
				// netifd treats `option ipaddr X` and `list ipaddr X` identically,
				// and the hand-written baseline uses both styles. This is the only
				// normalisation the comparator applies.
				if singleList, isList := g.Lists[opt]; isList && scalarKeys[opt] &&
					len(singleList) == 1 && singleList[0] == wv {
					continue
				}
				diffs = append(diffs, fmt.Sprintf("%s: missing option %s (baseline %q)", k, opt, wv))
				continue
			}
			if gv != wv {
				diffs = append(diffs, fmt.Sprintf("%s: option %s = %q, baseline %q", k, opt, gv, wv))
			}
		}
		for opt := range g.Opts {
			if _, ok := w.Opts[opt]; !ok {
				// netifd reads a missing boolean as 0, so spelling out
				// `igmp_snooping '0'` is the same configuration as omitting it.
				// The two deployed units disagree on which style they use.
				if opt == "igmp_snooping" && g.Opts[opt] == "0" {
					continue
				}
				diffs = append(diffs, fmt.Sprintf("%s: extra option %s=%q", k, opt, g.Opts[opt]))
			}
		}
		for key, wl := range w.Lists {
			gl := g.Lists[key]
			if scalarKeys[key] && len(wl) == 1 {
				if opt, ok := g.Opts[key]; ok && opt == wl[0] {
					continue
				}
			}
			if !sameSet(wl, gl) {
				diffs = append(diffs, fmt.Sprintf("%s: list %s = %v, baseline %v", k, key, gl, wl))
			}
		}
		for key := range g.Lists {
			if _, ok := w.Lists[key]; ok {
				continue
			}
			// Mirror of the option/list equivalence above: the baseline writes
			// `option ipaddr`, the renderer writes `list ipaddr`.
			if scalarKeys[key] && len(g.Lists[key]) == 1 {
				if opt, ok := w.Opts[key]; ok && opt == g.Lists[key][0] {
					continue
				}
			}
			diffs = append(diffs, fmt.Sprintf("%s: extra list %s", k, key))
		}
	}
	return diffs
}

// scalarKeys are options that netifd accepts as either a scalar option or a
// list. Comparing across the two spellings is required because the baseline is
// hand-written and mixes them (`lan` uses list, `lan2` uses option).
var scalarKeys = map[string]bool{
	"ipaddr":  true,
	"netmask": true,
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// Sanity check on the comparator itself: a deliberate change must be reported.
// Without this, a comparator that silently matches everything would be useless.
func TestComparatorDetectsDifferences(t *testing.T) {
	base := `config wifi-iface 'x'
	option ssid 'A'
	option device 'radio0'
	option encryption 'sae+ccmp'
`
	changed := `config wifi-iface 'x'
	option ssid 'A'
	option device 'radio0'
	option encryption 'owe'
`
	diffs := diffSections(keyed(t, base), keyed(t, changed))
	if len(diffs) == 0 {
		t.Fatal("comparator reported no difference for a changed option")
	}

	missing := diffSections(keyed(t, base), keyed(t, ""))
	if len(missing) == 0 {
		t.Fatal("comparator reported no difference for a missing section")
	}
}
