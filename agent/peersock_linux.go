//go:build linux

package agent

import (
	"context"
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// listenPeerMulticast binds a UDP/IPv6 socket to the management device, joins
// the link-local multicast group and pins every outgoing multicast packet to
// that interface with a hop limit of one. This is the transport uneighbord uses;
// keeping it inside the agent avoids a second daemon and a second writer of
// hostapd's neighbour table.
func listenPeerMulticast(port int, device, group string) (*net.UDPConn, error) {
	if device == "" {
		return nil, fmt.Errorf("no management device")
	}
	ifi, err := net.InterfaceByName(device)
	if err != nil {
		return nil, fmt.Errorf("interface %s: %w", device, err)
	}
	gaddr := net.ParseIP(group)
	if gaddr == nil || gaddr.To16() == nil {
		return nil, fmt.Errorf("invalid multicast group %q", group)
	}

	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var setErr error
			if err := c.Control(func(fd uintptr) {
				if e := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); e != nil {
					setErr = e
					return
				}
				if e := unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 1); e != nil {
					setErr = e
					return
				}
				if e := unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, device); e != nil {
					setErr = e
					return
				}
			}); err != nil {
				return err
			}
			return setErr
		},
	}

	pc, err := lc.ListenPacket(context.Background(), "udp6", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, err
	}
	conn, ok := pc.(*net.UDPConn)
	if !ok {
		_ = pc.Close()
		return nil, fmt.Errorf("udp6 listener returned %T", pc)
	}

	raw, err := conn.SyscallConn()
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	var setErr error
	if err := raw.Control(func(fd uintptr) {
		mreq := unix.IPv6Mreq{Interface: uint32(ifi.Index)}
		copy(mreq.Multiaddr[:], gaddr.To16())
		if e := unix.SetsockoptIPv6Mreq(int(fd), unix.IPPROTO_IPV6, unix.IPV6_JOIN_GROUP, &mreq); e != nil {
			setErr = e
			return
		}
		if e := unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_IF, ifi.Index); e != nil {
			setErr = e
			return
		}
		if e := unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_HOPS, 1); e != nil {
			setErr = e
			return
		}
		if e := unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_LOOP, 0); e != nil {
			setErr = e
			return
		}
	}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if setErr != nil {
		_ = conn.Close()
		return nil, setErr
	}
	return conn, nil
}
