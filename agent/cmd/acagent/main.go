// Command acagent is the access-point side of the provisioning loop: it tells
// the controller what it is running, pulls the revision the controller wants,
// verifies it, applies it, and reports the outcome.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/naoki66/ac-controller/agent"
	"github.com/naoki66/ac-controller/shared/proto"
	"github.com/naoki66/ac-controller/shared/version"
)

func main() {
	var (
		verbose     = flag.Bool("v", false, "enable debug logging")
		acAddr      = flag.String("ac", "", "controller address host:port (empty = discover via broadcast)")
		secret      = flag.String("secret", "", "shared secret (or AC_SECRET)")
		root        = flag.String("root", "", "filesystem root, defaults to / (or AC_ROOT)")
		dryRun      = flag.Bool("dry-run", false, "render and stage only, never touch /etc/config")
		interval    = flag.Duration("interval", 0, "sync interval (or AC_INTERVAL)")
		mac         = flag.String("mac", "", "override local MAC (used when /sys is unavailable)")
		model       = flag.String("model", "", "override model")
		keepDHCP    = flag.Bool("keep-dhcp", false, "do NOT stop dnsmasq/odhcpd (AP mode must not serve DHCP)")
		peerPort    = flag.Int("peer-port", 0, "AP-to-AP coordination UDP port (0 = AC_PEER_PORT/29821, <0 disables)")
		peerSteer   = flag.Bool("peer-steer", false, "suggest roaming locally when a client is weak and a peer has the same SSID (or AC_PEER_STEER)")
		peerNetwork = flag.String("peer-network", "", "OpenWrt logical network carrying AP-to-AP traffic (or AC_PEER_NETWORK/lan)")
		jsonOut     = flag.Bool("json", false, "with status: print the machine-readable status LuCI reads")
	)

	// Accept the subcommand anywhere: procd starts the agent as
	// "ac-agent run --ac 10.0.0.1:8090", while a human types
	// "ac-agent --ac 10.0.0.1:8090 once". Strip the first bare word and let the
	// flag package see only options. Flags that take a separate value must be
	// known here, otherwise their value would be mistaken for the subcommand.
	cmd := "run"
	args := os.Args[1:]
	valueFlags := map[string]bool{
		"-ac": true, "--ac": true,
		"-secret": true, "--secret": true,
		"-root": true, "--root": true,
		"-interval": true, "--interval": true,
		"-mac": true, "--mac": true,
		"-model": true, "--model": true,
		"-peer-port": true, "--peer-port": true,
		"-peer-network": true, "--peer-network": true,
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			if valueFlags[arg] {
				i++ // skip the flag's value
			}
			continue
		}
		cmd = arg
		args = append(append([]string{}, args[:i]...), args[i+1:]...)
		break
	}
	if err := flag.CommandLine.Parse(args); err != nil {
		os.Exit(2)
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if cmd == "version" {
		os.Stdout.WriteString("acagent " + version.Version + " (" + version.Commit + ", " + version.Date + ")\n")
		return
	}

	cfg := agent.Load(agent.Config{
		Root:        *root,
		ACAddr:      *acAddr,
		Secret:      *secret,
		Interval:    *interval,
		DryRun:      *dryRun,
		KeepDHCP:    *keepDHCP,
		PeerPort:    *peerPort,
		PeerSteer:   *peerSteer,
		PeerNetwork: *peerNetwork,
		Identity:    agent.Identity{MAC: *mac, Model: *model},
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := agent.New(cfg, log)
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}

	switch cmd {
	case "run":
		if err := a.Run(ctx); err != nil {
			log.Error("fatal", "err", err)
			os.Exit(1)
		}
	case "once":
		// A single cycle is what a human runs after editing a template, and what
		// the smoke test uses against a local controller.
		if err := a.Step(ctx); err != nil {
			log.Error("sync cycle failed", "err", err)
			os.Exit(1)
		}
	case "stations":
		// One raw station sample, in whichever source AC_STATION_SOURCE
		// selects. Exists to compare nl80211 against iw on a live AP: the two
		// must agree field by field before netlink is trusted as the default,
		// and a wrong attribute offset would otherwise look plausible.
		// AC_STATIONS_REPEAT exists so the cost of one sample can be measured
		// inside a single process: comparing two binaries' start-up time would
		// drown the difference being measured.
		repeat := envRepeat("AC_STATIONS_REPEAT")
		var list []proto.ClientStat
		for i := 0; i < repeat; i++ {
			list = a.Clients.Snapshot(ctx, 0)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		payload := map[string]any{
			"source":  cfg.StationSource,
			"repeat":  repeat,
			"clients": list,
		}
		if err := enc.Encode(payload); err != nil {
			log.Error("encode stations", "err", err)
			os.Exit(1)
		}
	case "nl-dump":
		// Raw nl80211 reply for one BSS, used to pin attribute numbers against
		// what the kernel actually sends.
		if len(os.Args) < 3 {
			log.Error("nl-dump needs an interface name", "hint", "ac-agent nl-dump phy0.1-ap0")
			os.Exit(2)
		}
		out, err := agent.DebugStationAttrs(os.Args[2])
		if err != nil {
			log.Error("nl-dump failed", "err", err)
			os.Exit(1)
		}
		fmt.Print(out)
	case "nl-ifaces":
		// Raw NL80211_CMD_GET_INTERFACE reply. Same purpose as nl-dump for the
		// station table: the BSS list moved off ubus, and the attribute numbers
		// had to be seen rather than trusted.
		out, err := agent.DebugInterfaceAttrs()
		if err != nil {
			log.Error("nl-ifaces failed", "err", err)
			os.Exit(1)
		}
		fmt.Print(out)
	case "neigh":
		// Decoded rtnetlink neighbour entries, to compare against
		// `ip neigh show` on a live AP.
		if *jsonOut {
			table, err := a.Clients.Neighbours(ctx)
			if err != nil {
				log.Error("neigh failed", "err", err)
				os.Exit(1)
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(table); err != nil {
				log.Error("encode neigh", "err", err)
				os.Exit(1)
			}
			return
		}
		out, err := agent.DebugNeighbourDump()
		if err != nil {
			log.Error("neigh failed", "err", err)
			os.Exit(1)
		}
		fmt.Print(out)
	case "ubus-dump":
		// One ubus call through the agent's own socket client, printed as JSON
		// so it can be diffed against `ubus call <obj> <method>` on the same
		// board. This is how the wire format was verified: a wrong attribute
		// number or a lost container level looks like a plausible empty reply,
		// not like an error.
		if len(os.Args) < 4 {
			log.Error("ubus-dump needs an object and a method",
				"hint", "ac-agent ubus-dump network.wireless status")
			os.Exit(2)
		}
		out, err := a.DebugUbusCall(ctx, os.Args[2], os.Args[3])
		if err != nil {
			log.Error("ubus-dump failed", "err", err)
			os.Exit(1)
		}
		var buf bytes.Buffer
		if err := json.Indent(&buf, out, "", "\t"); err != nil {
			fmt.Println(string(out))
			return
		}
		fmt.Println(buf.String())
	case "radios":
		// One radio sample, repeated, with the per-sample cost printed. Same
		// purpose as `stations`: measuring inside a single process keeps process
		// start-up out of the number, which is the only way the difference
		// between the ubus CLI and the socket path (a few milliseconds) is
		// visible rather than drowned. AC_UBUS selects the path, so one binary
		// measures both.
		repeat := envRepeat("AC_RADIOS_REPEAT")
		start := time.Now()
		var list []proto.RadioStat
		for i := 0; i < repeat; i++ {
			list = a.Radios.Collect(ctx, nil)
		}
		perSample := time.Since(start) / time.Duration(repeat)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		payload := map[string]any{
			"ubus_socket": cfg.Ubus,
			"repeat":      repeat,
			"sample_ms":   float64(perSample.Microseconds()) / 1000,
			"radios":      list,
		}
		if err := enc.Encode(payload); err != nil {
			log.Error("encode radios", "err", err)
			os.Exit(1)
		}
	case "neighbors":
		// One 11k self-report pass, repeated, with the per-pass cost printed.
		// Same purpose as `radios`: the pass is 11 ubus calls, and only an
		// in-process measurement shows what each transport really costs.
		repeat := envRepeat("AC_NEIGHBORS_REPEAT")
		start := time.Now()
		var list []proto.NeighborBSS
		for i := 0; i < repeat; i++ {
			list = a.Neighbors.Collect(ctx)
		}
		perPass := time.Since(start) / time.Duration(repeat)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		payload := map[string]any{
			"ubus_socket": cfg.Ubus,
			"repeat":      repeat,
			"pass_ms":     float64(perPass.Microseconds()) / 1000,
			"bss":         list,
		}
		if err := enc.Encode(payload); err != nil {
			log.Error("encode neighbors", "err", err)
			os.Exit(1)
		}
	case "status":
		if *jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(a.StatusSnapshot()); err != nil {
				log.Error("encode status", "err", err)
				os.Exit(1)
			}
			return
		}
		printStatus(a)
	default:
		log.Error("unknown command", "cmd", cmd, "hint", "supported: run, once, status, stations, version")
		os.Exit(2)
	}
}

// envRepeat reads a repeat count from the environment, defaulting to one so a
// human running the command gets a single sample.
func envRepeat(name string) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 1
}

func printStatus(a *agent.Agent) {
	st := a.State()
	snap := a.StatusSnapshot()
	fmt.Printf("state        %s\n", snap.State)
	fmt.Printf("ac_addr      %s", snap.ACAddr)
	if snap.ACAddrSource != "" {
		fmt.Printf(" (%s)", snap.ACAddrSource)
	}
	fmt.Printf("\n")
	fmt.Printf("mac          %s\n", a.Identity.MAC)
	fmt.Printf("model        %s\n", a.Identity.Model)
	fmt.Printf("firmware     %s\n", a.Identity.Firmware)
	fmt.Printf("applied_rev  %d\n", st.AppliedRev)
	fmt.Printf("desired_rev  %d\n", snap.DesiredRev)
	if snap.SHA256 != "" {
		fmt.Printf("sha256       %s\n", snap.SHA256)
	}
	if st.AppliedAt != "" {
		fmt.Printf("applied_at   %s\n", st.AppliedAt)
	}
	if st.ACID != "" {
		fmt.Printf("ac_id        %s\n", st.ACID)
	}
	if st.PubKey != "" {
		fmt.Printf("pubkey       %s\n", st.PubKey)
	}
	if snap.LastHelloAt != "" {
		fmt.Printf("last_hello   %s (ok=%v)\n", snap.LastHelloAt, snap.LastHelloOK)
	}
	if snap.LastHelloError != "" {
		fmt.Printf("hello_error  %s\n", snap.LastHelloError)
	}
	if st.PendingSync != "" {
		fmt.Printf("pending      %s (%s)\n", st.PendingSync, st.PendingSyncMsg)
	}
	if st.LastError != "" {
		fmt.Printf("last_error   %s\n", st.LastError)
	}
	fmt.Printf("peer_port    %d\n", a.Config.PeerPort)
	fmt.Printf("peer_network %s\n", a.Config.PeerNetwork)
	fmt.Printf("peer_auth    %v\n", a.Config.Secret != "")
	fmt.Printf("peer_steer   %v\n", a.Config.PeerSteer)
	fmt.Printf("steering     %s (mode %s", snap.Steering, snap.SteeringMode)
	if snap.SteeringFallback != "" && snap.SteeringFallback != "none" {
		fmt.Printf(", fallback %s", snap.SteeringFallback)
	}
	if snap.SteeringFallbackActive {
		fmt.Printf(", FALLBACK ACTIVE")
	}
	fmt.Printf(")\n")
}
