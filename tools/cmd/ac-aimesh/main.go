// ac-aimesh 收集已脱敏的协议观测结果，用于 AiMesh 混组适配与排障。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/naoki66/ac-controller/shared/aimesh"
)

type event struct {
	Time time.Time `json:"time"`
	*aimesh.Observation
}

func run(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("ac-aimesh", flag.ContinueOnError)
	pcap := flags.String("pcap", "", "read classic Ethernet PCAP file")
	iface := flags.String("interface", "", "passively listen on a Linux LAN interface")
	duration := flags.Duration("duration", 30*time.Second, "live observation duration (1s to 10m)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*pcap == "") == (*iface == "") {
		return errors.New("choose exactly one: -pcap FILE or -interface LAN_INTERFACE")
	}
	encoder := json.NewEncoder(output)
	var frames, observed, malformed uint64
	visit := func(at time.Time, frame []byte) error {
		frames++
		observation, err := aimesh.InspectEthernet(frame)
		if err != nil {
			malformed++
			return nil
		}
		if observation == nil {
			return nil
		}
		observed++
		return encoder.Encode(event{Time: at, Observation: observation})
	}
	if *pcap != "" {
		file, err := os.Open(*pcap)
		if err != nil {
			return err
		}
		defer file.Close()
		if err = aimesh.ReadPCAP(file, visit); err != nil {
			return err
		}
	} else {
		if *duration < time.Second || *duration > 10*time.Minute {
			return errors.New("duration must be between 1s and 10m")
		}
		if err := listen(*iface, *duration, visit); err != nil {
			return err
		}
	}
	return encoder.Encode(struct {
		Kind      string `json:"kind"`
		Frames    uint64 `json:"frames"`
		Observed  uint64 `json:"observed"`
		Malformed uint64 `json:"malformed"`
		Joined    bool   `json:"joined"`
	}{"passive_summary", frames, observed, malformed, false})
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
