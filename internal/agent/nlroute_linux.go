//go:build linux

package agent

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// netlinkNeighbourReader reads the kernel neighbour table directly.
//
// It answers the same question as `ip neigh show` for the same reason the
// station reader replaced `iw`: the table has a handful of entries and the
// command costs a process. See docs/ac-runtime-cost.md.
type netlinkNeighbourReader struct{}

func newNetlinkNeighbourReader() NeighbourReader { return &netlinkNeighbourReader{} }

func (r *netlinkNeighbourReader) Name() string { return "netlink" }

func (r *netlinkNeighbourReader) Neighbours(ctx context.Context) (map[string]string, error) {
	fd, err := nlSocket(unix.NETLINK_ROUTE)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)

	seq := uint32(time.Now().UnixNano())
	req := buildGetNeighbours(afInet, seq, unix.NLM_F_REQUEST|unix.NLM_F_DUMP)

	// The frames are copies: nlDump hands out slices of one reused receive
	// buffer, and folding the dump after the loop would otherwise read whatever
	// the last datagram left behind.
	var frames [][]byte
	if err := nlDump(ctx, fd, req, func(data []byte) {
		frames = append(frames, append([]byte(nil), data...))
	}); err != nil {
		return nil, fmt.Errorf("neighbour dump: %w", err)
	}
	return parseNeighbourDump(frames), nil
}

// DebugNeighbourDump renders the raw IPv4 neighbour entries, so a mis-decoded
// attribute can be seen rather than guessed at.
func DebugNeighbourDump() (string, error) {
	fd, err := nlSocket(unix.NETLINK_ROUTE)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)
	seq := uint32(time.Now().UnixNano())
	req := buildGetNeighbours(afInet, seq, unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	var b []byte
	var frames, usable int
	seen := map[string]bool{}
	err = nlDump(context.Background(), fd, req, func(data []byte) {
		frames++
		ip, mac, ok := parseNeighbourMessage(data)
		if ok {
			usable++
			seen[mac] = true
		}
		b = append(b, []byte(fmt.Sprintf("len %d ip %q mac %q usable %v\n", len(data), ip, mac, ok))...)
	})
	b = append(b, []byte(fmt.Sprintf("# frames %d usable %d unique-mac %d\n", frames, usable, len(seen)))...)
	return string(b), err
}
