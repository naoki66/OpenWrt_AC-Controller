package agent

import (
	"context"
	"errors"
	"log/slog"

	"github.com/naoki66/ac-controller/internal/proto"
)

// Station source selection.
//
// Reading the station table used to mean one `iw dev <bss> station dump` per
// BSS, and fork+exec — not the netlink round trip — was 6 ms of the agent's
// per-cycle budget. On an eleven-BSS AP that is the single most expensive
// periodic operation the agent performs (docs/ac-runtime-cost.md).
//
// netlink replaces it with one dump per BSS that costs a syscall pair instead
// of a process. It is not the default without proof, though: the attribute
// layout is a kernel ABI that iw had to get right, and a mis-parsed offset
// would not fail loudly — it would quietly report the wrong signal for every
// client. So the source is selectable, `auto` verifies on first use, and a
// mismatch falls back to iw rather than degrading telemetry.

// StationReader reads the associated stations of one BSS.
//
// ifindex is a hint: a reader that can address the interface directly uses it
// and skips a name lookup, which on Linux means a whole RTM_GETLINK dump. A
// zero means "not known", never "no interface".
type StationReader interface {
	Stations(ctx context.Context, ifname string, ifindex int) ([]proto.ClientStat, error)
	// Name is for logs and the status file, never for decisions.
	Name() string
}

// ErrStationSourceUnsupported means the platform has no implementation; the
// collector treats it exactly like a failed reader and falls back.
var ErrStationSourceUnsupported = errors.New("station source unsupported on this platform")

// iwReader is the reference implementation: it shells out to iw and parses its
// human-readable output. Slow, but it is what every field above was verified
// against.
type iwReader struct{ runner Runner }

func (r iwReader) Name() string { return "iw" }

func (r iwReader) Stations(ctx context.Context, ifname string, _ int) ([]proto.ClientStat, error) {
	out, err := r.runner.Run(ctx, "iw", "dev", ifname, "station", "dump")
	if err != nil {
		return nil, err
	}
	return parseStationDump(out), nil
}

// stationSourceMode is the configured preference.
const (
	stationSourceAuto    = "auto"
	stationSourceNetlink = "nl80211"
	stationSourceIW      = "iw"
)

// resolveStationSource builds the reader for the configured mode. An explicit
// request that the platform cannot honour degrades to iw instead of returning
// nil: a missing telemetry source would be worse than a slow one.
func resolveStationSource(mode string, runner Runner, log *slog.Logger) StationReader {
	fallback := iwReader{runner: runner}
	switch mode {
	case "", stationSourceAuto, stationSourceNetlink:
	default:
		if log != nil {
			log.Warn("unknown station source, using iw", "source", mode)
		}
		return fallback
	}
	r := newNetlinkStationReader()
	if r == nil {
		if mode == stationSourceNetlink && log != nil {
			log.Warn("nl80211 unavailable on this platform, using iw")
		}
		return fallback
	}
	return r
}
