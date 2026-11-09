package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/naoki66/ac-controller/shared/model"
	"github.com/naoki66/ac-controller/shared/proto"
)

// NTP configuration delivery.
//
// The controller is the natural place to declare the site's time sources: one
// list applies to every AP, and it is not the AP's job to guess a reachable
// server. The list is applied to /etc/config/system and persisted in the agent
// state, so an AC outage does not stop the NTP client that is already running.
// The peer channel is independent of this: it keeps carrying 11k data even when
// every AP is on its own time source.

// applyNTP applies a controller-managed server list. An empty or absent list
// is not authoritative: it leaves the AP's current configuration alone.
func (a *Agent) applyNTP(ctx context.Context, servers []string) error {
	list := model.NormalizeNTPServers(servers)
	if len(list) == 0 {
		return nil
	}
	if slices.Equal(a.state.NTPServers, list) {
		return nil
	}
	if a.Runner == nil {
		return fmt.Errorf("ntp: no runner")
	}

	if _, err := a.Runner.Run(ctx, "uci", "-q", "set", "system.ntp=timeserver"); err != nil {
		return fmt.Errorf("ntp: set system.ntp: %w", err)
	}
	// A missing list is normal on a fresh image; deleting it is best effort.
	if _, err := a.Runner.Run(ctx, "uci", "-q", "delete", "system.ntp.server"); err != nil {
		a.Log.Debug("NTP server list was already empty", "err", err)
	}
	for _, server := range list {
		if _, err := a.Runner.Run(ctx, "uci", "-q", "add_list", "system.ntp.server="+server); err != nil {
			return fmt.Errorf("ntp: add_list %s: %w", server, err)
		}
	}
	if _, err := a.Runner.Run(ctx, "uci", "-q", "commit", "system"); err != nil {
		return fmt.Errorf("ntp: commit system: %w", err)
	}
	// Restarting is best effort: the UCI file is already correct, so a missing
	// service must not make the next hello retry the same write forever.
	if _, err := a.Runner.Run(ctx, "/etc/init.d/sysntpd", "restart"); err != nil {
		a.Log.Warn("NTP service restart failed", "err", err)
	}

	a.state.NTPServers = list
	if err := a.saveState(); err != nil {
		return fmt.Errorf("ntp: save state: %w", err)
	}
	a.Log.Info("NTP servers applied", "servers", strings.Join(list, ","))
	return nil
}

// applyNTPFromReply is the hello-path wrapper: a failure is reported but does
// not fail provisioning, because time sync is an independent axis.
func (a *Agent) applyNTPFromReply(ctx context.Context, reply proto.HelloReply) {
	if len(reply.NTP) == 0 {
		return
	}
	if err := a.applyNTP(ctx, reply.NTP); err != nil {
		a.Log.Warn("controller NTP setting not applied", "err", err)
	}
}
