//go:build !linux

package agent

// newNetlinkStationReader returns nil off Linux: the agent only runs on
// OpenWrt, but the package still has to build (and its tests run) on the
// developer's machine.
func newNetlinkStationReader() StationReader { return nil }

// newNetlinkBSSReader returns nil for the same reason.
func newNetlinkBSSReader() BSSReader { return nil }

// newNetlinkNeighbourReader returns nil for the same reason.
func newNetlinkNeighbourReader() NeighbourReader { return nil }

// DebugStationAttrs is unavailable off Linux; the agent only runs on OpenWrt.
func DebugStationAttrs(string) (string, error) { return "", ErrStationSourceUnsupported }

// DebugInterfaceAttrs is unavailable off Linux.
func DebugInterfaceAttrs() (string, error) { return "", ErrStationSourceUnsupported }

// DebugNeighbourDump is unavailable off Linux.
func DebugNeighbourDump() (string, error) { return "", ErrStationSourceUnsupported }
