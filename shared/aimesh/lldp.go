package aimesh

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

// 已核对的华硕 LLDP 组织扩展子类型；其编号与 802.11 厂商 IE 标签不同。
const (
	SubtypeOnboarding = 1
	SubtypeLANMAC     = 4
	SubtypeModel      = 5
	SubtypeTimestamp  = 6
	SubtypeSessionKey = 8
	SubtypeSecurity   = 13
	SubtypeGroup      = 14
)

var asusOUI = []byte{0xf8, 0x32, 0xe4}

type VendorField struct {
	Subtype uint8
	Value   []byte
}

type LLDP struct {
	ChassisID []byte // 包含 LLDP 机箱标识的子类型字节
	PortID    []byte // 包含 LLDP 端口标识的子类型字节
	TTL       uint16
	Fields    []VendorField
}

func ParseLLDP(data []byte) (LLDP, error) {
	var out LLDP
	if len(data) > 9216 {
		return out, errors.New("LLDP frame too large")
	}
	index := 0
	for len(data) >= 2 {
		header := binary.BigEndian.Uint16(data)
		kind, length := header>>9, int(header&511)
		data = data[2:]
		if length > len(data) {
			return LLDP{}, errors.New("truncated LLDP TLV")
		}
		value := data[:length]
		data = data[length:]
		if kind == 0 {
			if length != 0 || index < 3 {
				return LLDP{}, errors.New("invalid LLDP end")
			}
			return out, nil // 结束 TLV 后允许存在以太网填充。
		}
		if index < 3 && kind != uint16(index+1) {
			return LLDP{}, errors.New("invalid LLDP mandatory TLV order")
		}
		if index >= 3 && kind <= 3 {
			return LLDP{}, errors.New("duplicate LLDP mandatory TLV")
		}
		switch kind {
		case 1, 2:
			if length < 2 || length > 256 || value[0] < 1 || value[0] > 7 {
				return LLDP{}, errors.New("invalid LLDP identifier")
			}
			if kind == 1 {
				out.ChassisID = append([]byte(nil), value...)
			} else {
				out.PortID = append([]byte(nil), value...)
			}
		case 3:
			if length != 2 {
				return LLDP{}, errors.New("invalid LLDP TTL")
			}
			out.TTL = binary.BigEndian.Uint16(value)
		case 127:
			if length < 4 {
				return LLDP{}, errors.New("short organizational TLV")
			}
			if bytes.Equal(value[:3], asusOUI) {
				field := VendorField{Subtype: value[3], Value: append([]byte(nil), value[4:]...)}
				if err := validateField(field); err != nil {
					return LLDP{}, err
				}
				out.Fields = append(out.Fields, field)
			}
		}
		index++
	}
	return LLDP{}, errors.New("missing LLDP end TLV")
}

func validateField(f VendorField) error {
	switch f.Subtype {
	case SubtypeOnboarding, SubtypeTimestamp, SubtypeSecurity, 15, 16, 17:
		if len(f.Value) != 4 {
			return fmt.Errorf("LLDP subtype %d requires 4 bytes", f.Subtype)
		}
	case SubtypeLANMAC:
		if len(f.Value) != 6 {
			return errors.New("LLDP LAN MAC requires 6 bytes")
		}
	}
	return nil
}

func (l LLDP) MarshalBinary() ([]byte, error) {
	if len(l.ChassisID) < 2 || len(l.ChassisID) > 256 || len(l.PortID) < 2 || len(l.PortID) > 256 {
		return nil, errors.New("invalid LLDP identifier length")
	}
	var out []byte
	add := func(kind uint16, value []byte) {
		out = binary.BigEndian.AppendUint16(out, kind<<9|uint16(len(value)))
		out = append(out, value...)
	}
	add(1, l.ChassisID)
	add(2, l.PortID)
	add(3, binary.BigEndian.AppendUint16(nil, l.TTL))
	for _, field := range l.Fields {
		if len(field.Value) > 507 {
			return nil, errors.New("LLDP vendor value too large")
		}
		add(127, append(append(append([]byte(nil), asusOUI...), field.Subtype), field.Value...))
	}
	add(0, nil)
	if _, err := ParseLLDP(out); err != nil {
		return nil, err
	}
	return out, nil
}

// FieldSummary 仅输出诊断所需信息，不暴露未知字段原文、密钥或分组标识。
type FieldSummary struct {
	Subtype uint8   `json:"subtype"`
	Length  int     `json:"length"`
	Name    string  `json:"name,omitempty"`
	Number  *uint32 `json:"number,omitempty"`
	MAC     string  `json:"mac,omitempty"`
}

func (l LLDP) Summary() []FieldSummary {
	out := make([]FieldSummary, 0, len(l.Fields))
	for _, f := range l.Fields {
		s := FieldSummary{Subtype: f.Subtype, Length: len(f.Value)}
		switch f.Subtype {
		case 1, 6, 13, 15, 16, 17:
			s.Name = map[uint8]string{1: "onboarding", 6: "timestamp", 13: "security", 15: "reboot_time", 16: "connection_timeout", 17: "traffic_timeout"}[f.Subtype]
			if len(f.Value) == 4 {
				n := binary.BigEndian.Uint32(f.Value)
				s.Number = &n
			}
		case 4:
			s.Name = "lan_mac"
			if len(f.Value) == 6 {
				s.MAC = net.HardwareAddr(f.Value).String()
			}
		case 5:
			s.Name = "model"
		case 8:
			s.Name = "session_key_redacted"
		case 14:
			s.Name = "group_redacted"
		}
		out = append(out, s)
	}
	return out
}
