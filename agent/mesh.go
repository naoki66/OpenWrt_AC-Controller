package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/naoki66/ac-controller/shared/proto"
	"github.com/naoki66/ac-controller/shared/ucimerge"
)

func hasMeshBackhaul(body string) bool {
	for _, s := range ucimerge.Parse(body).Sections {
		if s.Type != "wifi-iface" || !s.IsAC() {
			continue
		}
		for _, line := range s.Lines {
			k, v, ok := uciOption(line)
			if ok && k == "mode" && v == "mesh" {
				return true
			}
		}
	}
	return false
}

func (a *Applier) waitForMeshController(ctx context.Context) error {
	// 控制器可达仅用于触发回滚，不能代替无线回程或流量路径验证。
	if a.ACAddr == "" {
		return fmt.Errorf("controller address is required for mesh rollback protection")
	}
	timeout := a.HealthTimeout
	if timeout == 0 {
		timeout = defaultHealthTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if err := a.dialAC(probeCtx, nil); err == nil {
			return nil
		}
		select {
		case <-probeCtx.Done():
			return fmt.Errorf("controller unreachable after backhaul change: %w", probeCtx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (a *Agent) collectMesh(ctx context.Context) []proto.MeshStat {
	if a.Applier == nil {
		return nil
	}
	return collectMesh(ctx, a.Applier.Runner)
}

func collectMesh(ctx context.Context, runner Runner) []proto.MeshStat {
	// 仅采集 AC 管理的 Mesh 接口，避免把回程邻居当作普通无线终端。
	if runner == nil {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	body, err := runner.Run(probeCtx, "ubus", "call", "network.wireless", "status")
	if err != nil {
		return nil
	}
	var status wirelessStatus
	if json.Unmarshal([]byte(body), &status) != nil {
		return nil
	}
	stats := make([]proto.MeshStat, 0)
	for name, r := range status {
		for _, iface := range r.Interfaces {
			if iface.Config.Mode != "mesh" || !strings.HasPrefix(iface.Section, "ac_") {
				continue
			}
			st := proto.MeshStat{Radio: name, Ifname: iface.Ifname, Up: r.Up && !r.Disabled, Peers: []proto.MeshPeer{}}
			if iface.Up != nil {
				st.Up = st.Up && *iface.Up
			}
			if st.Up && st.Ifname != "" {
				info, e := runner.Run(probeCtx, "iw", "dev", st.Ifname, "info")
				if e == nil {
					for _, line := range strings.Split(info, "\n") {
						f := strings.Fields(line)
						if len(f) == 2 && f[0] == "addr" {
							st.MAC = f[1]
						}
					}
				}
				out, e := runner.Run(probeCtx, "iw", "dev", st.Ifname, "station", "dump")
				if e != nil {
					st.Error = "mesh peer probe failed (iw required)"
				} else {
					st.Peers = parseMeshPeers(out)
				}
			}
			stats = append(stats, st)
		}
	}
	sort.Slice(stats, func(i, j int) bool { return stats[i].Radio < stats[j].Radio })
	return stats
}

func parseMeshPeers(body string) []proto.MeshPeer {
	peers := make([]proto.MeshPeer, 0)
	for _, line := range strings.Split(body, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if f[0] == "Station" {
			mac, err := net.ParseMAC(f[1])
			if err != nil {
				continue
			}
			peers = append(peers, proto.MeshPeer{MAC: mac.String()})
			continue
		}
		if len(peers) == 0 {
			continue
		}
		p := &peers[len(peers)-1]
		if f[0] == "signal:" {
			p.Signal, _ = strconv.Atoi(f[1])
		}
		if f[0] == "mesh" && len(f) >= 3 && f[1] == "plink:" {
			p.Established = f[2] == "ESTAB"
		}
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].MAC < peers[j].MAC })
	return peers
}
