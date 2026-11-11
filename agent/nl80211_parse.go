package agent

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"

	"github.com/naoki66/ac-controller/shared/proto"
)

// nl80211 attribute decoding.
//
// Kept free of build tags so the wire format can be tested on the developer's
// machine: only the socket half is Linux-specific, and a byte-level decoder is
// exactly the part that must be pinned by tests. The numbers are kernel ABI
// from <linux/nl80211.h>.
const (
	genlIDCtrl         = 16
	ctrlCmdGetFamily   = 3
	ctrlAttrFamilyID   = 1
	ctrlAttrFamilyName = 2
	genlCtrlVersion    = 1

	nl80211CmdGetInterface = 5
	nl80211CmdGetStation   = 17

	nl80211AttrIfindex   = 3
	nl80211AttrIfname    = 4
	nl80211AttrIfType    = 5
	nl80211AttrMAC       = 6
	nl80211AttrStaInfo   = 21
	nl80211AttrWiphyFreq = 38
	nl80211AttrSSID      = 52

	// Not decoded: observed on the reference AP as WIPHY_TX_POWER_LEVEL 98
	// (mBm) and CHANNEL_WIDTH 159. The radio collector keeps reading those from
	// netifd because it also needs config-derived fields netlink does not carry
	// (htmode string, country, disabled).

	nl80211IftypeAP = 3

	staInfoInactiveTime  = 1
	staInfoSignal        = 7
	staInfoTxBitrate     = 8
	staInfoSignalAvg     = 13
	staInfoRxBitrate     = 14
	staInfoConnectedTime = 16

	rateInfoBitrate   = 1
	rateInfoBitrate32 = 10

	nl80211GenlVersion = 0
)

// --- message construction -------------------------------------------------

func nlmsg(typ uint16, flags uint16, seq, pid uint32, payload []byte) []byte {
	hdr := make([]byte, 16)
	binary.NativeEndian.PutUint32(hdr[0:], uint32(16+len(payload)))
	binary.NativeEndian.PutUint16(hdr[4:], typ)
	binary.NativeEndian.PutUint16(hdr[6:], flags)
	binary.NativeEndian.PutUint32(hdr[8:], seq)
	binary.NativeEndian.PutUint32(hdr[12:], pid)
	return append(hdr, payload...)
}

// buildGetStation encodes a dump request. ifindex 0 omits the attribute, which
// is what makes the kernel walk every netdev instead of one BSS.
func buildGetStation(ifindex, seq uint32, family uint16, flags uint16) []byte {
	payload := []byte{nl80211CmdGetStation, nl80211GenlVersion, 0, 0}
	if ifindex != 0 {
		idx := make([]byte, 4)
		binary.NativeEndian.PutUint32(idx, ifindex)
		payload = putAttr(payload, nl80211AttrIfindex, idx)
	}
	return nlmsg(family, flags, seq, 0, payload)
}

// buildGetInterface encodes a dump of every wireless interface. The reply
// carries name, type, SSID and frequency, which is everything the client
// collector needs to know about a BSS — the same facts netifd reports for the
// price of a ubus round trip and a process spawn.
func buildGetInterface(seq uint32, family uint16, flags uint16) []byte {
	payload := []byte{nl80211CmdGetInterface, nl80211GenlVersion, 0, 0}
	return nlmsg(family, flags, seq, 0, payload)
}

func putAttr(dst []byte, typ uint16, payload []byte) []byte {
	hdr := make([]byte, 4)
	binary.NativeEndian.PutUint16(hdr[0:], uint16(4+len(payload)))
	binary.NativeEndian.PutUint16(hdr[2:], typ)
	dst = append(dst, hdr...)
	dst = append(dst, payload...)
	for len(dst)%4 != 0 {
		dst = append(dst, 0)
	}
	return dst
}

// --- message parsing ------------------------------------------------------

// genlPayload strips the netlink header and the 4-byte generic-netlink header.
func genlPayload(data []byte) []byte {
	if len(data) < 4 {
		return nil
	}
	return data[4:]
}

// nlaTypeMask strips the attribute-flag bits. The kernel sets NLA_F_NESTED
// (0x8000) on every nested attribute, so NL80211_ATTR_STA_INFO arrives as
// 32789, not 21 — a decoder that compares the raw type simply never matches and
// reports every field as zero, which is exactly what the on-device check showed
// before this mask existed (see nl-dump).
const (
	nlaTypeMask = 0x3fff
)

func walkAttrs(b []byte, fn func(typ uint16, data []byte)) {
	for len(b) >= 4 {
		l := int(binary.NativeEndian.Uint16(b[0:2]))
		typ := binary.NativeEndian.Uint16(b[2:4]) & nlaTypeMask
		if l < 4 || l > len(b) {
			return
		}
		fn(typ, b[4:l])
		next := (l + 3) &^ 3
		if next >= len(b) {
			return
		}
		b = b[next:]
	}
}

// parseStationMessage decodes one NL80211_CMD_NEW_STATION reply.
func parseStationMessage(data []byte) (proto.ClientStat, bool) {
	st, _, ok := parseStationReply(data)
	return st, ok
}

// parseStationReply also reports the interface index the station belongs to,
// which is what makes an unfiltered dump usable: one request covers every BSS,
// and the replies are sorted apart by this attribute.
func parseStationReply(data []byte) (proto.ClientStat, uint32, bool) {
	var st proto.ClientStat
	var ifindex uint32
	var haveSignal bool
	walkAttrs(genlPayload(data), func(typ uint16, v []byte) {
		switch typ {
		case nl80211AttrIfindex:
			if len(v) == 4 {
				ifindex = binary.NativeEndian.Uint32(v)
			}
		case nl80211AttrMAC:
			if len(v) == 6 {
				st.MAC = net.HardwareAddr(v).String()
			}
		case nl80211AttrStaInfo:
			walkAttrs(v, func(t2 uint16, v2 []byte) {
				switch t2 {
				case staInfoInactiveTime:
					if len(v2) == 4 {
						st.InactiveSeconds = int(binary.NativeEndian.Uint32(v2)) / 1000
					}
				case staInfoSignal, staInfoSignalAvg:
					// SIGNAL is authoritative; SIGNAL_AVG only fills in when the
					// driver reports an average and no instantaneous value.
					if len(v2) == 1 && (t2 == staInfoSignal || !haveSignal) {
						st.Signal = int(int8(v2[0]))
						haveSignal = true
					}
				case staInfoConnectedTime:
					if len(v2) == 4 {
						st.ConnectedSeconds = int(binary.NativeEndian.Uint32(v2))
					}
				case staInfoRxBitrate:
					st.RxRateMbps = rateMbps(v2)
				case staInfoTxBitrate:
					st.TxRateMbps = rateMbps(v2)
				}
			})
		}
	})
	return st, ifindex, st.MAC != ""
}

// apInterface is one wireless interface as NL80211_CMD_GET_INTERFACE reports
// it. Only the fields the collector needs are decoded; anything absent stays
// zero rather than being guessed.
type apInterface struct {
	Ifindex int
	Ifname  string
	SSID    string
	Band    string
	FreqMHz int
	IfType  int
}

// accessPoint reports whether the interface is an AP-mode BSS. Other types
// (managed, monitor, mesh) are real interfaces with real ifindexes, and asking
// them for a station table returns nothing useful.
func (i apInterface) accessPoint() bool { return i.IfType == nl80211IftypeAP }

// parseInterfaceMessage decodes one NL80211_CMD_NEW_INTERFACE reply.
func parseInterfaceMessage(data []byte) (apInterface, bool) {
	var out apInterface
	walkAttrs(genlPayload(data), func(typ uint16, v []byte) {
		switch typ {
		case nl80211AttrIfindex:
			if len(v) == 4 {
				out.Ifindex = int(binary.NativeEndian.Uint32(v))
			}
		case nl80211AttrIfType:
			if len(v) == 4 {
				out.IfType = int(binary.NativeEndian.Uint32(v))
			}
		case nl80211AttrIfname:
			out.Ifname = nlaString(v)
		case nl80211AttrSSID:
			out.SSID = nlaString(v)
		case nl80211AttrWiphyFreq:
			if len(v) == 4 {
				out.FreqMHz = int(binary.NativeEndian.Uint32(v))
			}
		}
	})
	out.Band = bandOfFreq(out.FreqMHz)
	return out, out.Ifname != ""
}

// nlaString decodes an NLA_STRING: a byte string whose trailing NUL is counted
// in the attribute length. NL80211_ATTR_IFNAME carries that NUL — measured on
// the reference AP as len 11 for "phy0.1-ap3" — and keeping it turns every
// interface lookup into "no such interface". NL80211_ATTR_SSID does not, but a
// usable SSID cannot contain a NUL, so trimming is safe for both.
func nlaString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// bandOfFreq maps a centre frequency to the band name the controller's client
// and radio tables already use. Boundaries follow the 802.11 band plan, not a
// regulatory domain: 5925 MHz is the first 6 GHz channel everywhere it exists.
func bandOfFreq(mhz int) string {
	switch {
	case mhz >= 2400 && mhz < 2500:
		return "2g"
	case mhz >= 4900 && mhz < 5925:
		return "5g"
	case mhz >= 5925 && mhz <= 7125:
		return "6g"
	}
	return ""
}

// DescribeInterfaceAttrs renders one interface reply as a type/length tree, for
// the same reason DescribeStationAttrs exists: a wrong attribute number is a
// plausible zero, not an error.
func DescribeInterfaceAttrs(data []byte) string {
	var b []byte
	walkAttrs(genlPayload(data), func(typ uint16, v []byte) {
		switch typ {
		case nl80211AttrIfname, nl80211AttrSSID:
			b = append(b, []byte(fmt.Sprintf("attr %d len %d str %q\n", typ, len(v), string(v)))...)
		default:
			b = append(b, []byte(fmt.Sprintf("attr %d len %d raw % x\n", typ, len(v), v))...)
		}
	})
	return string(b)
}

// rateMbps converts an NL80211_RATE_INFO nested attribute into Mbps.
// Both bitrate attributes are in 100 kbps units; the 32-bit one exists because
// 802.11ax rates overflow the 16-bit field. Prefer it when present.
func rateMbps(b []byte) int {
	var b16, b32 int
	walkAttrs(b, func(typ uint16, v []byte) {
		switch typ {
		case rateInfoBitrate:
			if len(v) == 2 {
				b16 = int(binary.NativeEndian.Uint16(v))
			}
		case rateInfoBitrate32:
			if len(v) == 4 {
				b32 = int(binary.NativeEndian.Uint32(v))
			}
		}
	})
	hundredKbps := b32
	if hundredKbps == 0 {
		hundredKbps = b16
	}
	// 1444 -> 144.4 Mbps -> 144, matching roundMbps on iw's text output.
	return (hundredKbps + 5) / 10
}

// netlinkMessage is one decoded netlink frame.
type netlinkMessage struct {
	typ   uint16
	flags uint16
	seq   uint32
	data  []byte
}

// parseNetlinkMessages splits one recv buffer into frames. Written out rather
// than taken from syscall/unix so both platforms decode identically and the
// framing rules (16-byte header, 4-byte alignment) are visible next to the
// attribute parser that depends on them.
func parseNetlinkMessages(b []byte) []netlinkMessage {
	var out []netlinkMessage
	for len(b) >= 16 {
		l := int(binary.NativeEndian.Uint32(b[0:4]))
		typ := binary.NativeEndian.Uint16(b[4:6])
		if l < 16 || l > len(b) {
			return out
		}
		out = append(out, netlinkMessage{
			typ: typ, flags: binary.NativeEndian.Uint16(b[6:8]),
			seq: binary.NativeEndian.Uint32(b[8:12]), data: b[16:l],
		})
		next := (l + 3) &^ 3
		if next >= len(b) {
			return out
		}
		b = b[next:]
	}
	return out
}

// DescribeStationAttrs renders one station reply as a type/length tree. It
// exists for on-device diagnosis: the attribute numbers are kernel ABI, and
// when one is wrong the symptom is a plausible-looking zero rather than an
// error, so the only reliable check is to look at what the kernel actually
// sent.
func DescribeStationAttrs(data []byte) string {
	var b []byte
	walkAttrs(genlPayload(data), func(typ uint16, v []byte) {
		b = append(b, []byte(fmt.Sprintf("attr %d len %d\n", typ, len(v)))...)
		if typ == nl80211AttrStaInfo {
			walkAttrs(v, func(t2 uint16, v2 []byte) {
				b = append(b, []byte(fmt.Sprintf("  info %d len %d raw % x\n", t2, len(v2), v2))...)
			})
		}
	})
	return string(b)
}
