// Package aimesh 实现从公开华硕固件中恢复的基础报文格式。
// 本包尚未实现 AiMesh 认证或配对入网，不能据此认定支持原厂混组。
package aimesh

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

const (
	ConfigPort       = 7788
	ConfigHeaderSize = 12
	// 样本中的 UDP 接收缓冲区为 2048 字节，包含报文头。
	MaxUDPPayload = 2048 - ConfigHeaderSize
)

// ConfigDatagram 表示配置协议的 UDP 外层，载荷可能已加密。
// 消息语义尚未独立核实，因此 Type 保留原始编号，不推测消息名称。
type ConfigDatagram struct {
	Type    uint32
	Payload []byte
}

// ConfigChecksum 使用反射 IEEE 校验表，寄存器初值为 0，结束时不取反。
// 此算法与 crc32.ChecksumIEEE 的初值和取反规则不同。
func ConfigChecksum(payload []byte) uint32 {
	var sum uint32
	for _, b := range payload {
		sum = crc32.IEEETable[byte(sum)^b] ^ (sum >> 8)
	}
	return sum
}

func ParseConfigDatagram(data []byte) (ConfigDatagram, error) {
	if len(data) < ConfigHeaderSize || len(data) > 2048 {
		return ConfigDatagram{}, errors.New("invalid cfg UDP size")
	}
	kind := binary.BigEndian.Uint32(data)
	length := binary.BigEndian.Uint32(data[4:])
	if kind < 1 || kind > 7 || length != uint32(len(data)-ConfigHeaderSize) {
		return ConfigDatagram{}, errors.New("invalid cfg UDP type or length")
	}
	payload := data[ConfigHeaderSize:]
	if ConfigChecksum(payload) != binary.BigEndian.Uint32(data[8:]) {
		return ConfigDatagram{}, errors.New("cfg UDP checksum mismatch")
	}
	return ConfigDatagram{Type: kind, Payload: append([]byte(nil), payload...)}, nil
}

func (d ConfigDatagram) MarshalBinary() ([]byte, error) {
	if d.Type < 1 || d.Type > 7 || len(d.Payload) > MaxUDPPayload {
		return nil, errors.New("invalid cfg UDP type or payload size")
	}
	data := make([]byte, ConfigHeaderSize+len(d.Payload))
	binary.BigEndian.PutUint32(data, d.Type)
	binary.BigEndian.PutUint32(data[4:], uint32(len(d.Payload)))
	binary.BigEndian.PutUint32(data[8:], ConfigChecksum(d.Payload))
	copy(data[12:], d.Payload)
	return data, nil
}
