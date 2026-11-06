package render

import (
	"encoding/json"
	"testing"
)

// Two APs, each serving SSID-A on 5G and a private SSID. The lists must only ever
// name a sibling that carries the same SSID: offering a client a BSS it cannot
// join makes the transition fail and teaches it to ignore the report.
func TestNeighborsOnlyAdvertisesSameSSID(t *testing.T) {
	entries := []NeighborEntry{
		{APMAC: "ap1", Ifname: "phy0.1-ap0", BSSID: "00:00:00:00:01:01", SSID: "SSID-A", Report: "r11"},
		{APMAC: "ap1", Ifname: "phy0.1-ap1", BSSID: "02:00:00:00:01:01", SSID: "SSID-B", Report: "r12"},
		{APMAC: "ap2", Ifname: "phy0.1-ap0", BSSID: "00:00:00:00:02:01", SSID: "SSID-A", Report: "r21"},
		{APMAC: "ap2", Ifname: "phy0.1-ap1", BSSID: "02:00:00:00:02:01", SSID: "SSID-B", Report: "r22"},
	}

	got := decode(t, Neighbors(entries, "ap1"))

	home := got["phy0.1-ap0"]
	if len(home) != 1 || home[0][0] != "00:00:00:00:02:01" {
		t.Fatalf("SSID-A list = %v, want only the peer AP's SSID-A BSS", home)
	}
	if home[0][2] != "r21" {
		t.Errorf("report body = %q, want the peer's verbatim report", home[0][2])
	}
	oplt := got["phy0.1-ap1"]
	if len(oplt) != 1 || oplt[0][0] != "02:00:00:00:02:01" {
		t.Errorf("SSID-B list = %v", oplt)
	}
}

// hostapd appends the BSS's own entry when it answers a Neighbor Report
// request, so advertising it in the list would duplicate it on the air.
func TestNeighborsExcludesSelf(t *testing.T) {
	entries := []NeighborEntry{
		{APMAC: "ap1", Ifname: "phy0.1-ap0", BSSID: "00:00:00:00:01:01", SSID: "SSID-A", Report: "r11"},
	}
	if got := Neighbors(entries, "ap1"); got != nil {
		t.Errorf("single AP produced a list: %s", got)
	}
}

// The first AP on a network has no siblings. Emitting an empty list would make
// the agent write over hostapd for no reason, so the file must be absent.
func TestNeighborsEmptyWhenNoPeers(t *testing.T) {
	entries := []NeighborEntry{
		{APMAC: "ap1", Ifname: "phy0.1-ap0", BSSID: "00:00:00:00:01:01", SSID: "SSID-A", Report: "r11"},
		{APMAC: "ap2", Ifname: "phy0.1-ap0", BSSID: "00:00:00:00:02:01", SSID: "OTHER", Report: "r21"},
	}
	if got := Neighbors(entries, "ap1"); got != nil {
		t.Errorf("no shared SSID but a list was produced: %s", got)
	}
}

// Two identical inputs must produce identical bytes, or every revision churns.
func TestNeighborsIsDeterministic(t *testing.T) {
	entries := []NeighborEntry{
		{APMAC: "ap1", Ifname: "phy0.2-ap0", BSSID: "00:00:00:00:01:02", SSID: "SSID-A", Report: "r12"},
		{APMAC: "ap2", Ifname: "phy0.1-ap0", BSSID: "00:00:00:00:02:01", SSID: "SSID-A", Report: "r21"},
		{APMAC: "ap3", Ifname: "phy0.1-ap0", BSSID: "00:00:00:00:03:01", SSID: "SSID-A", Report: "r31"},
	}

	first := Neighbors(entries, "ap1")
	for i := 0; i < 10; i++ {
		// Shuffle the input order; the output must not care.
		rotated := append(append([]NeighborEntry{}, entries[1:]...), entries[0])
		if got := Neighbors(rotated, "ap1"); string(got) != string(first) {
			t.Fatalf("output changed with input order:\n%s\n%s", first, got)
		}
	}
	if first[len(first)-1] != '\n' {
		t.Error("expected a trailing newline, so the file ends like every other UCI-adjacent file")
	}
}

func decode(t *testing.T, body []byte) map[string][][]string {
	t.Helper()
	if body == nil {
		t.Fatal("no neighbours produced")
	}
	var m map[string][][]string
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	return m
}
