package aimesh

import (
	"encoding/binary"
	"errors"
	"net"
)

// Observation 表示已脱敏的协议观测结果，不代表设备已成功配对。
type Observation struct {
	Protocol        string         `json:"protocol"`
	SourceMAC       string         `json:"source_mac"`
	DestinationMAC  string         `json:"destination_mac"`
	VLANs           []uint16       `json:"vlans,omitempty"`
	TTL             *uint16        `json:"ttl,omitempty"`
	Fields          []FieldSummary `json:"fields,omitempty"`
	SourceIP        string         `json:"source_ip,omitempty"`
	DestinationIP   string         `json:"destination_ip,omitempty"`
	SourcePort      uint16         `json:"source_port,omitempty"`
	DestinationPort uint16         `json:"destination_port,omitempty"`
	MessageType     uint32         `json:"message_type,omitempty"`
	PayloadLength   int            `json:"payload_length,omitempty"`
}

// InspectEthernet 识别华硕 LLDP 和 IPv4 上的配置 UDP 报文。
// 当前不进行 IP 分片重组、TCP 流重组或凭据载荷解密。
// 与目标协议无关的流量返回 (nil, nil)。
func InspectEthernet(frame []byte) (*Observation, error) {
	if len(frame) < 14 {
		return nil, errors.New("short Ethernet frame")
	}
	out := &Observation{SourceMAC: net.HardwareAddr(frame[6:12]).String(), DestinationMAC: net.HardwareAddr(frame[:6]).String()}
	kind := binary.BigEndian.Uint16(frame[12:14])
	data := frame[14:]
	for depth := 0; kind == 0x8100 || kind == 0x88a8; depth++ {
		if depth >= 2 || len(data) < 4 {
			return nil, errors.New("invalid VLAN header")
		}
		out.VLANs = append(out.VLANs, binary.BigEndian.Uint16(data)&4095)
		kind = binary.BigEndian.Uint16(data[2:])
		data = data[4:]
	}
	switch kind {
	case 0x88cc:
		l, err := ParseLLDP(data)
		if err != nil {
			return nil, err
		}
		if len(l.Fields) == 0 {
			return nil, nil
		}
		out.Protocol, out.TTL, out.Fields = "asus_lldp", &l.TTL, l.Summary()
		return out, nil
	case 0x0800:
		if len(data) < 20 || data[0]>>4 != 4 {
			return nil, errors.New("short or invalid IPv4 header")
		}
		ihl, size := int(data[0]&15)*4, int(binary.BigEndian.Uint16(data[2:]))
		if ihl < 20 || size < ihl || size > len(data) {
			return nil, errors.New("invalid IPv4 length")
		}
		if data[9] != 17 {
			return nil, nil
		}
		if binary.BigEndian.Uint16(data[6:])&0x3fff != 0 {
			return nil, nil
		}
		udp := data[ihl:size]
		if len(udp) < 8 {
			return nil, errors.New("short UDP header")
		}
		out.SourcePort, out.DestinationPort = binary.BigEndian.Uint16(udp), binary.BigEndian.Uint16(udp[2:])
		if out.SourcePort != ConfigPort && out.DestinationPort != ConfigPort {
			return nil, nil
		}
		if int(binary.BigEndian.Uint16(udp[4:])) != len(udp) {
			return nil, errors.New("invalid UDP length")
		}
		packet, err := ParseConfigDatagram(udp[8:])
		if err != nil {
			return nil, err
		}
		out.Protocol = "asus_cfg_udp"
		out.SourceIP, out.DestinationIP = net.IP(data[12:16]).String(), net.IP(data[16:20]).String()
		out.MessageType, out.PayloadLength = packet.Type, len(packet.Payload)
		return out, nil
	}
	return nil, nil
}
