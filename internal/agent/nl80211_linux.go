//go:build linux

package agent

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/naoki66/ac-controller/internal/proto"
)

// nl80211 readers (socket half; the attribute decoders are in nl80211_parse.go
// so they can be tested everywhere).
//
// One generic-netlink dump replaces one process per BSS, and one more replaces
// the `ubus call network.wireless status` that only ever existed to learn the
// BSS list. The costs are not comparable: a fork+exec of a dynamically linked
// binary measures ~6 ms on the reference AP, a dump measures ~0.1 ms. At the
// steering loop's cadence that is the difference between "sampling every BSS is
// the dominant cost" and "sampling is free".

// nlClient speaks generic netlink for one family. The family id is resolved
// once per process: it is assigned at module load and stable afterwards.
type nlClient struct {
	family uint16
	known  bool
}

// netlinkStationReader is the platform implementation.
type netlinkStationReader struct{ nl nlClient }

// newNetlinkStationReader returns the reader, or nil when the platform cannot
// provide one so the caller can fall back without its own platform check.
func newNetlinkStationReader() StationReader { return &netlinkStationReader{} }

func (r *netlinkStationReader) Name() string { return "nl80211" }

// Stations dumps the station table of one interface. ifindex is preferred when
// the caller already knows it (the BSS dump reports it); a zero falls back to a
// name lookup, which costs one link dump.
func (r *netlinkStationReader) Stations(ctx context.Context, ifname string, ifindex int) ([]proto.ClientStat, error) {
	idx, err := resolveIfindex(ifname, ifindex)
	if err != nil {
		return nil, err
	}
	var out []proto.ClientStat
	err = r.nl.dump(ctx, func(seq uint32, family uint16) []byte {
		return buildGetStation(idx, seq, family, unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	}, func(data []byte) {
		if st, ok := parseStationMessage(data); ok {
			out = append(out, st)
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// resolveIfindex prefers a caller-supplied index and only consults the kernel
// when it has none. The name is carried through for the error message: "no such
// interface" with no name is the hardest kind of failure to place.
func resolveIfindex(ifname string, ifindex int) (uint32, error) {
	if ifindex > 0 {
		return uint32(ifindex), nil
	}
	ifi, err := net.InterfaceByName(ifname)
	if err != nil {
		return 0, fmt.Errorf("interface %s: %w", ifname, err)
	}
	return uint32(ifi.Index), nil
}

// BSSReader implementation: one dump lists every wireless interface with its
// name, type, SSID and frequency.
type netlinkBSSReader struct{ nl nlClient }

func newNetlinkBSSReader() BSSReader { return &netlinkBSSReader{} }

func (r *netlinkBSSReader) Name() string { return "nl80211" }

func (r *netlinkBSSReader) BSS(ctx context.Context) ([]bssInfo, error) {
	out := make([]bssInfo, 0, 8)
	err := r.nl.dump(ctx, func(seq uint32, family uint16) []byte {
		return buildGetInterface(seq, family, unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	}, func(data []byte) {
		ifc, ok := parseInterfaceMessage(data)
		if !ok || !ifc.accessPoint() {
			return
		}
		out = append(out, bssInfo{
			Ifname:  ifc.Ifname,
			Ifindex: ifc.Ifindex,
			SSID:    ifc.SSID,
			Band:    ifc.Band,
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DebugStationAttrs renders the raw reply for one interface. On-device tool
// for pinning the attribute numbers: a wrong one shows up as a zero, not an
// error.
func DebugStationAttrs(ifname string) (string, error) {
	var ifindex uint32
	if ifname != "" {
		idx, err := resolveIfindex(ifname, 0)
		if err != nil {
			return "", err
		}
		ifindex = idx
	}
	r := &netlinkStationReader{}
	var b strings.Builder
	err := r.nl.dump(context.Background(), func(seq uint32, family uint16) []byte {
		return buildGetStation(ifindex, seq, family, unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	}, func(data []byte) {
		b.WriteString(DescribeStationAttrs(data))
		b.WriteString("\n")
	})
	return b.String(), err
}

// DebugInterfaceAttrs renders one interface dump. Same purpose as
// DebugStationAttrs: NL80211_CMD_GET_INTERFACE is a different command with its
// own attribute set, and every number in it has to be seen before it is
// trusted.
func DebugInterfaceAttrs() (string, error) {
	r := &netlinkBSSReader{}
	var b strings.Builder
	err := r.nl.dump(context.Background(), func(seq uint32, family uint16) []byte {
		return buildGetInterface(seq, family, unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	}, func(data []byte) {
		b.WriteString(DescribeInterfaceAttrs(data))
		b.WriteString("\n")
	})
	return b.String(), err
}

// dump sends one request and hands every reply frame to fn. The family id is
// resolved lazily so a failure to resolve is reported once, not per BSS.
func (c *nlClient) dump(ctx context.Context, build func(seq uint32, family uint16) []byte, fn func(data []byte)) error {
	fd, err := nlSocket(unix.NETLINK_GENERIC)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	family, err := c.familyID(fd)
	if err != nil {
		return err
	}
	seq := uint32(time.Now().UnixNano())
	return nlDump(ctx, fd, build(seq, family), fn)
}

// nlSocket opens a netlink socket bound to this process. The receive timeout is
// what keeps a stalled kernel from holding the collector's mutex forever: the
// heartbeat and the steering loop share this call.
func nlSocket(protocol int) (int, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, protocol)
	if err != nil {
		return -1, fmt.Errorf("netlink socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("netlink bind: %w", err)
	}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 2}); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("netlink timeout: %w", err)
	}
	return fd, nil
}

// nlDump sends one already-encoded request and walks the replies until the
// kernel sends NLMSG_DONE.
//
// data aliases one reused receive buffer and is only valid for the duration of
// the call: a dump can span several datagrams, and a caller that stores the
// slice and reads it after the loop sees whatever the last datagram left
// behind. That is not hypothetical: the first neighbour reader folded stored
// slices and reported 37 of the 42 entries present, with the losses scattered
// across the dump rather than at one end.
func nlDump(ctx context.Context, fd int, req []byte, fn func(data []byte)) error {
	if err := unix.Sendto(fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("netlink send: %w", err)
	}
	buf := make([]byte, 1<<16)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return fmt.Errorf("netlink recv: %w", err)
		}
		done := false
		for _, m := range parseNetlinkMessages(buf[:n]) {
			switch m.typ {
			case unix.NLMSG_DONE:
				done = true
			case unix.NLMSG_ERROR:
				return fmt.Errorf("netlink error: %w", netlinkErr(m.data))
			default:
				fn(m.data)
			}
		}
		if done {
			return nil
		}
	}
}

// familyID caches the nl80211 generic-netlink family id.
func (c *nlClient) familyID(fd int) (uint16, error) {
	if c.known {
		return c.family, nil
	}
	seq := uint32(time.Now().UnixNano())
	payload := []byte{ctrlCmdGetFamily, genlCtrlVersion, 0, 0}
	payload = putAttr(payload, ctrlAttrFamilyName, append([]byte("nl80211"), 0))
	req := nlmsg(genlIDCtrl, unix.NLM_F_REQUEST, seq, 0, payload)
	if err := unix.Sendto(fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return 0, fmt.Errorf("family request: %w", err)
	}
	buf := make([]byte, 4096)
	n, _, err := unix.Recvfrom(fd, buf, 0)
	if err != nil {
		return 0, fmt.Errorf("family reply: %w", err)
	}
	for _, m := range parseNetlinkMessages(buf[:n]) {
		if m.typ == unix.NLMSG_ERROR {
			return 0, fmt.Errorf("family error: %w", netlinkErr(m.data))
		}
		var id uint16
		walkAttrs(genlPayload(m.data), func(typ uint16, data []byte) {
			if typ == ctrlAttrFamilyID && len(data) == 2 {
				id = binary.NativeEndian.Uint16(data)
			}
		})
		if id != 0 {
			c.family, c.known = id, true
			return id, nil
		}
	}
	return 0, fmt.Errorf("nl80211 family not reported by the kernel")
}

// netlinkErr turns NLMSG_ERROR payload into an error.
func netlinkErr(data []byte) error {
	if len(data) < 4 {
		return fmt.Errorf("truncated error message")
	}
	code := int32(binary.NativeEndian.Uint32(data[0:4]))
	if code == 0 {
		return fmt.Errorf("ack")
	}
	return unix.Errno(-code)
}
