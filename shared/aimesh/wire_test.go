package aimesh

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// 测试向量由独立提取的华硕 ELF 校验表计算得到，不依赖本包编码器。
// 样本来源及复核方法见 testdata 和 AIMESH_PROTOCOL.md。
func TestConfigReferenceVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/wire-evidence.json")
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		Vectors []struct {
			PayloadHex string `json:"payload_hex"`
			PacketHex  string `json:"packet_hex"`
		} `json:"vectors"`
	}
	if err = json.Unmarshal(raw, &evidence); err != nil {
		t.Fatal(err)
	}
	if len(evidence.Vectors) < 4 {
		t.Fatal("missing reference evidence")
	}
	for _, v := range evidence.Vectors {
		packet, err := hex.DecodeString(v.PacketHex)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := hex.DecodeString(v.PayloadHex)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := ParseConfigDatagram(packet)
		if err != nil || decoded.Type != 1 || !bytes.Equal(payload, decoded.Payload) {
			t.Fatalf("decode: %v %+v", err, decoded)
		}
		encoded, err := (ConfigDatagram{Type: 1, Payload: payload}).MarshalBinary()
		if err != nil || !bytes.Equal(encoded, packet) {
			t.Fatalf("encode does not match reference: %v %x", err, encoded)
		}
	}
}

func TestConfigRejectsMalformed(t *testing.T) {
	valid, _ := hex.DecodeString("00000001000000092dfd2d88313233343536373839")
	for _, offset := range []int{0, 4, 8, 12} {
		packet := append([]byte(nil), valid...)
		packet[offset] ^= 0x80
		if _, err := ParseConfigDatagram(packet); err == nil {
			t.Fatalf("accepted corruption at %d", offset)
		}
	}
	for _, packet := range [][]byte{nil, valid[:11], append(valid, 0), make([]byte, 2049)} {
		if _, err := ParseConfigDatagram(packet); err == nil {
			t.Fatal("accepted invalid length")
		}
	}
}

func lldpFixture(t *testing.T) []byte {
	t.Helper()
	// 手工构造 LLDP 测试帧：华硕 OUI、子类型 1 的值为 2，以及模拟密钥。
	data, err := hex.DecodeString("02070402000000000104070502000000000106020078fe08f832e40100000002fe0af832e4087365637265740000")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLLDPReferenceLayoutAndRedaction(t *testing.T) {
	fixture := lldpFixture(t)
	l, err := ParseLLDP(append(fixture, 0, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if l.TTL != 120 || len(l.Fields) != 2 || binary.BigEndian.Uint32(l.Fields[0].Value) != 2 {
		t.Fatalf("unexpected LLDP %+v", l)
	}
	encoded, err := l.MarshalBinary()
	if err != nil || !bytes.Equal(encoded, fixture) {
		t.Fatalf("LLDP layout changed: %v %x", err, encoded)
	}
	summary, _ := json.Marshal(l.Summary())
	if bytes.Contains(summary, []byte("secret")) || !bytes.Contains(summary, []byte("session_key_redacted")) {
		t.Fatalf("redaction failure %s", summary)
	}
	// 分别覆盖必需字段顺序错误、缺少结束 TLV、TLV 截断及标量宽度错误。
	for _, data := range [][]byte{fixture[9:], fixture[:len(fixture)-2], fixture[:len(fixture)-4], {2, 1, 4, 4, 1, 5, 6, 2, 0, 120, 254, 5, 248, 50, 228, 1, 2, 0, 0}} {
		if _, err := ParseLLDP(data); err == nil {
			t.Fatalf("accepted malformed LLDP %x", data)
		}
	}
}

func ethernetLLDP(t *testing.T) []byte {
	header, _ := hex.DecodeString("0180c200000e0200000000018100000788cc")
	return append(header, lldpFixture(t)...)
}

func TestEthernetVLANAndUDP(t *testing.T) {
	o, err := InspectEthernet(ethernetLLDP(t))
	if err != nil || o == nil || o.Protocol != "asus_lldp" || len(o.VLANs) != 1 || o.VLANs[0] != 7 {
		t.Fatalf("VLAN: %v %+v", err, o)
	}
	packet, _ := hex.DecodeString("0000000100000002e27fadbc7b7d")
	frame := make([]byte, 14+20+8+len(packet))
	binary.BigEndian.PutUint16(frame[12:], 0x800)
	ip := frame[14:]
	ip[0], ip[9] = 0x45, 17
	binary.BigEndian.PutUint16(ip[2:], uint16(len(ip)))
	copy(ip[12:], []byte{192, 168, 50, 1, 192, 168, 50, 2})
	udp := ip[20:]
	binary.BigEndian.PutUint16(udp, ConfigPort)
	binary.BigEndian.PutUint16(udp[2:], 12345)
	binary.BigEndian.PutUint16(udp[4:], uint16(len(udp)))
	copy(udp[8:], packet)
	o, err = InspectEthernet(frame)
	if err != nil || o == nil || o.Protocol != "asus_cfg_udp" || o.PayloadLength != 2 || o.SourceIP != "192.168.50.1" {
		t.Fatalf("UDP: %v %+v", err, o)
	}
	ip[6] = 0x20 // 存在后续分片时跳过，不推测重组后的内容。
	if o, err = InspectEthernet(frame); err != nil || o != nil {
		t.Fatal("fragment should be ignored")
	}
	ip[6], udp[5] = 0, 0
	if _, err = InspectEthernet(frame); err == nil {
		t.Fatal("accepted bad UDP size")
	}
}

func TestPCAPBoundsAndEndian(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		frame := ethernetLLDP(t)
		buf := new(bytes.Buffer)
		for _, value := range []any{uint32(0xa1b2c3d4), uint16(2), uint16(4), uint32(0), uint32(0), uint32(65535), uint32(1), uint32(10), uint32(123), uint32(len(frame)), uint32(len(frame))} {
			if err := binary.Write(buf, order, value); err != nil {
				t.Fatal(err)
			}
		}
		buf.Write(frame)
		calls := 0
		visit := func(at time.Time, data []byte) error {
			calls++
			if at.UnixNano() != 10000123000 || !bytes.Equal(data, frame) {
				t.Fatal("changed PCAP record")
			}
			return nil
		}
		if err := ReadPCAP(bytes.NewReader(buf.Bytes()), visit); err != nil || calls != 1 {
			t.Fatalf("PCAP: %v", err)
		}
		if err := ReadPCAP(bytes.NewReader(buf.Bytes()[:buf.Len()-1]), visit); err != io.ErrUnexpectedEOF {
			t.Fatalf("truncation: %v", err)
		}
		broken := append([]byte(nil), buf.Bytes()...)
		order.PutUint32(broken[32:], 0xffffffff)
		if err := ReadPCAP(bytes.NewReader(broken), visit); err == nil {
			t.Fatal("accepted unbounded allocation")
		}
	}
	if err := ReadPCAP(strings.NewReader(strings.Repeat("x", 24)), func(time.Time, []byte) error { return nil }); err == nil {
		t.Fatal("accepted invalid PCAP")
	}
}

func FuzzWireParsers(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseConfigDatagram(data)
		_, _ = ParseLLDP(data)
		_, _ = InspectEthernet(data)
		_ = ReadPCAP(bytes.NewReader(data), func(time.Time, []byte) error { return nil })
	})
}
