package agent

import (
	"context"
	"log/slog"
)

// Neighbour table selection.
//
// The collector attaches a client's IPv4 address when the kernel happens to know
// it. On a bridged AP that is opportunistic — the AP never speaks L3 with its
// clients — but not worthless: a client that ARPs for the gateway, or that the
// AP itself talks to, does leave an entry. The reference site had two usable
// IPv4 entries out of 58 neighbour lines, the rest being IPv6 link-local router
// entries, and `ip neigh show` cost 6 ms to produce them.
//
// So: read the same table over rtnetlink with family AF_INET, which makes the
// kernel do the filtering. The IPv6 noise never crosses the socket.

// NeighbourReader returns MAC→IPv4.
type NeighbourReader interface {
	Neighbours(ctx context.Context) (map[string]string, error)
	Name() string
}

// ipNeighbourReader is the reference implementation: `ip neigh show` parsed as
// text. Slow, but it is what the netlink reader is compared against.
type ipNeighbourReader struct{ runner Runner }

func (r ipNeighbourReader) Name() string { return "ip" }

func (r ipNeighbourReader) Neighbours(ctx context.Context) (map[string]string, error) {
	out, err := r.runner.Run(ctx, "ip", "neigh", "show")
	if err != nil {
		return nil, err
	}
	return parseNeighbours(out), nil
}

// resolveNeighbourReader builds the reader. Neighbours are best-effort, so an
// unavailable netlink silently keeps the text path; there is nothing here worth
// a warning.
func resolveNeighbourReader(runner Runner, netlink bool, log *slog.Logger) NeighbourReader {
	fallback := ipNeighbourReader{runner: runner}
	if !netlink {
		return fallback
	}
	r := newNetlinkNeighbourReader()
	if r == nil {
		if log != nil {
			log.Debug("neighbours: netlink unavailable on this platform, using ip")
		}
		return fallback
	}
	return r
}
