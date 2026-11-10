package aimesh

import (
	"encoding/binary"
	"errors"
	"io"
	"time"
)

// ReadPCAP 流式读取经典以太网 PCAP，并限制单条记录大小。
// PCAPNG 或 Linux cooked 抓包需要先使用抓包工具转换格式。
func ReadPCAP(reader io.Reader, visit func(time.Time, []byte) error) error {
	var header [24]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return err
	}
	var order binary.ByteOrder
	nano := false
	switch binary.BigEndian.Uint32(header[:4]) {
	case 0xa1b2c3d4:
		order = binary.BigEndian
	case 0xd4c3b2a1:
		order = binary.LittleEndian
	case 0xa1b23c4d:
		order, nano = binary.BigEndian, true
	case 0x4d3cb2a1:
		order, nano = binary.LittleEndian, true
	default:
		return errors.New("expected classic PCAP, not PCAPNG")
	}
	snaplen := order.Uint32(header[16:])
	if order.Uint16(header[4:]) != 2 || order.Uint16(header[6:]) != 4 || order.Uint32(header[20:]) != 1 || snaplen < 14 || snaplen > 262144 {
		return errors.New("unsupported PCAP header (requires Ethernet, bounded snaplen)")
	}
	var record [16]byte
	for {
		if _, err := io.ReadFull(reader, record[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		seconds, fraction := order.Uint32(record[:]), order.Uint32(record[4:])
		length, original := order.Uint32(record[8:]), order.Uint32(record[12:])
		limit := uint32(1000000)
		if nano {
			limit = 1000000000
		}
		if length > snaplen || length > original || fraction >= limit {
			return errors.New("invalid PCAP record")
		}
		data := make([]byte, int(length))
		if _, err := io.ReadFull(reader, data); err != nil {
			return err
		}
		if length != original {
			continue
		} // 截断报文无法完整校验，跳过该记录
		ns := int64(fraction)
		if !nano {
			ns *= 1000
		}
		if err := visit(time.Unix(int64(seconds), ns).UTC(), data); err != nil {
			return err
		}
	}
}
