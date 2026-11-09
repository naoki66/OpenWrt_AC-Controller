package agent

import (
	"encoding/binary"
	"net"
)

// rtnetlink neighbour decoding.
//
// `ip neigh show` was 6 ms of the per-cycle budget on the reference AP, and
// almost all of it was fork+exec of iproute2 — the neighbour table itself is a
// handful of entries. The kernel ABI is small enough to read directly, and the
// same argument as nl80211 applies: the cost is the process, not the netlink
// round trip.
//
// Kept free of build tags so the parser is testable everywhere; only the socket
// half is Linux-specific.
const (
	rtmGetNeigh = 30

	ndaDst    = 1
	ndaLLAddr = 2

	// ndmsg is fixed-size and every field is read by offset below:
	// family u8 | pad u8 | pad u16 | ifindex u32 | state u16 | flags u8 | type u8
	ndmsgLen     = 12
	ndmsgOffType = 11
	ndmsgOffStat = 8

	// NUD_FAILED entries are exactly the ones `ip neigh show` prints without a
	// lladdr or with `FAILED`; they carry no usable address.
	nudFailed = 0x20

	// rtnUnicast is RTN_UNICAST, the only neighbour kind `ip neigh show` lists.
	// The broadcast, multicast and 0.0.0.0 entries the bridge installs are real
	// rows in the table but never a client, and keeping them would spend map
	// entries and make the two sources disagree on rubbish alone.
	rtnUnicast = 1

	// afInet is AF_INET. Spelled out rather than taken from x/sys, whose unix
	// package does not exist on every platform this file must compile for.
	afInet = 2
)

// buildGetNeighbours encodes an RTM_GETNEIGH dump. family AF_INET asks the
// kernel to filter, so the IPv6 link-local router entries that dominate a
// bridged AP's table never cross the socket — that is 56 of 58 lines on the
// reference site.
func buildGetNeighbours(family uint8, seq uint32, flags uint16) []byte {
	payload := make([]byte, ndmsgLen)
	payload[0] = family // ifindex stays 0: every interface, not one
	return nlmsg(rtmGetNeigh, flags, seq, 0, payload)
}

// parseNeighbourMessage decodes one RTM_NEWNEIGH reply into an IPv4 address and
// its link-layer address. ok is false for anything that cannot be used as a
// client address: a non-IPv4 family, a failed entry, or a missing address.
func parseNeighbourMessage(data []byte) (ip, mac string, ok bool) {
	if len(data) < ndmsgLen {
		return "", "", false
	}
	family := data[0]
	state := binary.NativeEndian.Uint16(data[ndmsgOffStat:])
	if family != afInet || state&nudFailed != 0 || data[ndmsgOffType] != rtnUnicast {
		return "", "", false
	}
	walkAttrs(data[ndmsgLen:], func(typ uint16, v []byte) {
		switch typ {
		case ndaDst:
			if len(v) == 4 {
				ip = net.IP(v).String()
			}
		case ndaLLAddr:
			if len(v) == 6 {
				mac = net.HardwareAddr(v).String()
			}
		}
	})
	return ip, mac, ip != "" && mac != ""
}

// parseNeighbourDump folds a whole dump into MAC→IPv4. A MAC appearing on two
// addresses keeps the last one the kernel reported, matching what the text
// parser does with duplicate lines.
func parseNeighbourDump(frames [][]byte) map[string]string {
	out := map[string]string{}
	for _, f := range frames {
		ip, mac, ok := parseNeighbourMessage(f)
		if ok {
			out[mac] = ip
		}
	}
	return out
}
