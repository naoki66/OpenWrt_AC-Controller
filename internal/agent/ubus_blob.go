package agent

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// ubus wire format.
//
// The agent needs four ubus reads per cycle (two for the radio sample, twelve
// for the 11k self-reports) and every one of them used to cost a fork+exec of
// the ubus binary — 3.5 to 4 ms each, ~9 ms per heartbeat that produced nothing
// the kernel could not have answered. Talking to ubusd over its own socket
// removes the process entirely.
//
// Every constant below is taken from the upstream headers, not from memory:
// ubusmsg.h (message types, attribute ids, status codes), blob.h (the attribute
// framing) and blobmsg.h (the typed values). That matters: the difference
// between UBUS_ATTR_OBJID=3 and a remembered 2 is not an error, it is an empty
// reply — the same failure mode as the wrong nl80211 attribute number, which
// cost a round of on-device debugging.
//
// Framing, from libubus-io.c's get_next_msg(): a message is an 8-byte header
// followed by exactly one container attribute whose length covers the whole
// body. All integers on the wire are big-endian, and blob attribute lengths
// include their own 4-byte header.

const (
	ubusMsgHello  = 0
	ubusMsgStatus = 1
	ubusMsgData   = 2
	ubusMsgPing   = 3
	ubusMsgLookup = 4
	ubusMsgInvoke = 5

	ubusAttrUnspec  = 0
	ubusAttrStatus  = 1
	ubusAttrObjPath = 2
	ubusAttrObjID   = 3
	ubusAttrMethod  = 4
	ubusAttrData    = 7

	ubusStatusOK             = 0
	ubusStatusMethodNotFound = 3
	ubusStatusNotFound       = 4
	ubusStatusNoData         = 5
	ubusStatusPermission     = 6
	ubusStatusTimeout        = 7

	ubusHeaderLen = 8
	ubusMaxMsgLen = 1024 * 1024
	// ubusMsgVersion is UBUS_MSG_VERSION; ubusd rejects anything else.
	ubusMsgVersion = 0

	// blobmsg value types. They share the number space with the blob attribute
	// ids above, which is why a table is "id 2" inside a blobmsg one.
	blobmsgUnspec = 0
	blobmsgArray  = 1
	blobmsgTable  = 2
	blobmsgString = 3
	blobmsgInt64  = 4
	blobmsgInt32  = 5
	blobmsgInt16  = 6
	blobmsgInt8   = 7
	blobmsgDouble = 8

	blobAttrLenMask      = 0x00ffffff
	blobAttrIDMask       = 0x7f000000
	blobAttrIDShift      = 24
	blobAttrExtendedFlag = 0x80000000
	blobAlign            = 4
)

// align4 rounds n up to the blob attribute alignment.
func align4(n int) int { return (n + blobAlign - 1) &^ (blobAlign - 1) }

// --- encoding -------------------------------------------------------------

// blobAttr encodes one unnamed attribute: a big-endian id/len word followed by
// the payload, padded to four bytes. The stored length includes the header,
// which is easy to get wrong in the other direction and produces a message the
// daemon silently drops.
func blobAttr(id int, payload []byte) []byte {
	raw := 4 + len(payload)
	out := make([]byte, 4, align4(raw))
	binary.BigEndian.PutUint32(out, uint32(id)<<blobAttrIDShift|uint32(raw))
	out = append(out, payload...)
	for len(out) < align4(raw) {
		out = append(out, 0)
	}
	return out
}

// blobU32 encodes a 32-bit value; blob_get_u32 reads it big-endian.
func blobU32(id int, v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return blobAttr(id, b[:])
}

// blobString encodes a NUL-terminated string, as blob_put_string does.
func blobString(id int, s string) []byte {
	return blobAttr(id, append([]byte(s), 0))
}

// blobEmpty encodes a zero-length attribute, which is how ubus_put_data sends a
// call with no arguments.
func blobEmpty(id int) []byte { return blobAttr(id, nil) }

// ubusMessage frames a body for the wire: the packed header, then the container
// attribute that get_next_msg reads the total length from.
func ubusMessage(typ uint8, seq uint16, peer uint32, children []byte) []byte {
	out := make([]byte, ubusHeaderLen, ubusHeaderLen+align4(4+len(children)))
	out[0] = 0 // version
	out[1] = typ
	binary.BigEndian.PutUint16(out[2:], seq)
	binary.BigEndian.PutUint32(out[4:], peer)
	return append(out, blobAttr(blobmsgUnspec, children)...)
}

// readUbusMessage reads exactly one message off r, returning its body: the
// message's single top-level attribute, header included.
//
// The header carries no length of its own, so the read is staged — 8-byte
// header, then the first attribute's 4-byte id/len word, then the rest that
// word asks for. This is get_next_msg's sequence (libubus-io.c) and it is the
// only way to frame a message on a stream socket.
//
// Two details decide whether the stream stays in sync, and both are easy to get
// backwards:
//
//   - The length to read is blob_raw_len, i.e. id_len & LEN_MASK. It is *not*
//     padded: ubus_send_msg writes blob_raw_len(msg) bytes while blob_pad_len is
//     what the in-memory buffer occupies. Rounding the read up would swallow the
//     next message's first bytes; using the unpadded length is correct.
//   - The body keeps its attribute header. libubus hands `blob_data(ub->data)`
//     to the reply callback, i.e. the *payload* of this attribute is the table,
//     so a decoder that stripped the header here would then look for
//     UBUS_ATTR_DATA among the table's own children — and find nothing, forever.
func readUbusMessage(r io.Reader) (typ uint8, seq uint16, peer uint32, body []byte, err error) {
	hdr := make([]byte, ubusHeaderLen)
	if _, err = io.ReadFull(r, hdr); err != nil {
		return 0, 0, 0, nil, err
	}
	if hdr[0] != ubusMsgVersion {
		return 0, 0, 0, nil, fmt.Errorf("unsupported ubus message version %d", hdr[0])
	}
	typ = hdr[1]
	seq = binary.BigEndian.Uint16(hdr[2:])
	peer = binary.BigEndian.Uint32(hdr[4:])

	word := make([]byte, 4)
	if _, err = io.ReadFull(r, word); err != nil {
		return 0, 0, 0, nil, err
	}
	raw := int(binary.BigEndian.Uint32(word) & blobAttrLenMask)
	if raw < 4 || raw > ubusMaxMsgLen {
		return 0, 0, 0, nil, fmt.Errorf("bad ubus body length %d", raw)
	}
	buf := make([]byte, raw)
	copy(buf, word)
	if raw > 4 {
		if _, err = io.ReadFull(r, buf[4:]); err != nil {
			return 0, 0, 0, nil, err
		}
	}
	return typ, seq, peer, buf, nil
}

// --- decoding -------------------------------------------------------------

// blobField is one decoded attribute. payload and everything derived from it
// alias the message buffer and are only valid until the next read, which is why
// decode copies values out immediately instead of handing fields to callers.
type blobField struct {
	id       int
	extended bool
	payload  []byte
}

// name returns the blobmsg name. An attribute inside a table carries one; an
// array element and a plain blob attribute do not.
//
// namelen counts the name and nothing else: the +1 inside blobmsg_hdrlen is the
// byte reserved for the terminator, so a 6-character name stores 6. Slicing
// namelen-1 bytes instead compiles, parses, and silently drops the last
// character of every field name — radio0 becomes radio, which then reads like a
// netifd field that does not exist.
func (f blobField) name() string {
	if !f.extended || len(f.payload) < 2 {
		return ""
	}
	n := int(binary.BigEndian.Uint16(f.payload[0:2]))
	if n < 1 || 2+n > len(f.payload) {
		return ""
	}
	return blobStringValue(f.payload[2 : 2+n])
}

// value strips the blobmsg name header. blobmsg_data only skips it when the
// extended flag is set, so a plain attribute's payload starts immediately.
func (f blobField) value() []byte {
	if !f.extended {
		return f.payload
	}
	hdr := blobmsgHeaderLen(int(binary.BigEndian.Uint16(f.payload[0:2])))
	if hdr > len(f.payload) {
		return nil
	}
	return f.payload[hdr:]
}

// blobmsgHeaderLen mirrors blobmsg_hdrlen: the two-byte length field, the name,
// and the one byte BLOBMSG_PADDING reserves for its terminator, rounded up to the
// four-byte alignment.
//
// The extra +1 is upstream's, not a typo here: BLOBMSG_PADDING rounds
// sizeof(struct blobmsg_hdr) + namelen + 1. namelen does *not* count the NUL
// (that reserved byte is where it lands), so a 5-character name occupies 8 bytes
// of header. Get the rounding wrong and every value after the first field shifts.
func blobmsgHeaderLen(namelen int) int { return align4(2 + namelen + 1) }

// ubusBodyAttrs returns a message body's attribute list.
//
// A ubus body is not a bare list of attributes: ubus_msg_new wraps whatever it
// is given in a single container attribute (id 0, length covering the whole
// body), so the body's only top-level attribute is that container and the real
// attributes — UBUS_ATTR_DATA, UBUS_ATTR_STATUS, UBUS_ATTR_OBJID — are its
// children. libubus reads them with blob_parse_untrusted(ub->data, ...), which
// iterates blob_data(attr), i.e. exactly one level in.
//
// Missing this level is invisible in the worst way: the container parses fine as
// a single unnamed field, so every lookup returns "object not found" and every
// call returns "no data" while the bytes on the wire are perfectly good.
func ubusBodyAttrs(body []byte) []blobField {
	fields := parseBlobFields(body)
	if len(fields) == 1 && fields[0].id == ubusAttrUnspec && !fields[0].extended {
		return parseBlobFields(fields[0].payload)
	}
	return fields
}

// parseBlobFields walks a list of sibling attributes. It stops at the first
// malformed one rather than returning an error the caller would have to ignore:
// a truncated tail means the daemon sent something this decoder does not
// understand, and the fields parsed so far are still trustworthy.
func parseBlobFields(b []byte) []blobField {
	var out []blobField
	for len(b) >= 4 {
		v := binary.BigEndian.Uint32(b[0:4])
		raw := int(v & blobAttrLenMask)
		if raw < 4 || raw > len(b) {
			return out
		}
		out = append(out, blobField{
			id:       int((v & blobAttrIDMask) >> blobAttrIDShift),
			extended: v&blobAttrExtendedFlag != 0,
			payload:  b[4:raw],
		})
		next := align4(raw)
		if next >= len(b) {
			return out
		}
		b = b[next:]
	}
	return out
}

// blobStringValue trims the NUL that blobmsg_add_string includes.
func blobStringValue(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// decodeTablePayload is the entry point for a reply's top-level table, which
// arrives as the payload of UBUS_ATTR_DATA and therefore has no type of its own.
func decodeTablePayload(payload []byte) (map[string]any, error) {
	fields := parseBlobFields(payload)
	table := make(map[string]any, len(fields))
	for _, f := range fields {
		v, err := decodeValue(f)
		if err != nil {
			return nil, err
		}
		if name := f.name(); name != "" {
			table[name] = v
		}
	}
	return table, nil
}

// decodeArrayPayload converts an array body into a slice, in wire order.
func decodeArrayPayload(payload []byte) ([]any, error) {
	fields := parseBlobFields(payload)
	out := make([]any, 0, len(fields))
	for _, f := range fields {
		v, err := decodeValue(f)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// decodeValue converts one blobmsg attribute. The signed casts matter: noise
// floors and signal levels are negative, and reading them unsigned would turn
// -90 dBm into 4 billion.
//
// Array versus table is decided by the type, never by the contents. Deciding by
// "are the children named?" cannot tell the two apart when there are no children
// at all, and netifd reports exactly that: `stations` and `vlans` are empty
// arrays, which the CLI prints as [] and a contents-based decoder turns into {}.
// The controller stores whatever shape it is handed, so that difference survives
// all the way to the UI.
func decodeValue(f blobField) (any, error) {
	v := f.value()
	switch f.id {
	case blobmsgArray:
		return decodeArrayPayload(v)
	case blobmsgTable:
		return decodeTablePayload(v)
	case blobmsgString:
		return blobStringValue(v), nil
	case blobmsgInt8:
		// blobmsg_json prints INT8 as a boolean (hostapd's "cac_active": false),
		// so the decoded form has to be a bool or the on-device comparison
		// against the ubus command fails on a type difference alone.
		return len(v) > 0 && v[0] != 0, nil
	case blobmsgInt16:
		if len(v) < 2 {
			return int64(0), nil
		}
		return int64(int16(binary.BigEndian.Uint16(v))), nil
	case blobmsgInt32:
		if len(v) < 4 {
			return int64(0), nil
		}
		return int64(int32(binary.BigEndian.Uint32(v))), nil
	case blobmsgInt64:
		if len(v) < 8 {
			return int64(0), nil
		}
		return int64(binary.BigEndian.Uint64(v)), nil
	case blobmsgDouble:
		if len(v) < 8 {
			return float64(0), nil
		}
		return math.Float64frombits(binary.BigEndian.Uint64(v)), nil
	}
	return nil, nil
}

// statusFromStatusMessage reads UBUS_ATTR_STATUS out of a STATUS message. A
// missing attribute is read as 0 rather than as an error: the message type
// already says the call ended, and ubusd writes the code explicitly whenever it
// is non-zero.
func statusFromStatusMessage(body []byte) int {
	for _, f := range ubusBodyAttrs(body) {
		if f.id == ubusAttrStatus && len(f.value()) >= 4 {
			return int(int32(binary.BigEndian.Uint32(f.value())))
		}
	}
	return ubusStatusOK
}

// statusText turns a UBUS_STATUS_* code into something a log line can carry.
func statusText(code int) string {
	switch code {
	case ubusStatusOK:
		return "ok"
	case 1:
		return "invalid command"
	case 2:
		return "invalid argument"
	case ubusStatusMethodNotFound:
		return "method not found"
	case ubusStatusNotFound:
		return "object not found"
	case ubusStatusNoData:
		return "no data"
	case ubusStatusPermission:
		return "permission denied"
	case ubusStatusTimeout:
		return "timeout"
	case 8:
		return "not supported"
	case 9:
		return "unknown error"
	case 10:
		return "connection failed"
	case 11:
		return "no memory"
	case 12:
		return "parse error"
	case 13:
		return "system error"
	}
	return fmt.Sprintf("status %d", code)
}
