//go:build linux

package agent

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

// ubusd socket transport (the decoder is in ubus_blob.go so it can be tested
// everywhere). Linux-only because it is a unix domain socket; the agent only
// runs on OpenWrt but the package still has to build on the developer machine.

const (
	ubusSocketPath = "/var/run/ubus/ubus.sock"
	// ubusCallTimeout bounds one lookup+invoke pair. The daemon answers in
	// microseconds; a stall means something is wrong and the caller should fall
	// back rather than hold the heartbeat.
	ubusCallTimeout = 2 * time.Second
)

// ubusConn is a connection to ubusd. Not safe for concurrent use; Ubus serialises.
type ubusConn struct {
	conn net.Conn
	seq  uint16
}

func newUbusTransport() ubusTransport { return &ubusConn{} }

func (c *ubusConn) Close() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

// ensure dials on first use. Connection setup is one connect plus the daemon's
// hello; every later call reuses it, which is the whole point of not spawning a
// process per call.
func (c *ubusConn) ensure(ctx context.Context) error {
	if c.conn != nil {
		return nil
	}
	d := net.Dialer{Timeout: ubusCallTimeout}
	conn, err := d.DialContext(ctx, "unix", ubusSocketPath)
	if err != nil {
		return fmt.Errorf("ubus connect: %w", err)
	}
	c.conn = conn
	c.seq = 0

	c.deadline(ctx)
	typ, _, _, body, err := c.readMessage()
	if err != nil {
		c.Close()
		return fmt.Errorf("ubus hello: %w", err)
	}
	if typ != ubusMsgHello {
		c.Close()
		return fmt.Errorf("ubus hello: unexpected message type %d", typ)
	}
	_ = body // the hello carries our client id in the header; we do not need it
	return nil
}

func (c *ubusConn) deadline(ctx context.Context) {
	dl := time.Now().Add(ubusCallTimeout)
	if t, ok := ctx.Deadline(); ok && t.Before(dl) {
		dl = t
	}
	_ = c.conn.SetDeadline(dl)
}

// readMessage reads exactly one message. The framing itself lives in
// ubus_blob.go so it can be tested without a daemon.
func (c *ubusConn) readMessage() (typ uint8, seq uint16, peer uint32, body []byte, err error) {
	return readUbusMessage(c.conn)
}

func (c *ubusConn) send(typ uint8, seq uint16, peer uint32, children []byte) error {
	_, err := c.conn.Write(ubusMessage(typ, seq, peer, children))
	return err
}

func (c *ubusConn) nextSeq() uint16 {
	c.seq++
	if c.seq == 0 {
		c.seq = 1
	}
	return c.seq
}

// Lookup resolves an object path to its id. Every invoke needs one, and the
// daemon reuses the same id for the life of the object.
func (c *ubusConn) Lookup(ctx context.Context, obj string) (uint32, error) {
	if err := c.ensure(ctx); err != nil {
		return 0, err
	}
	c.deadline(ctx)
	seq := c.nextSeq()
	body := blobString(ubusAttrObjPath, obj)
	if err := c.send(ubusMsgLookup, seq, 0, body); err != nil {
		return 0, fmt.Errorf("ubus lookup send: %w", err)
	}

	var id uint32
	var found bool
	for {
		typ, mseq, _, payload, err := c.readMessage()
		if err != nil {
			return 0, fmt.Errorf("ubus lookup read: %w", err)
		}
		if mseq != seq {
			continue
		}
		switch typ {
		case ubusMsgData:
			for _, f := range ubusBodyAttrs(payload) {
				if f.id == ubusAttrObjID && !found && len(f.value()) >= 4 {
					id = binary.BigEndian.Uint32(f.value())
					found = true
				}
			}
		case ubusMsgStatus:
			code := statusFromStatusMessage(payload)
			if code != ubusStatusOK {
				return 0, statusError("lookup "+obj, code)
			}
			if !found {
				return 0, statusError("lookup "+obj, ubusStatusNotFound)
			}
			return id, nil
		}
	}
}

// Invoke calls a method with no arguments and returns the reply table.
func (c *ubusConn) Invoke(ctx context.Context, objID uint32, method string) (map[string]any, error) {
	if err := c.ensure(ctx); err != nil {
		return nil, err
	}
	c.deadline(ctx)
	seq := c.nextSeq()

	var body []byte
	body = append(body, blobU32(ubusAttrObjID, objID)...)
	body = append(body, blobString(ubusAttrMethod, method)...)
	body = append(body, blobEmpty(ubusAttrData)...)
	if err := c.send(ubusMsgInvoke, seq, objID, body); err != nil {
		return nil, fmt.Errorf("ubus invoke send: %w", err)
	}

	var result map[string]any
	for {
		typ, mseq, _, payload, err := c.readMessage()
		if err != nil {
			return nil, fmt.Errorf("ubus invoke read: %w", err)
		}
		if mseq != seq {
			continue
		}
		switch typ {
		case ubusMsgData:
			for _, f := range ubusBodyAttrs(payload) {
				if f.id != ubusAttrData {
					continue
				}
				table, err := decodeTablePayload(f.payload)
				if err != nil {
					return nil, err
				}
				// A method is allowed to reply more than once (the 11k
				// neighbour call does not, but the protocol permits it). Every
				// reply here is a complete table, so the last one wins; merging
				// would invent a half-table that neither side ever sent.
				result = table
			}
		case ubusMsgStatus:
			// Every invoke ends with a STATUS — ubusd sends one even when the
			// handler returns 0 — which is what makes this loop terminate
			// instead of waiting out the deadline.
			code := statusFromStatusMessage(payload)
			if code != ubusStatusOK {
				return nil, statusError("invoke "+method, code)
			}
			if result == nil {
				return nil, statusError("invoke "+method, ubusStatusNoData)
			}
			return result, nil
		}
	}
}
