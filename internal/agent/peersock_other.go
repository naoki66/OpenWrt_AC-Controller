//go:build !linux

package agent

import (
	"fmt"
	"net"
)

// The agent itself targets OpenWrt/Linux. On a workstation build the IPv6
// multicast transport is unavailable; the bus falls back to IPv4 broadcast so
// the rest of the code remains buildable and testable.
func listenPeerMulticast(port int, device, group string) (*net.UDPConn, error) {
	return nil, fmt.Errorf("IPv6 multicast peer transport is only implemented on Linux")
}
