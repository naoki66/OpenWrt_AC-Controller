// Package ucimerge applies controller-owned sections to an existing OpenWrt
// configuration file without touching anything the controller does not own.
//
// This is the single most dangerous piece of the agent: replacing
// /etc/config/network wholesale would remove the uplink and lock the operator
// out of the access point. The merge therefore works at section granularity and
// only ever replaces sections whose name carries the ac_ prefix.
package ucimerge

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ACPrefix marks every section the controller owns. The AP side uses it both to
// identify what it may replace and to keep locally authored config intact
// (design §6.2, §8.1).
const ACPrefix = "ac_"

// Section is one `config <type> '<name>'` block, kept as raw lines so that
// formatting and unknown options round-trip untouched.
type Section struct {
	Type string
	Name string
	// Lines holds the whole block including its header line.
	Lines []string
}

// File is a parsed UCI config.
type File struct {
	// Preamble holds comments and blank lines before the first section.
	Preamble []string
	Sections []Section
}

var headerRe = regexp.MustCompile(`^\s*config\s+([A-Za-z0-9_-]+)(?:\s+(?:'([^']*)'|"([^"]*)"|([^\s#]+)))?\s*(?:#.*)?$`)

// Parse splits UCI text into sections. It is deliberately permissive: anything
// that is not a section header is treated as part of the surrounding block
// rather than as an error, because a parse failure here means "refuse to apply"
// and that would block provisioning on a valid-but-unusual file.
func Parse(content string) File {
	var f File
	var current *Section

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if m := headerRe.FindStringSubmatch(trimmed); m != nil {
			f.Sections = append(f.Sections, Section{
				Type:  m[1],
				Name:  firstNonEmpty(m[2], m[3], m[4]),
				Lines: []string{line},
			})
			current = &f.Sections[len(f.Sections)-1]
			continue
		}
		if current == nil {
			f.Preamble = append(f.Preamble, line)
			continue
		}
		current.Lines = append(current.Lines, line)
	}
	return f
}

// IsAC reports whether the section is owned by the controller. Anonymous
// sections are never ours: they have no name to match.
func (s Section) IsAC() bool {
	return strings.HasPrefix(s.Name, ACPrefix)
}

// Render serialises the file back to UCI text.
func (f File) Render() string {
	var b strings.Builder

	pre := f.Preamble
	for len(pre) > 0 && strings.TrimSpace(pre[len(pre)-1]) == "" {
		pre = pre[:len(pre)-1]
	}
	for _, line := range pre {
		b.WriteString(strings.TrimRight(line, " \t"))
		b.WriteByte('\n')
	}
	if len(pre) > 0 && len(f.Sections) > 0 {
		b.WriteByte('\n')
	}

	for i, s := range f.Sections {
		if i > 0 {
			b.WriteByte('\n')
		}
		body := s.Lines
		// Drop trailing blank lines so blocks are separated by exactly one
		// blank line; the renderer must stay byte-stable across runs.
		for len(body) > 1 && strings.TrimSpace(body[len(body)-1]) == "" {
			body = body[:len(body)-1]
		}
		for _, line := range body {
			b.WriteString(strings.TrimRight(line, " \t"))
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// Options controls what happens to sections the controller does not own.
type Options struct {
	// DisableLocal marks non-ac_ sections of the listed types as disabled, so a
	// thin-mode AP does not bring up its own SSIDs alongside the managed ones.
	// They are disabled rather than deleted so a fat-mode rollback is trivial.
	DisableLocal bool
	DisableTypes []string
	// ReplaceTypes drops the local sections of the listed types instead of
	// keeping them. Only for types that have no `disabled` option and that the
	// device writes anonymously, so there is no name to match on: `config
	// device` and `config bridge-vlan`. The deployed APs define their bridge in
	// an anonymous `config device` section, and leaving it next to the
	// controller's own bridge definition would give netifd two devices with the
	// same name.
	//
	// A type listed here is only dropped when incoming actually carries sections
	// of that type, so a wireless-only package can never delete a device's
	// network config.
	ReplaceTypes []string
	// ProtectNames lists section names that must never be dropped or disabled,
	// whatever the other rules say. `loopback` is the one that matters: it is a
	// plain `config interface`, so a rule meant for LAN interfaces would take
	// the device's own stack down.
	ProtectNames []string
	// DropInterfacesOnBridgePorts drops local `config interface` sections whose
	// `device` is a physical port the incoming file enslaves to a bridge. netifd
	// cannot put L3 on a bridge port, so a factory `wan` (proto dhcp) left beside
	// the managed br-lan either keeps a dead routing-mode interface or fights the
	// bridge for the port. Only exact port names match, so `wan.2` and the
	// br-lan.N interfaces are untouched.
	DropInterfacesOnBridgePorts bool
}

// Merge applies the controller's sections to an existing file.
//
// A local section is replaced when the incoming set claims the same type and
// name, when it carries the ac_ prefix (the controller owned it in an earlier
// revision), or when its type is listed in Options.ReplaceTypes. Everything
// else is left alone, so a local interface the controller has never heard of —
// an uplink, say — survives untouched.
//
// Matching by name rather than by prefix is what lets the controller render
// netifd interfaces under the device's own spelling: /etc/config/firewall zones
// them by name, and renaming lan3 to ac_lan3 takes it out of its zone.
//
// Incoming must itself be a UCI file (its preamble is discarded: the agent
// never writes comments into a device config).
//
// Merge is idempotent: merging the same incoming twice yields the same output.
func Merge(existing, incoming string, opts Options) string {
	cur := Parse(existing)
	add := Parse(incoming)

	disable := map[string]bool{}
	for _, t := range opts.DisableTypes {
		disable[t] = true
	}
	protect := map[string]bool{}
	for _, n := range opts.ProtectNames {
		protect[n] = true
	}
	// Guard against deleting local sections the package says nothing about.
	replace := map[string]bool{}
	for _, t := range opts.ReplaceTypes {
		replace[t] = true
	}
	dropPorts := map[string]bool{}
	if opts.DropInterfacesOnBridgePorts {
		dropPorts = bridgePorts(add)
	}
	present := map[string]bool{}
	claimed := map[string]bool{}
	for _, s := range add.Sections {
		present[s.Type] = true
		claimed[s.Type+"\x00"+s.Name] = true
	}

	out := File{Preamble: cur.Preamble}
	for _, s := range cur.Sections {
		if protect[s.Name] {
			out.Sections = append(out.Sections, s)
			continue
		}
		if s.IsAC() {
			continue // the controller owned it in an earlier revision
		}
		if s.Type == "interface" && dropPorts[s.option("device")] {
			continue // its device is now a bridge port and cannot carry L3
		}
		// Anonymous sections cannot be claimed by name; ReplaceTypes is the
		// only rule that can retire them.
		if s.Name != "" && claimed[s.Type+"\x00"+s.Name] {
			continue
		}
		if replace[s.Type] && present[s.Type] {
			continue // no `disabled` exists for this type: drop it instead
		}
		if opts.DisableLocal && disable[s.Type] {
			s = s.withDisabled()
		}
		out.Sections = append(out.Sections, s)
	}
	out.Sections = append(out.Sections, add.Sections...)
	return out.Render()
}

// withDisabled returns a copy of the section with `option disabled '1'` set,
// replacing an existing disabled option instead of appending a duplicate. A
// duplicate would make uci's behaviour depend on file order.
func (s Section) withDisabled() Section {
	clone := Section{Type: s.Type, Name: s.Name, Lines: append([]string(nil), s.Lines...)}
	for i := 1; i < len(clone.Lines); i++ {
		fields := strings.Fields(clone.Lines[i])
		if len(fields) >= 2 && fields[0] == "option" && fields[1] == "disabled" {
			clone.Lines[i] = "\toption disabled '1'"
			return clone
		}
	}
	clone.Lines = append(clone.Lines[:1], append([]string{"\toption disabled '1'"}, clone.Lines[1:]...)...)
	return clone
}

// ACNames lists the section names the controller owns in a file, in order. The
// agent uses it to build the health check expectations.
func ACNames(content string) []string {
	var names []string
	for _, s := range Parse(content).Sections {
		if s.IsAC() && s.Name != "" {
			names = append(names, s.Name)
		}
	}
	return names
}

// ACWifiInterfaces lists the managed BSS section names. These are the sections
// hostapd reports through `ubus call network.wireless status`, so they are what
// the post-apply health check must find up.
// ACWifiInterfaces lists the BSSes the health check must see up.
//
// Disabled BSSes are excluded. A template deliberately disables some SSIDs (the
// deployed XR1710G has 7 of 18 off), and expecting them to come up makes every
// apply fail its health check and roll back a perfectly correct configuration —
// observed on real hardware before this was fixed.
func ACWifiInterfaces(content string) []string {
	var names []string
	for _, s := range Parse(content).Sections {
		if s.Type != "wifi-iface" || !s.IsAC() || s.Name == "" {
			continue
		}
		if s.option("disabled") == "1" {
			continue
		}
		names = append(names, s.Name)
	}
	return names
}

// option returns an option's value from the section, unquoted. It is a targeted
// reader, not a general UCI parser: only the keys the merge logic needs.
func (s Section) option(key string) string {
	prefix := "option " + key + " "
	for _, line := range s.Lines {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, prefix) {
			continue
		}
		return strings.Trim(strings.TrimSpace(t[len(prefix):]), "'\"")
	}
	return ""
}

// listValues returns every `list <key>` value in the section, unquoted.
func (s Section) listValues(key string) []string {
	prefix := "list " + key + " "
	var vals []string
	for _, line := range s.Lines {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, prefix) {
			continue
		}
		if v := strings.Trim(strings.TrimSpace(t[len(prefix):]), "'\""); v != "" {
			vals = append(vals, v)
		}
	}
	return vals
}

// bridgePorts collects the physical ports every bridge device in the incoming
// file claims. Only sections explicitly typed as a bridge count, so a stray
// device section cannot make the merge delete an uplink.
func bridgePorts(f File) map[string]bool {
	ports := map[string]bool{}
	for _, s := range f.Sections {
		if s.Type != "device" || s.option("type") != "bridge" {
			continue
		}
		for _, p := range s.listValues("ports") {
			ports[p] = true
		}
	}
	return ports
}

// Validate checks that a rendered file is structurally sane before it is written
// to the device: every ac_ section must have a name, and names must be unique.
// A duplicate name would silently shadow a BSS at apply time.
func Validate(content string) error {
	seen := map[string]bool{}
	for _, s := range Parse(content).Sections {
		if !s.IsAC() {
			continue
		}
		if s.Name == "" {
			return fmt.Errorf("ucimerge: anonymous %s section in controller output", s.Type)
		}
		if seen[s.Name] {
			return fmt.Errorf("ucimerge: duplicate section %q", s.Name)
		}
		seen[s.Name] = true
	}
	if len(seen) == 0 {
		return fmt.Errorf("ucimerge: no %s sections in controller output", ACPrefix)
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Netdevs lists the underlying devices the interface sections are built on
// (br-lan.1, br-lan.2, ...), sorted and deduplicated.
//
// The agent uses it to know what must exist before hostapd is asked to bring up
// a BSS: a `network reload` rebuilds those devices, and hostapd started in that
// window attaches its interfaces to nothing and stays down.
func Netdevs(content string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range Parse(content).Sections {
		if s.Type != "interface" {
			continue
		}
		if d := s.option("device"); d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

// ManagementInterfaces lists the interfaces that carry this AP's own management
// addresses: static interfaces built on the br-lan bridge (br-lan, br-lan.1,
// br-lan.2, ...). Uplinks (wan, proto dhcp) and client VLANs (proto none) are
// deliberately excluded, so the agent can open firewall/SSH access without
// exposing the uplink.
func ManagementInterfaces(content string) []string {
	var out []string
	for _, s := range Parse(content).Sections {
		if s.Type != "interface" || s.Name == "" || s.Name == "loopback" {
			continue
		}
		if s.option("proto") != "static" {
			continue
		}
		d := s.option("device")
		if d == "br-lan" || strings.HasPrefix(d, "br-lan.") {
			out = append(out, s.Name)
		}
	}
	sort.Strings(out)
	return out
}
