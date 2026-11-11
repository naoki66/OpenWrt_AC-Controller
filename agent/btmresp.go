package agent

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// hostapd BTM response listener.
//
// Everything the guard knows about a suggestion used to be inferred: "the
// client is still on the same association after the window, so it declined".
// hostapd already sees the real answer — the station's WNM BSS Transition
// Management Response — and prints it on its control interface. ATTACH turns
// the per-BSS socket into an event stream; the lines we care about carry the
// station, the status code and the target BSSID.
//
// This is strictly an upgrade path: if the socket is missing, the build does
// not emit the event, or the agent runs on a non-Linux host, nothing breaks and
// the old inference keeps working. That is why a failed connect is a Debug log
// and a 30 s retry instead of an error.
const (
	defaultHostapdControlDir = "/var/run/hostapd"
	btmRespRetry             = 30 * time.Second
)

// btmListener follows the control socket of every hostapd BSS.
type btmListener struct {
	// Dir is the directory holding hostapd's per-BSS sockets. Empty means the
	// standard /var/run/hostapd; tests inject a temporary directory.
	Dir string
	// Runner enumerates BSSes through `ubus list`. Nil disables the listener.
	Runner Runner
	Log    *slog.Logger
	// OnResponse receives one parsed response per BTM the client answered.
	OnResponse func(mac string, status int, target string)
	// Retry overrides the reconnect backoff; zero means btmRespRetry.
	Retry time.Duration

	mu      sync.Mutex
	started map[string]context.CancelFunc
	conns   map[string]net.Conn
	cancel  context.CancelFunc
	closed  bool
	wg      sync.WaitGroup
}

// Start enumerates hostapd BSSes and follows their control sockets until ctx is
// cancelled or Close is called. It is safe to call once; repeated calls do
// nothing.
func (l *btmListener) Start(ctx context.Context) {
	if l == nil || l.Runner == nil {
		return
	}
	l.mu.Lock()
	if l.started != nil || l.closed {
		l.mu.Unlock()
		return
	}
	l.started = map[string]context.CancelFunc{}
	l.conns = map[string]net.Conn{}
	ctx, cancel := context.WithCancel(ctx)
	l.cancel = cancel
	l.wg.Add(1)
	l.mu.Unlock()
	go func() {
		defer l.wg.Done()
		l.supervise(ctx)
	}()
}

// Close stops every follower. It is idempotent and safe before Start.
func (l *btmListener) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	cancel := l.cancel
	conns := make([]net.Conn, 0, len(l.conns))
	for _, c := range l.conns {
		conns = append(conns, c)
	}
	l.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	l.wg.Wait()
}

// supervise re-enumerates hostapd so a BSS that appears after the agent starts
// (a radio finishing ACS, a reload) is followed too.
func (l *btmListener) supervise(ctx context.Context) {
	for {
		if names, ok := l.enumerate(ctx); ok {
			live := make(map[string]bool, len(names))
			for _, ifname := range names {
				live[ifname] = true
				l.followBSS(ctx, ifname)
			}
			l.mu.Lock()
			for ifname, stop := range l.started {
				if !live[ifname] {
					stop()
					delete(l.started, ifname)
				}
			}
			l.mu.Unlock()
		}
		if !l.sleep(ctx) {
			return
		}
	}
}

func (l *btmListener) enumerate(ctx context.Context) ([]string, bool) {
	out, err := l.Runner.Run(ctx, "ubus", "list")
	if err != nil {
		l.Log.Debug("btm listener: ubus list unavailable", "err", err)
		return nil, false
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		ifname, ok := strings.CutPrefix(strings.TrimSpace(line), "hostapd.")
		if ok && ifname != "" {
			names = append(names, ifname)
		}
	}
	return names, true
}

// followBSS starts one reconnect loop per interface.
func (l *btmListener) followBSS(ctx context.Context, ifname string) {
	l.mu.Lock()
	if l.closed || l.started[ifname] != nil {
		l.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	l.started[ifname] = cancel
	l.wg.Add(1)
	l.mu.Unlock()
	go func() {
		defer l.wg.Done()
		l.follow(ctx, ifname)
	}()
}

func (l *btmListener) follow(ctx context.Context, ifname string) {
	localDir, err := os.MkdirTemp("", "ac-btm-")
	if err != nil {
		l.Log.Warn("btm listener: create local socket directory failed", "err", err)
		return
	}
	defer os.Remove(localDir)
	local := filepath.Join(localDir, "ctrl")
	defer os.Remove(local)
	remote := filepath.Join(l.dir(), ifname)
	for ctx.Err() == nil {
		conn, err := net.DialUnix("unixgram",
			&net.UnixAddr{Name: local, Net: "unixgram"},
			&net.UnixAddr{Name: remote, Net: "unixgram"})
		if err != nil {
			_ = os.Remove(local)
			l.Log.Debug("btm listener: connect failed", "ifname", ifname, "path", remote, "err", err)
			if !l.sleep(ctx) {
				return
			}
			continue
		}

		l.mu.Lock()
		// Close may have run between Dial and this lock; in that case the
		// connection is not in its snapshot and would keep the reader (and
		// therefore Close's Wait) alive forever.
		if l.closed {
			l.mu.Unlock()
			_ = conn.Close()
			return
		}
		l.conns[ifname] = conn
		l.mu.Unlock()

		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		if err := l.read(conn); err != nil && ctx.Err() == nil {
			l.Log.Debug("btm listener: control socket ended", "ifname", ifname, "err", err)
		}
		stop()

		_ = conn.Close()
		_ = os.Remove(local)
		l.mu.Lock()
		if l.conns[ifname] == conn {
			delete(l.conns, ifname)
		}
		l.mu.Unlock()

		if !l.sleep(ctx) {
			return
		}
	}
}

// read verifies the ATTACH reply and handles one event per datagram.
func (l *btmListener) read(conn *net.UnixConn) error {
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	if _, err := conn.Write([]byte("ATTACH")); err != nil {
		return err
	}
	buf := make([]byte, 8193)
	n, err := conn.Read(buf)
	if err != nil {
		return err
	}
	if string(buf[:n]) != "OK\n" {
		return errors.New("hostapd rejected ATTACH")
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		return err
	}
	// An idle Unix datagram socket does not signal hostapd exit. Periodically
	// ping it to detect a replaced socket even with no station activity.
	waitingPong := false
	for {
		wait := 2 * time.Minute
		if waitingPong {
			wait = 2 * time.Second
		}
		if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
			return err
		}
		n, err := conn.Read(buf)
		if err != nil {
			if ne, ok := err.(net.Error); !ok || !ne.Timeout() || waitingPong {
				return err
			}
			if err := conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
				return err
			}
			if _, err := conn.Write([]byte("PING")); err != nil {
				return err
			}
			waitingPong = true
			continue
		}
		if n == len(buf) {
			continue
		}
		if string(buf[:n]) == "PONG\n" {
			waitingPong = false
			continue
		}
		mac, status, target, ok := parseBTMResponse(string(buf[:n]))
		if !ok {
			continue
		}
		if l.OnResponse != nil {
			l.OnResponse(mac, status, target)
		}
	}
}

func (l *btmListener) dir() string {
	if l.Dir != "" {
		return l.Dir
	}
	return defaultHostapdControlDir
}

func (l *btmListener) sleep(ctx context.Context) bool {
	d := l.Retry
	if d <= 0 {
		d = btmRespRetry
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// parseBTMResponse extracts the station, the status code and the target BSSID
// from one hostapd event line.
//
// Three spellings exist across hostapd versions and log prefixes:
// BSS_TM_RESP, WNM_BSS_TM_RESP and WNM-BSS-TM-RESP. They all carry the same
// key=value tail, so the parser matches the family and reads fields instead of
// relying on column positions. A line without status_code is a different event
// that happens to mention the name, and is ignored.
func parseBTMResponse(line string) (mac string, status int, target string, ok bool) {
	line = strings.TrimSpace(line)
	// hostapd prefixes events with a priority marker ("<3>"); a command reply
	// uses ">". Strip either before looking at the event name.
	for strings.HasPrefix(line, "<") || strings.HasPrefix(line, ">") {
		if line[0] == '>' {
			line = strings.TrimSpace(line[1:])
			continue
		}
		i := strings.IndexByte(line, '>')
		if i < 0 {
			return "", 0, "", false
		}
		line = strings.TrimSpace(line[i+1:])
	}

	if !strings.Contains(line, "BSS_TM_RESP") && !strings.Contains(line, "BSS-TM-RESP") {
		return "", 0, "", false
	}

	var haveStatus bool
	for _, field := range strings.Fields(line) {
		switch {
		case strings.HasPrefix(field, "status_code="):
			n, err := strconv.Atoi(strings.TrimPrefix(field, "status_code="))
			if err != nil {
				return "", 0, "", false
			}
			status = n
			haveStatus = true
		case strings.HasPrefix(field, "bssid="):
			target = normalizeBTMAddr(strings.TrimPrefix(field, "bssid="))
		case strings.HasPrefix(field, "target_bssid="):
			target = normalizeBTMAddr(strings.TrimPrefix(field, "target_bssid="))
		default:
			if mac == "" && !strings.Contains(field, "=") && looksLikeMAC(field) {
				mac = strings.ToLower(field)
			}
		}
	}
	if mac == "" || !haveStatus {
		return "", 0, "", false
	}
	return mac, status, target, true
}

func looksLikeMAC(s string) bool {
	if len(s) != 17 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if i%3 == 2 {
			if s[i] != ':' {
				return false
			}
			continue
		}
		if !isHexDigit(s[i]) {
			return false
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// normalizeBTMAddr lower-cases an address and drops the all-zero placeholder a
// declined response carries. A zero target is not a BSS worth backing off from.
func normalizeBTMAddr(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "00:00:00:00:00:00" {
		return ""
	}
	return s
}
