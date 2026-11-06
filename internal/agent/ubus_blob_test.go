package agent

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// Wire-format tests.
//
// Everything here is built from the byte layout rather than from the production
// encoders, so a wrong constant shows up as a failed expectation instead of as a
// symmetric round trip that agrees with itself. The golden case in
// TestBlobmsgFieldGolden is hand-computed from blobmsg.h's
// BLOBMSG_PADDING(sizeof(struct blobmsg_hdr) + namelen + 1).

// --- encoders used to build replies ---------------------------------------

func be32(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

func be64(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}

// msgField encodes one blobmsg attribute exactly as blobmsg_add_field does: the
// extended bit, the unnamed name header, then the payload, padded to four.
// namelen is the name's length — the terminator comes from blobmsg_hdrlen's +1.
func msgField(typ int, name string, payload []byte) []byte {
	namelen := len(name)
	hdrLen := align4(2 + namelen + 1)
	total := 4 + hdrLen + len(payload)
	out := make([]byte, 4, align4(total))
	binary.BigEndian.PutUint32(out, uint32(typ)<<blobAttrIDShift|blobAttrExtendedFlag|uint32(total))
	hdr := make([]byte, hdrLen)
	binary.BigEndian.PutUint16(hdr, uint16(namelen))
	copy(hdr[2:], name)
	out = append(out, hdr...)
	out = append(out, payload...)
	for len(out) < align4(total) {
		out = append(out, 0)
	}
	return out
}

func strField(name, v string) []byte {
	return msgField(blobmsgString, name, append([]byte(v), 0))
}

func i32Field(name string, v int32) []byte {
	return msgField(blobmsgInt32, name, be32(uint32(v)))
}

func i8Field(name string, v uint8) []byte {
	return msgField(blobmsgInt8, name, []byte{v})
}

func i64Field(name string, v int64) []byte {
	return msgField(blobmsgInt64, name, be64(uint64(v)))
}

func dblField(name string, v float64) []byte {
	return msgField(blobmsgDouble, name, be64(math.Float64bits(v)))
}

func tableField(name string, children []byte) []byte {
	return msgField(blobmsgTable, name, children)
}

func arrayField(name string, elems []byte) []byte {
	return msgField(blobmsgArray, name, elems)
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// --- decoding -------------------------------------------------------------

// TestBlobmsgFieldGolden pins the name header length against a blob computed by
// hand: INT32 "freq" = 2427.
//
//	85 00 00 10   id 5 (INT32), extended, total 16
//	00 04         namelen 4 ("freq", no terminator counted)
//	66 72 65 71   "freq"
//	00 00         the terminator blobmsg_hdrlen reserves, plus the alignment byte
//	00 00 09 7B   2427
func TestBlobmsgFieldGolden(t *testing.T) {
	golden := []byte{
		0x85, 0x00, 0x00, 0x10,
		0x00, 0x04,
		'f', 'r', 'e', 'q', 0x00, 0x00,
		0x00, 0x00, 0x09, 0x7b,
	}
	if got := i32Field("freq", 2427); !bytes.Equal(got, golden) {
		t.Fatalf("encoder disagrees with the hand-computed blob:\n got % x\nwant % x", got, golden)
	}

	table, err := decodeTablePayload(golden)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := table["freq"]; got != int64(2427) {
		t.Fatalf("freq = %#v, want 2427", got)
	}
}

// TestDecodePreservesFullFieldNames is the regression test for a truncation that
// only a live board showed: reading namelen-1 bytes drops the last character of
// every field, so radio0 arrives as radio and reads like a missing netifd field.
// The names here cover every alignment class, because the header rounding and the
// name length interact.
func TestDecodePreservesFullFieldNames(t *testing.T) {
	names := []string{"up", "band", "radio", "config", "interfaces", "bss_transition"}
	var body []byte
	for i, name := range names {
		body = append(body, i32Field(name, int32(i))...)
	}
	table, err := decodeTablePayload(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(table) != len(names) {
		t.Fatalf("decoded %d fields, want %d: %v", len(table), len(names), table)
	}
	for i, name := range names {
		got, ok := table[name]
		if !ok {
			t.Fatalf("field %q missing; decoded keys: %v", name, keysOf(table))
		}
		if got != int64(i) {
			t.Fatalf("%q = %#v, want %d", name, got, i)
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestDecodeReplyTable(t *testing.T) {
	// Shaped like `ubus call hostapd.<if> get_status`, which is what the radio
	// sample parses: strings, ints, a nested table and an array.
	body := cat(
		strField("status", "ENABLED"),
		i32Field("freq", 2427),
		i32Field("channel", 4),
		i32Field("op_class", 81),
		strField("phy", "phy0.0"),
		tableField("airtime", cat(
			i32Field("utilization", 45),
			i32Field("time", 1234),
		)),
		arrayField("ifnames", cat(
			msgField(blobmsgString, "", append([]byte("phy0.0-ap0"), 0)),
			msgField(blobmsgString, "", append([]byte("phy0.0-ap1"), 0)),
		)),
		i8Field("cac_active", 0),
		i64Field("connected_time", 3600),
		dblField("noise", -92.5),
	)

	table, err := decodeTablePayload(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]any{
		"status":         "ENABLED",
		"freq":           int64(2427),
		"channel":        int64(4),
		"op_class":       int64(81),
		"phy":            "phy0.0",
		"cac_active":     false,
		"connected_time": int64(3600),
		"noise":          -92.5,
	}
	for k, v := range want {
		if got := table[k]; got != v {
			t.Errorf("%s = %#v (%T), want %#v", k, got, got, v)
		}
	}

	airtime, ok := table["airtime"].(map[string]any)
	if !ok {
		t.Fatalf("airtime = %#v, want a table", table["airtime"])
	}
	if airtime["utilization"] != int64(45) {
		t.Errorf("airtime.utilization = %#v, want 45", airtime["utilization"])
	}

	ifnames, ok := table["ifnames"].([]any)
	if !ok {
		t.Fatalf("ifnames = %#v, want an array", table["ifnames"])
	}
	if len(ifnames) != 2 || ifnames[0] != "phy0.0-ap0" || ifnames[1] != "phy0.0-ap1" {
		t.Errorf("ifnames = %#v", ifnames)
	}
}

// TestDecodeInt8BecomesBool guards the type convention the on-device comparison
// against `ubus call` depends on: blobmsg_json prints INT8 as true/false, so a
// decoder that returned numbers would differ from the CLI on every bool field.
func TestDecodeInt8BecomesBool(t *testing.T) {
	tests := []struct {
		name string
		raw  uint8
		want any
	}{
		{"zero is false", 0, false},
		{"one is true", 1, true},
		{"anything else is still true", 2, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			table, err := decodeTablePayload(i8Field("flag", tc.raw))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			got, ok := table["flag"].(bool)
			if !ok {
				t.Fatalf("flag = %#v (%T), want a bool", table["flag"], table["flag"])
			}
			if got != tc.want {
				t.Fatalf("flag = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDecodeNegativeIntegers covers the signed casts: a station's signal is
// reported as a negative number, and reading it unsigned would turn -90 into
// four billion.
func TestDecodeNegativeIntegers(t *testing.T) {
	body := cat(i32Field("signal", -90), i64Field("rx_bytes", -1))
	table, err := decodeTablePayload(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if table["signal"] != int64(-90) {
		t.Errorf("signal = %#v, want -90", table["signal"])
	}
	if table["rx_bytes"] != int64(-1) {
		t.Errorf("rx_bytes = %#v, want -1", table["rx_bytes"])
	}
}

func TestDecodeToleratesTruncatedTail(t *testing.T) {
	body := cat(strField("status", "ENABLED"), i32Field("freq", 2427))
	// Chop the last field in half the way a short read would.
	truncated := body[:len(body)-3]
	table, err := decodeTablePayload(truncated)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if table["status"] != "ENABLED" {
		t.Errorf("status = %#v, want ENABLED", table["status"])
	}
	if _, ok := table["freq"]; ok {
		t.Errorf("freq survived truncation: %#v", table["freq"])
	}
}

// --- framing --------------------------------------------------------------

// wireMessage builds a message the way ubus_send_msg writes it: the header,
// then blob_raw_len bytes of body — the length the attribute declares, not the
// padded size.
func wireMessage(typ uint8, seq uint16, peer uint32, attrs []byte) []byte {
	msg := ubusMessage(typ, seq, peer, attrs)
	if want := ubusHeaderLen + 4 + len(attrs); want < len(msg) {
		msg = msg[:want]
	}
	return msg
}

// dataMessage builds a reply the way ubus_send_reply does: the table is wrapped
// in UBUS_ATTR_DATA, and that single attribute is what the message body
// contains (ubusMessage then wraps it in the id-0 container).
func dataMessage(seq uint16, peer uint32, children []byte) []byte {
	return wireMessage(ubusMsgData, seq, peer, blobAttr(ubusAttrData, children))
}

// statusMessage builds a STATUS reply: one UBUS_ATTR_STATUS attribute.
func statusMessage(seq uint16, peer uint32, code int) []byte {
	return wireMessage(ubusMsgStatus, seq, peer, blobU32(ubusAttrStatus, uint32(code)))
}

func TestReadUbusMessageFramesTwoMessages(t *testing.T) {
	first := cat(strField("status", "ENABLED"), i32Field("freq", 2427))

	stream := bytes.NewReader(cat(
		dataMessage(7, 42, first),
		statusMessage(8, 42, 0),
	))

	typ, seq, peer, body, err := readUbusMessage(stream)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if typ != ubusMsgData || seq != 7 || peer != 42 {
		t.Fatalf("first message = type %d seq %d peer %d", typ, seq, peer)
	}
	table, err := decodeTablePayload(bodyFieldData(t, body))
	if err != nil {
		t.Fatalf("first decode: %v", err)
	}
	if table["status"] != "ENABLED" {
		t.Fatalf("first status = %#v", table["status"])
	}

	typ, seq, _, body, err = readUbusMessage(stream)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if typ != ubusMsgStatus || seq != 8 {
		t.Fatalf("second message = type %d seq %d", typ, seq)
	}
	if code := statusFromStatusMessage(body); code != 0 {
		t.Fatalf("second status = %d, want 0", code)
	}
}

// bodyFieldData digs out UBUS_ATTR_DATA from a decoded body. The message body is
// one container attribute, so this is what ubusBodyAttrs exists for.
func bodyFieldData(t *testing.T, body []byte) []byte {
	t.Helper()
	for _, f := range ubusBodyAttrs(body) {
		if f.id == ubusAttrData {
			return f.payload
		}
	}
	t.Fatalf("no UBUS_ATTR_DATA in body % x", body)
	return nil
}

// TestReadUbusMessageUsesUnpaddedLength pins the framing rule that keeps the
// stream in sync: the length to read is id_len & LEN_MASK, which is not rounded
// up. Rounding it up would swallow the first bytes of the next message and every
// reply after it would decode as garbage.
func TestReadUbusMessageUsesUnpaddedLength(t *testing.T) {
	// A body of one string attribute whose payload is 7 bytes, so the declared
	// length (11) is not a multiple of four.
	odd := msgField(blobmsgString, "x", append([]byte("abc123"), 0))
	if raw := int(binary.BigEndian.Uint32(odd) & blobAttrLenMask); raw%4 == 0 {
		t.Fatalf("test needs an unaligned attribute, got length %d", raw)
	}

	stream := bytes.NewReader(cat(
		wireMessage(ubusMsgData, 1, 0, odd),
		statusMessage(2, 0, 4),
	))

	if _, seq, _, _, err := readUbusMessage(stream); err != nil {
		t.Fatalf("first read: %v", err)
	} else if seq != 1 {
		t.Fatalf("first seq = %d", seq)
	}
	_, seq, _, body, err := readUbusMessage(stream)
	if err != nil {
		t.Fatalf("second read desynchronised: %v", err)
	}
	if seq != 2 {
		t.Fatalf("second seq = %d, want 2", seq)
	}
	if code := statusFromStatusMessage(body); code != 4 {
		t.Fatalf("second status = %d, want 4", code)
	}
}

func TestReadUbusMessageRejectsBadFrames(t *testing.T) {
	tests := []struct {
		name  string
		frame []byte
		want  string
	}{
		{
			name:  "wrong version",
			frame: []byte{1, ubusMsgData, 0, 0, 0, 0, 0, 0, 0, 0, 0, 4},
			want:  "version",
		},
		{
			name:  "length below the header",
			frame: []byte{0, ubusMsgData, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2},
			want:  "body length",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, err := readUbusMessage(bytes.NewReader(tc.frame))
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestUbusBodyAttrsDescendsIntoContainer is the one-level descent that the whole
// client depends on: ubus_msg_new wraps the attributes in an id-0 container, and
// skipping the descent looks like a perfectly valid empty reply.
func TestUbusBodyAttrsDescendsIntoContainer(t *testing.T) {
	inner := cat(blobU32(ubusAttrObjID, 77), blobString(ubusAttrMethod, "get_status"))
	body := blobAttr(ubusAttrUnspec, inner)

	fields := ubusBodyAttrs(body)
	if len(fields) != 2 {
		t.Fatalf("got %d fields, want the two inside the container", len(fields))
	}
	if fields[0].id != ubusAttrObjID || len(fields[0].value()) != 4 {
		t.Fatalf("first field = id %d len %d", fields[0].id, len(fields[0].value()))
	}
	if got := binary.BigEndian.Uint32(fields[0].value()); got != 77 {
		t.Fatalf("objid = %d, want 77", got)
	}
	if fields[1].id != ubusAttrMethod || blobStringValue(fields[1].value()) != "get_status" {
		t.Fatalf("second field = id %d %q", fields[1].id, blobStringValue(fields[1].value()))
	}
}

// TestUbusMessageLayout pins the request side: the body is one container whose
// length covers every attribute inside it, which is what get_next_msg measures.
func TestUbusMessageLayout(t *testing.T) {
	attrs := cat(blobU32(ubusAttrObjID, 9), blobString(ubusAttrMethod, "status"))
	msg := ubusMessage(ubusMsgInvoke, 3, 9, attrs)

	if len(msg) != ubusHeaderLen+4+len(attrs) {
		t.Fatalf("message length = %d, want %d", len(msg), ubusHeaderLen+4+len(attrs))
	}
	if msg[0] != ubusMsgVersion || msg[1] != ubusMsgInvoke {
		t.Fatalf("header = version %d type %d", msg[0], msg[1])
	}
	if got := binary.BigEndian.Uint16(msg[2:]); got != 3 {
		t.Fatalf("seq = %d", got)
	}
	if got := binary.BigEndian.Uint32(msg[4:]); got != 9 {
		t.Fatalf("peer = %d", got)
	}
	word := binary.BigEndian.Uint32(msg[ubusHeaderLen:])
	if id := int((word & blobAttrIDMask) >> blobAttrIDShift); id != ubusAttrUnspec {
		t.Fatalf("container id = %d, want 0", id)
	}
	if raw := int(word & blobAttrLenMask); raw != 4+len(attrs) {
		t.Fatalf("container length = %d, want %d", raw, 4+len(attrs))
	}
}

// TestDecodeEmptyArrayIsNotAnEmptyTable is the difference only a live board
// showed: netifd reports `stations: []`, and a decoder that decides array
// versus table by looking at the children cannot tell that apart from an empty
// table, because both have none. Only the type knows, and the controller stores
// the shape it is handed all the way to the UI.
func TestDecodeEmptyArrayIsNotAnEmptyTable(t *testing.T) {
	body := cat(
		arrayField("stations", nil),
		tableField("vlans", nil),
	)
	table, err := decodeTablePayload(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := table["stations"].([]any); !ok {
		t.Fatalf("stations = %#v (%T), want an empty slice", table["stations"], table["stations"])
	}
	if _, ok := table["vlans"].(map[string]any); !ok {
		t.Fatalf("vlans = %#v (%T), want an empty map", table["vlans"], table["vlans"])
	}

	for field, want := range map[string]string{"stations": "[]", "vlans": "{}"} {
		b, err := json.Marshal(table[field])
		if err != nil {
			t.Fatalf("marshal %s: %v", field, err)
		}
		if string(b) != want {
			t.Errorf("%s renders as %s, want %s", field, b, want)
		}
	}
}
