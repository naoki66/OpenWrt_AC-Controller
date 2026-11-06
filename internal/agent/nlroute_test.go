package agent

import (
	"encoding/binary"
	"net"
	"testing"
)

// buildInterfaceReply assembles a NL80211_CMD_NEW_INTERFACE frame: the 4-byte
// generic-netlink header, then one attribute per field. The kernel nests
// nothing here, which is why an interface reply is flat and the station record
// is not.
func buildInterfaceReply(ifindex int, ifname, ssid string, iftype, freq int) []byte {
	u32 := func(v int) []byte {
		b := make([]byte, 4)
		binary.NativeEndian.PutUint32(b, uint32(v))
		return b
	}
	msg := []byte{nl80211CmdGetInterface, nl80211GenlVersion, 0, 0}
	msg = putAttr(msg, nl80211AttrIfindex, u32(ifindex))
	// The kernel includes the terminating NUL in the attribute length; the
	// fixture has to as well or the trim is not what is being tested.
	msg = putAttr(msg, nl80211AttrIfname, append([]byte(ifname), 0))
	msg = putAttr(msg, nl80211AttrIfType, u32(iftype))
	if ssid != "" {
		msg = putAttr(msg, nl80211AttrSSID, []byte(ssid))
	}
	if freq > 0 {
		msg = putAttr(msg, nl80211AttrWiphyFreq, u32(freq))
	}
	return msg
}

// The BSS list moved from netifd to nl80211 to save a ubus round trip, so every
// field it used to get from netifd must come out of this reply unchanged. A
// mis-numbered attribute would not fail: it would produce an empty band and a
// client table with no SSID on it.
func TestParseInterfaceMessageMatchesNetifdFacts(t *testing.T) {
	cases := []struct {
		name   string
		freq   int
		band   string
		ifname string
		ssid   string
	}{
		{"2.4 GHz", 2427, "2g", "phy0.0-ap0", "SSID-A"},
		{"5 GHz", 5180, "5g", "phy0.1-ap0", "SSID-A_5G"},
		{"6 GHz", 6135, "6g", "phy0.2-ap4", "SSID-E_6G"},
		{"down interface reports no frequency", 0, "", "phy0.1-ap9", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ifc, ok := parseInterfaceMessage(buildInterfaceReply(23, tc.ifname, tc.ssid, nl80211IftypeAP, tc.freq))
			if !ok {
				t.Fatal("interface reply rejected")
			}
			if ifc.Ifname != tc.ifname {
				t.Errorf("Ifname = %q, want %q", ifc.Ifname, tc.ifname)
			}
			if ifc.SSID != tc.ssid {
				t.Errorf("SSID = %q, want %q", ifc.SSID, tc.ssid)
			}
			if ifc.Band != tc.band {
				t.Errorf("Band = %q, want %q (freq %d)", ifc.Band, tc.band, tc.freq)
			}
			if ifc.Ifindex != 23 {
				t.Errorf("Ifindex = %d, want 23", ifc.Ifindex)
			}
			if !ifc.accessPoint() {
				t.Error("AP-mode interface not recognised as an access point")
			}
		})
	}
}

// NL80211_ATTR_IFNAME is an NLA_STRING and the kernel counts its NUL in the
// length: "phy0.1-ap3" arrives as 11 bytes. Keeping the NUL makes every name
// comparison fail, and the symptom is an interface that does not exist rather
// than a decode error — the on-device dump is what caught it.
func TestParseInterfaceMessageTrimsTheNameNUL(t *testing.T) {
	ifc, ok := parseInterfaceMessage(buildInterfaceReply(25, "phy0.1-ap3", "SSID-D_5G", nl80211IftypeAP, 5180))
	if !ok {
		t.Fatal("interface reply rejected")
	}
	if ifc.Ifname != "phy0.1-ap3" {
		t.Fatalf("Ifname = %q, want %q", ifc.Ifname, "phy0.1-ap3")
	}
	// An SSID with no NUL must survive untouched.
	if ifc.SSID != "SSID-D_5G" {
		t.Errorf("SSID = %q", ifc.SSID)
	}
}

// A managed or monitor interface is a real netdev with a real ifindex, and
// asking it for a station table returns nothing useful. Filtering happens on
// the type so the collector never spends a dump on one.
func TestParseInterfaceMessageRejectsNonAccessPoints(t *testing.T) {
	const iftypeStation = 2
	ifc, ok := parseInterfaceMessage(buildInterfaceReply(9, "wlan0", "", iftypeStation, 5180))
	if !ok {
		t.Fatal("reply should still decode")
	}
	if ifc.accessPoint() {
		t.Fatal("station-mode interface treated as an AP")
	}
}

// The band boundaries are the 802.11 band plan, not a regulatory domain: 5925
// MHz is the first 6 GHz channel everywhere 6 GHz exists, and 5 GHz ends there.
func TestBandOfFreqBoundaries(t *testing.T) {
	cases := map[int]string{
		2412: "2g", 2484: "2g",
		0: "", 2399: "", 2500: "",
		4910: "5g", 5180: "5g", 5924: "5g",
		5925: "6g", 6135: "6g", 7125: "6g",
		7126: "", 1725: "",
	}
	for mhz, want := range cases {
		if got := bandOfFreq(mhz); got != want {
			t.Errorf("bandOfFreq(%d) = %q, want %q", mhz, got, want)
		}
	}
}

// --- neighbours -----------------------------------------------------------

// buildNeighbourReply assembles an RTM_NEWNEIGH frame: the fixed 12-byte ndmsg
// header, then NDA_* attributes.
func buildNeighbourReply(t *testing.T, family uint8, state uint16, ip string, mac []byte) []byte {
	t.Helper()
	msg := make([]byte, ndmsgLen)
	msg[0] = family
	msg[11] = rtnUnicast
	binary.NativeEndian.PutUint16(msg[8:], state)
	if ip != "" {
		parsed := net.ParseIP(ip).To4()
		if parsed == nil {
			t.Fatalf("not an IPv4 address: %q", ip)
		}
		msg = putAttr(msg, ndaDst, parsed)
	}
	if mac != nil {
		msg = putAttr(msg, ndaLLAddr, mac)
	}
	return msg
}

func TestParseNeighbourMessage(t *testing.T) {
	mac := []byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x3d}

	ip, got, ok := parseNeighbourMessage(buildNeighbourReply(t, afInet, 0x02 /* REACHABLE */, "10.0.10.143", mac))
	if !ok {
		t.Fatal("reachable IPv4 entry rejected")
	}
	if ip != "10.0.10.143" {
		t.Errorf("ip = %q", ip)
	}
	if got != "02:00:00:00:00:3d" {
		t.Errorf("mac = %q", got)
	}
}

// The AF_INET filter is what keeps the reference AP's 56 IPv6 link-local router
// entries out of the reply. If one arrives anyway it must be ignored, not mapped
// onto a MAC and shipped to the controller as a client address.
func TestParseNeighbourMessageSkipsIPv6AndFailedEntries(t *testing.T) {
	mac := []byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x3d}
	if _, _, ok := parseNeighbourMessage(buildNeighbourReply(t, 10 /* AF_INET6 */, 0x02, "10.0.10.143", mac)); ok {
		t.Error("IPv6 entry accepted")
	}
	if _, _, ok := parseNeighbourMessage(buildNeighbourReply(t, afInet, nudFailed, "10.0.10.143", mac)); ok {
		t.Error("NUD_FAILED entry accepted, it carries no usable address")
	}
	if _, _, ok := parseNeighbourMessage(buildNeighbourReply(t, afInet, 0x02, "10.0.10.143", nil)); ok {
		t.Error("entry with no lladdr accepted")
	}
	if _, _, ok := parseNeighbourMessage([]byte{1, 2, 3}); ok {
		t.Error("truncated frame accepted")
	}
}

// The bridge installs broadcast, multicast and 0.0.0.0 rows as real neighbours.
// `ip neigh show` lists only unicast entries, and so must this decoder — those
// four junk keys were the entire difference between the two sources on a live
// AP, which hides a real decode bug behind noise.
func TestParseNeighbourMessageSkipsNonUnicastRows(t *testing.T) {
	mac := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	for _, entryType := range []uint8{2 /* LOCAL */, 3 /* BROADCAST */, 5 /* MULTICAST */} {
		reply := buildNeighbourReply(t, afInet, 0x02, "10.0.10.255", mac)
		reply[11] = entryType
		if _, _, ok := parseNeighbourMessage(reply); ok {
			t.Errorf("entry type %d accepted, only RTN_UNICAST is a client", entryType)
		}
	}
	reply := buildNeighbourReply(t, afInet, 0x02, "10.0.10.5", mac)
	if _, _, ok := parseNeighbourMessage(reply); !ok {
		t.Error("unicast entry rejected")
	}
}

// The text parser folds duplicate lines last-wins; the netlink path must agree,
// because the two are compared field by field on a live AP.
func TestParseNeighbourDumpFoldsToLastAnswer(t *testing.T) {
	old := []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x01}
	cur := []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x02}
	table := parseNeighbourDump([][]byte{
		buildNeighbourReply(t, afInet, 0x02, "10.0.10.1", old),
		buildNeighbourReply(t, afInet, 0x02, "10.0.10.2", cur),
	})
	if len(table) != 2 {
		t.Fatalf("table = %+v, want 2 entries", table)
	}
	if table["aa:bb:cc:dd:ee:02"] != "10.0.10.2" {
		t.Errorf("second entry = %+v", table)
	}
}

// The request itself has to be shaped right or the kernel answers with the
// whole table: family AF_INET and ifindex 0 (all interfaces), followed by no
// attributes at all.
func TestBuildGetNeighboursAsksForIPv4Only(t *testing.T) {
	req := buildGetNeighbours(afInet, 7, 0x0301)
	if got := binary.NativeEndian.Uint32(req[0:4]); int(got) != len(req) {
		t.Errorf("nlmsg length = %d, want %d", got, len(req))
	}
	if typ := binary.NativeEndian.Uint16(req[4:6]); int(typ) != rtmGetNeigh {
		t.Errorf("nlmsg type = %d, want %d", typ, rtmGetNeigh)
	}
	payload := req[16:]
	if len(payload) != ndmsgLen {
		t.Fatalf("payload = %d bytes, want %d (no attributes)", len(payload), ndmsgLen)
	}
	if payload[0] != afInet {
		t.Errorf("family = %d, want %d", payload[0], afInet)
	}
	if idx := binary.NativeEndian.Uint32(payload[4:]); idx != 0 {
		t.Errorf("ifindex = %d, want 0 (every interface)", idx)
	}
}
