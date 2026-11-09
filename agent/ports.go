package agent

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/naoki66/ac-controller/shared/proto"
)

// physicalPort matches the netdev names OpenWrt exposes for wired ports: DSA
// user ports (lan1..n, wan), CPU/ethernet devices (eth0) and switchdev ports
// (swp*/p*). Wireless and virtual interfaces (br-*, wlan*, phy*, veth*) are
// deliberately excluded.
var physicalPort = regexp.MustCompile(`^(eth\d+|lan\d+|wan|swp\d+|p\d+)$`)

// collectPorts reads the kernel's view of every wired port from sysfs. It
// returns nil when sysfs is unreadable, which the controller interprets as
// "this agent does not collect port data" rather than "this AP has no ports".
func collectPorts(root string) []proto.PortStat {
	base := filepath.Join(root, "sys/class/net")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	out := make([]proto.PortStat, 0, len(entries))
	for _, e := range entries {
		// /sys/class/net/<if> is a symlink to the device directory, so IsDir()
		// is false; the name is the only thing this collector needs.
		name := e.Name()
		if !physicalPort.MatchString(name) {
			continue
		}
		dir := filepath.Join(base, name)
		p := proto.PortStat{
			Name:   name,
			MAC:    portField(dir, "address"),
			Up:     portField(dir, "carrier") == "1",
			Duplex: portField(dir, "duplex"),
		}
		// Some drivers omit carrier on ports that are down; operstate is the
		// fallback so a linked port is never reported as down.
		if !p.Up {
			p.Up = portField(dir, "operstate") == "up"
		}
		if n, err := strconv.Atoi(portField(dir, "speed")); err == nil && n > 0 {
			p.SpeedMbps = n
		}
		if n, err := strconv.Atoi(portField(dir, "mtu")); err == nil {
			p.MTU = n
		}
		out = append(out, p)
	}
	return out
}

func portField(dir, file string) string {
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
