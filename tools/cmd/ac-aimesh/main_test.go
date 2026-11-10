package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/naoki66/ac-controller/shared/aimesh"
)

func TestOfflineCommandRedactsAndReportsPassive(t *testing.T) {
	l := aimesh.LLDP{ChassisID: []byte{4, 2, 0, 0, 0, 0, 1}, PortID: []byte{5, 'l', 'a', 'n', '1'}, TTL: 120,
		Fields: []aimesh.VendorField{{Subtype: 8, Value: []byte("credential-must-not-appear")}}}
	data, err := l.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	frame := append([]byte{1, 128, 194, 0, 0, 14, 2, 0, 0, 0, 0, 1, 0x88, 0xcc}, data...)
	var pcap bytes.Buffer
	for _, value := range []any{uint32(0xa1b2c3d4), uint16(2), uint16(4), uint32(0), uint32(0), uint32(65535), uint32(1), uint32(1), uint32(0), uint32(len(frame)), uint32(len(frame))} {
		if err := binary.Write(&pcap, binary.LittleEndian, value); err != nil {
			t.Fatal(err)
		}
	}
	pcap.Write(frame)
	path := filepath.Join(t.TempDir(), "capture.pcap")
	if err := os.WriteFile(path, pcap.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run([]string{"-pcap", path}, &output); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output.Bytes(), []byte("credential-must-not-appear")) {
		t.Fatal("leaked credential")
	}
	decoder := json.NewDecoder(&output)
	var first map[string]any
	if err := decoder.Decode(&first); err != nil || first["protocol"] != "asus_lldp" {
		t.Fatalf("observation %v %+v", err, first)
	}
	var summary map[string]any
	if err := decoder.Decode(&summary); err != nil || summary["joined"] != false || summary["observed"] != float64(1) {
		t.Fatalf("summary %v %+v", err, summary)
	}
}

func TestCommandRequiresUnambiguousInput(t *testing.T) {
	for _, args := range [][]string{nil, {"-pcap", "x", "-interface", "lo"}, {"-interface", "lo", "-duration", "0s"}, {"extra"}} {
		if err := run(args, new(bytes.Buffer)); err == nil {
			t.Fatalf("accepted args %v", args)
		}
	}
}
