package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ubus call client.
//
// The wire format lives in ubus_blob.go and the socket in
// ubus_client_linux.go; this file is the part both builds share: the transport
// interface, the object-id cache, and the circuit breaker.
//
// Why the extra layer: the agent makes four ubus reads per heartbeat that a
// fork+exec of the `ubus` binary costs 3.5-4 ms each on, and the 11k self-report
// pass makes twelve more. Every one of them is a read-only method with no
// arguments. Talking to ubusd directly removes the process; the cache and the
// breaker make sure the removal cannot make things worse on a board where the
// socket is not where it is expected.

// ErrUbusUnavailable reports that the ubus transport cannot be used on this
// platform or in this build. Callers fall back to the CLI on it, exactly as they
// do on any other ubus failure.
var ErrUbusUnavailable = errors.New("ubus: transport unavailable")

// ubusTransport is the wire half: resolve a path to an object id, then invoke a
// method without arguments. An interface because the client must be testable
// without a daemon, and because the non-Linux build supplies a stub rather than
// failing to compile.
type ubusTransport interface {
	Lookup(ctx context.Context, obj string) (uint32, error)
	Invoke(ctx context.Context, objID uint32, method string) (map[string]any, error)
	Close() error
}

// ubusStatusError is a UBUS_STATUS_* reply: the daemon answered, and the answer
// was "no". It is distinct from a broken socket because the two need opposite
// responses — a stale object id is worth one retry, a dead socket is not.
type ubusStatusError struct {
	Op   string
	Code int
}

func (e *ubusStatusError) Error() string {
	return fmt.Sprintf("ubus %s: %s", e.Op, statusText(e.Code))
}

func statusError(op string, code int) error {
	if code == ubusStatusOK {
		return nil
	}
	return &ubusStatusError{Op: op, Code: code}
}

// isStaleObjectError reports whether a cached object id has become invalid.
//
// ubusd allocates ids monotonically and never reuses them, so an id that no
// longer resolves means the object was destroyed and re-created. That happens on
// every `wifi reload` — which this agent issues itself — so treating it as a
// hard failure would disable the fast path for a minute after every apply.
func isStaleObjectError(err error) bool {
	var se *ubusStatusError
	if !errors.As(err, &se) {
		return false
	}
	return se.Code == ubusStatusNotFound || se.Code == ubusStatusMethodNotFound
}

const (
	// ubusFailureLimit is how many consecutive transport failures it takes to
	// conclude the socket is not going to answer. One failure is always
	// retried: a dropped connection just needs a redial.
	ubusFailureLimit = 3
	// ubusBackoff is how long the client stops trying after that. Every caller
	// falls back to the CLI when a call fails, so backing off restores the
	// previous behaviour instead of regressing it — the cost is the old cost.
	ubusBackoff = 60 * time.Second
)

// ubusClient serialises calls over one connection and remembers object ids.
//
// Serialisation is deliberate: ubusd multiplexes by sequence number on one
// socket, so concurrent callers would need the reply demultiplexing that
// libubus gets from its event loop. The heartbeat is single-threaded and the
// calls take microseconds, so a mutex is the honest answer.
type ubusClient struct {
	mu       sync.Mutex
	tr       ubusTransport
	ids      map[string]uint32
	failures int
	skipTill time.Time
	logged   bool

	// Now is injectable so the breaker can be tested without sleeping.
	Now func() time.Time
	Log *slog.Logger
}

func newUbusClient(log *slog.Logger) *ubusClient {
	return &ubusClient{
		tr:  newUbusTransport(),
		ids: make(map[string]uint32, 16),
		Now: time.Now,
		Log: log,
	}
}

func (c *ubusClient) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Close drops the connection. The next call redials.
func (c *ubusClient) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tr == nil {
		return nil
	}
	return c.tr.Close()
}

// Call invokes one method and returns the reply table.
//
// A nil client is not a programming error: AC_UBUS=off leaves it nil on purpose,
// and every caller is expected to fall back to the CLI.
func (c *ubusClient) Call(ctx context.Context, obj, method string) (map[string]any, error) {
	if c == nil {
		return nil, ErrUbusUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tr == nil {
		return nil, ErrUbusUnavailable
	}
	if now := c.now(); now.Before(c.skipTill) {
		return nil, fmt.Errorf("ubus %s.%s: %w", obj, method, ErrUbusUnavailable)
	}

	id, cached := c.ids[obj]
	if !cached {
		var err error
		id, err = c.tr.Lookup(ctx, obj)
		if err != nil {
			c.failed(obj, err)
			return nil, err
		}
		c.ids[obj] = id
	}

	res, err := c.tr.Invoke(ctx, id, method)
	if err == nil {
		c.succeeded(obj, method)
		return res, nil
	}
	if !cached || !isStaleObjectError(err) {
		c.failed(obj, err)
		return nil, err
	}

	// The id we cached points at a corpse. Resolve once more and retry: this is
	// the `wifi reload` case, not the "object is genuinely gone" case, and one
	// extra lookup costs less than a heartbeat spent on the CLI path.
	delete(c.ids, obj)
	id, lerr := c.tr.Lookup(ctx, obj)
	if lerr != nil {
		c.failed(obj, lerr)
		return nil, lerr
	}
	c.ids[obj] = id
	res, err = c.tr.Invoke(ctx, id, method)
	if err != nil {
		c.failed(obj, err)
		return nil, err
	}
	c.succeeded(obj, method)
	return res, nil
}

// CallJSON invokes one method and returns its reply as JSON text.
//
// The collectors were written against the CLI's JSON output, and keeping that
// shape is the whole basis of swapping the transport safely: the same bytes go
// into the same json.Unmarshal, so every value is bit-identical by construction
// rather than by a field-by-field argument. Marshal of a 2 KB reply costs tens
// of microseconds against the 3.5 ms of fork+exec it replaces.
func (c *ubusClient) CallJSON(ctx context.Context, obj, method string) ([]byte, error) {
	table, err := c.Call(ctx, obj, method)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(table)
	if err != nil {
		return nil, fmt.Errorf("ubus %s.%s: encode reply: %w", obj, method, err)
	}
	return b, nil
}

// ubusReply returns one ubus method's reply as JSON, using the socket when it is
// available and the CLI otherwise.
//
// Both collectors parse this JSON with the code they always used, which is what
// makes the swap safe: the bytes arrive in the CLI's shape by construction, so
// "the numbers did not change" does not depend on anyone re-deriving netifd's and
// hostapd's field types — including the one field whose window is undocumented
// (hostapd's airtime.utilization).
func ubusReply(ctx context.Context, ubus *ubusClient, runner Runner, log *slog.Logger, obj, method string) ([]byte, error) {
	if ubus != nil {
		out, err := ubus.CallJSON(ctx, obj, method)
		if err == nil {
			return out, nil
		}
		// Degrade to the old cost, not to no data: a board without the socket,
		// or one whose ubusd is wedged, must keep reporting.
		if log != nil {
			log.Debug("ubus socket call failed, falling back to the CLI",
				"obj", obj, "method", method, "err", err)
		}
	}
	out, err := runner.Run(ctx, "ubus", "call", obj, method)
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// DebugUbusCall runs one ubus call through the agent's own client and returns
// the reply as JSON, so `ac-agent ubus-dump <obj> <method>` measures the path the
// heartbeat uses — the id cache, the breaker, the decoders — and its output can
// be diffed against `ubus call <obj> <method>` on the same board.
func (a *Agent) DebugUbusCall(ctx context.Context, obj, method string) ([]byte, error) {
	c := a.ubus
	if c == nil {
		// Still works with AC_UBUS=off: the kill switch is for the heartbeat,
		// not for a human asking a question.
		c = newUbusClient(a.Log)
		defer c.Close()
	}
	return c.CallJSON(ctx, obj, method)
}

// succeeded resets the breaker and announces the fast path once per process, so
// a log from a live AP answers "is this build actually using the socket?".
func (c *ubusClient) succeeded(obj, method string) {
	c.failures = 0
	c.skipTill = time.Time{}
	if c.logged {
		return
	}
	c.logged = true
	if c.Log != nil {
		c.Log.Info("ubus transport active", "obj", obj, "method", method)
	}
}

// failed counts transport failures and trips the breaker after enough of them.
//
// A status reply is not counted: the daemon answered, so the socket works and
// the object is simply absent — a board with no hostapd objects would otherwise
// trip the breaker on every cycle and log about it forever.
func (c *ubusClient) failed(obj string, err error) {
	var se *ubusStatusError
	if errors.As(err, &se) {
		if c.Log != nil {
			c.Log.Debug("ubus call refused", "obj", obj, "err", err)
		}
		return
	}
	c.failures++
	// Drop the socket so the next attempt starts from a fresh connect rather
	// than reading the tail of a half-dead stream.
	if c.tr != nil {
		_ = c.tr.Close()
	}
	if c.failures < ubusFailureLimit {
		return
	}
	c.skipTill = c.now().Add(ubusBackoff)
	if c.Log != nil {
		c.Log.Warn("ubus transport backing off", "err", err, "failures", c.failures, "retry_in", ubusBackoff)
	}
}
