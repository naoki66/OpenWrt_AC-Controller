package agent

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/naoki66/ac-controller/shared/proto"
)

// buildStationReply assembles a NL80211_CMD_NEW_STATION frame the way the
// kernel lays it out: a 4-byte generic-netlink header, then attributes, with
// the whole station record nested under NL80211_ATTR_STA_INFO.
func buildStationReply(t *testing.T, mac string, signal int8, connected, inactiveMS uint32, rate100kbps uint16) []byte {
	t.Helper()
	hw, err := net.ParseMAC(mac)
	if err != nil {
		t.Fatalf("bad mac %q: %v", mac, err)
	}
	u32 := func(v uint32) []byte {
		b := make([]byte, 4)
		binary.NativeEndian.PutUint32(b, v)
		return b
	}
	u16 := func(v uint16) []byte {
		b := make([]byte, 2)
		binary.NativeEndian.PutUint16(b, v)
		return b
	}

	staInfo := []byte{}
	staInfo = putAttr(staInfo, staInfoSignal, []byte{byte(signal)})
	staInfo = putAttr(staInfo, staInfoConnectedTime, u32(connected))
	staInfo = putAttr(staInfo, staInfoInactiveTime, u32(inactiveMS))
	staInfo = putAttr(staInfo, staInfoRxBitrate, putAttr(nil, rateInfoBitrate, u16(rate100kbps)))

	msg := []byte{nl80211CmdGetStation, nl80211GenlVersion, 0, 0}
	msg = putAttr(msg, nl80211AttrMAC, hw)
	msg = putAttr(msg, nl80211AttrStaInfo, staInfo)
	return msg
}

// The wire decoder must produce exactly the numbers the iw parser produces,
// because these fields drive steering decisions and a wrong offset would look
// plausible rather than broken.
func TestParseStationMessageMatchesIwSemantics(t *testing.T) {
	msg := buildStationReply(t, "aa:bb:cc:dd:ee:ff", -85, 120, 3500, 1444)
	st, ok := parseStationMessage(msg)
	if !ok {
		t.Fatal("station reply rejected")
	}
	if st.MAC != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("MAC = %q", st.MAC)
	}
	if st.Signal != -85 {
		t.Errorf("Signal = %d, want -85 (must decode as signed)", st.Signal)
	}
	if st.ConnectedSeconds != 120 {
		t.Errorf("ConnectedSeconds = %d, want 120", st.ConnectedSeconds)
	}
	if st.InactiveSeconds != 3 {
		t.Errorf("InactiveSeconds = %d, want 3 (3500 ms truncated like iw)", st.InactiveSeconds)
	}
	if st.RxRateMbps != 144 {
		t.Errorf("RxRateMbps = %d, want 144 (1444 x 100 kbps, rounded like iw)", st.RxRateMbps)
	}
}

// 802.11ax rates overflow u16, which is why the kernel added the 32-bit
// attribute; when both are present the wide one wins.
func TestRateMbpsPrefersTheWideAttribute(t *testing.T) {
	narrow := putAttr(nil, rateInfoBitrate, []byte{0x2c, 0x01})             // 300 -> 30 Mbps
	wide := putAttr(nil, rateInfoBitrate32, []byte{0xa4, 0x2f, 0x00, 0x00}) // 12200 -> 1220 Mbps
	both := append(append([]byte{}, narrow...), wide...)
	if got := rateMbps(both); got != 1220 {
		t.Fatalf("rateMbps = %d, want 1220 from the 32-bit attribute", got)
	}
	if got := rateMbps(narrow); got != 30 {
		t.Fatalf("rateMbps = %d, want 30 from the fallback attribute", got)
	}
	if got := rateMbps(nil); got != 0 {
		t.Fatalf("rateMbps(nil) = %d, want 0 (unknown, not a guess)", got)
	}
}

// A frame carrying no MAC (or nothing at all) must be skipped rather than
// reported as a client with an empty address.
func TestParseStationMessageRejectsIncomplete(t *testing.T) {
	if _, ok := parseStationMessage([]byte{17, 0, 0, 0}); ok {
		t.Error("a reply without NL80211_ATTR_MAC was accepted")
	}
	if _, ok := parseStationMessage(nil); ok {
		t.Error("an empty frame was accepted")
	}
}

// The dump request must carry the family, the command, and the interface index
// the kernel matches on; getting the attribute type wrong returns EINVAL at
// runtime, which is only visible on a device.
func TestBuildGetStationEncodesRequest(t *testing.T) {
	req := buildGetStation(7, 42, 0x1f, 0x301)
	if len(req) < 16 {
		t.Fatalf("request is %d bytes", len(req))
	}
	total := binary.NativeEndian.Uint32(req[0:4])
	if int(total) != len(req) {
		t.Errorf("nlmsg length = %d, header says %d", len(req), total)
	}
	if typ := binary.NativeEndian.Uint16(req[4:6]); typ != 0x1f {
		t.Errorf("nlmsg type = %d, want the resolved family 31", typ)
	}
	if cmd := req[16]; cmd != nl80211CmdGetStation {
		t.Errorf("genl command = %d, want %d", cmd, nl80211CmdGetStation)
	}
	attrLen := int(binary.NativeEndian.Uint16(req[20:22]))
	attrType := binary.NativeEndian.Uint16(req[22:24])
	if attrType != nl80211AttrIfindex || attrLen != 8 {
		t.Fatalf("first attribute type=%d len=%d, want ifindex(3)/8", attrType, attrLen)
	}
	if idx := binary.NativeEndian.Uint32(req[24:28]); idx != 7 {
		t.Errorf("ifindex = %d, want 7", idx)
	}
}

// Two frames arriving in one buffer (the normal case for a dump) must both be
// decoded, including the 4-byte padding between them.
func TestParseNetlinkMessagesHandlesMultipleFrames(t *testing.T) {
	mk := func(typ uint16, payload []byte) []byte {
		hdr := make([]byte, 16)
		binary.NativeEndian.PutUint32(hdr[0:], uint32(16+len(payload)))
		binary.NativeEndian.PutUint16(hdr[4:], typ)
		frame := append(hdr, payload...)
		// The kernel pads every frame to a 4-byte boundary but keeps the
		// unpadded length in the header, which is exactly the asymmetry the
		// parser has to handle.
		for len(frame)%4 != 0 {
			frame = append(frame, 0)
		}
		return frame
	}
	buf := append(mk(10, []byte{1, 2, 3}), mk(11, []byte{4, 5, 6, 7})...)
	buf = append(buf, mk(12, nil)...)
	msgs := parseNetlinkMessages(buf)
	if len(msgs) != 3 {
		t.Fatalf("decoded %d frames, want 3", len(msgs))
	}
	if msgs[0].typ != 10 || msgs[1].typ != 11 || msgs[2].typ != 12 {
		t.Fatalf("types = %d/%d/%d", msgs[0].typ, msgs[1].typ, msgs[2].typ)
	}
	if len(msgs[0].data) != 3 || len(msgs[1].data) != 4 {
		t.Fatalf("payload lengths = %d/%d", len(msgs[0].data), len(msgs[1].data))
	}
}

// The default source is netlink where it exists, but the resolver must never
// return nil: a missing telemetry source is worse than a slow one.
func TestResolveStationSourceAlwaysReturnsAReader(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, mode := range []string{"", "auto", "nl80211", "iw", "bogus"} {
		r := resolveStationSource(mode, &fakeRunner{}, log)
		if r == nil {
			t.Fatalf("mode %q resolved to nil", mode)
		}
		if mode == "iw" && r.Name() != "iw" {
			t.Fatalf("mode iw resolved to %q", r.Name())
		}
	}
}

// failingReader stands in for a netlink reader the kernel rejects.
type failingReader struct{ calls int }

func (f *failingReader) Name() string { return "nl80211" }
func (f *failingReader) Stations(context.Context, string, int) ([]proto.ClientStat, error) {
	f.calls++
	return nil, ErrStationSourceUnsupported
}

// After a source fails the collector must switch to iw for the rest of the
// process: a kernel that rejects one dump will reject the next eleven, and
// paying a failing syscall per BSS per cycle is worse than the spawn it replaced.
func TestCollectorFallsBackToIwAfterSourceFailure(t *testing.T) {
	runner := &fakeRunner{replies: map[string]string{
		"iw dev phy0.1-ap0 station dump": "Station aa:bb:cc:dd:ee:ff\n signal: -70\n connected time: 10 seconds\n",
	}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	bad := &failingReader{}
	c := &ClientCollector{Runner: runner, Log: log, reader: bad}

	bss := bssInfo{Ifname: "phy0.1-ap0", SSID: "SSID-A", Band: "5g"}
	if list, err := c.stations(context.Background(), bss, nil); err != nil || len(list) != 1 {
		t.Fatalf("first read should fall back to iw: %+v, %v", list, err)
	}
	if got := c.stationReader().Name(); got != "iw" {
		t.Fatalf("source after failure = %q, want iw", got)
	}

	list, err := c.stations(context.Background(), bss, nil)
	if err != nil || len(list) != 1 {
		t.Fatalf("stations after fallback = %+v, %v", list, err)
	}
	if list[0].SSID != "SSID-A" || list[0].Band != "5g" || list[0].Ifname != "phy0.1-ap0" {
		t.Errorf("context not filled in: %+v", list[0])
	}
	if bad.calls != 1 {
		t.Errorf("failed source was consulted %d times, want 1", bad.calls)
	}
}

// The kernel tags nested attributes with NLA_F_NESTED (0x8000): STA_INFO
// arrives as 32789. Comparing the raw type never matches, and because the
// failure mode is a plausible zero rather than an error, it went unnoticed
// until the on-device raw dump was read.
func TestParseStationMessageHandlesNestedFlag(t *testing.T) {
	hw, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	u32 := func(v uint32) []byte {
		b := make([]byte, 4)
		binary.NativeEndian.PutUint32(b, v)
		return b
	}
	signed := func(v int8) []byte { return []byte{byte(v)} }

	staInfo := []byte{}
	staInfo = putAttr(staInfo, staInfoSignal, signed(-70))
	staInfo = putAttr(staInfo, staInfoConnectedTime, u32(60))
	staInfo = putAttr(staInfo, staInfoInactiveTime, u32(1200))
	staInfo = putAttr(staInfo, staInfoRxBitrate, putAttr(nil, rateInfoBitrate32, u32(86670)))

	body := []byte{nl80211CmdGetStation, nl80211GenlVersion, 0, 0}
	body = putAttr(body, nl80211AttrMAC, hw)
	body = putAttr(body, nl80211AttrStaInfo|0x8000, staInfo)

	st, ok := parseStationMessage(body)
	if !ok {
		t.Fatal("nested-flagged reply rejected")
	}
	if st.Signal != -70 || st.ConnectedSeconds != 60 || st.InactiveSeconds != 1 || st.RxRateMbps != 8667 {
		t.Fatalf("decoded %+v, want signal -70 / 60 s / 1 s / 8667 Mbps", st)
	}
}
