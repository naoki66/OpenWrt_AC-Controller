package agent

import (
	"context"
	"net"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

type btmListRunner struct{ list string }

func (r btmListRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	return r.list, nil
}

// A full round trip through a fake hostapd control socket: the listener must
// attach, parse one response and hand the station, status and target to the
// callback.
func TestBTMListenerParsesHostapdResponse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hostapd uses Linux Unix datagram sockets")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "phy0-ap0")
	ln, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer ln.Close()

	got := make(chan [3]string, 1)
	l := &btmListener{
		Dir:    dir,
		Runner: btmListRunner{list: "hostapd.phy0-ap0\n"},
		Log:    testLog(),
		OnResponse: func(mac string, status int, target string) {
			got <- [3]string{mac, itoa(status), target}
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.Start(ctx)
	defer l.Close()

	if err := ln.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	var buf [4096]byte
	n, addr, err := ln.ReadFromUnix(buf[:])
	if err != nil {
		t.Fatalf("read attach: %v", err)
	}

	// The first thing on the wire must be ATTACH, otherwise hostapd keeps the
	// socket in command mode and never streams events.
	if string(buf[:n]) != "ATTACH" {
		t.Fatalf("first write = %q, want ATTACH", buf[:n])
	}

	if _, err := ln.WriteToUnix([]byte("OK\n"), addr); err != nil {
		t.Fatalf("reply to attach: %v", err)
	}
	if _, err := ln.WriteToUnix([]byte("<3>CTRL-EVENT-SCAN-RESULTS"), addr); err != nil {
		t.Fatalf("write noise: %v", err)
	}
	if _, err := ln.WriteToUnix([]byte("<3>BSS-TM-RESP 02:00:00:00:00:9a status_code=0 target_bssid=AA:BB:CC:DD:EE:FF"), addr); err != nil {
		t.Fatalf("write response: %v", err)
	}

	select {
	case r := <-got:
		if r[0] != "02:00:00:00:00:9a" {
			t.Errorf("mac = %q", r[0])
		}
		if r[1] != "0" {
			t.Errorf("status = %q, want 0", r[1])
		}
		if r[2] != "aa:bb:cc:dd:ee:ff" {
			t.Errorf("target = %q", r[2])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no response delivered to the callback")
	}
}

func TestParseBTMResponseSpellings(t *testing.T) {
	cases := []struct {
		name   string
		line   string
		mac    string
		status int
		target string
		ok     bool
	}{
		{
			name: "bss_tm_resp",
			line: "<3>BSS_TM_RESP 02:00:00:00:00:9a status_code=0 bssid=aa:bb:cc:dd:ee:ff",
			mac:  "02:00:00:00:00:9a", status: 0, target: "aa:bb:cc:dd:ee:ff", ok: true,
		},
		{
			name: "wnm_bss_tm_resp",
			line: "WNM_BSS_TM_RESP 02:00:00:00:00:9b status_code=1 bssid=aa:bb:cc:dd:ee:01",
			mac:  "02:00:00:00:00:9b", status: 1, target: "aa:bb:cc:dd:ee:01", ok: true,
		},
		{
			name: "wnm_dashed",
			line: "WNM-BSS-TM-RESP 02:00:00:00:00:9c status_code=1",
			mac:  "02:00:00:00:00:9c", status: 1, target: "", ok: true,
		},
		{
			// A rejected response commonly carries an all-zero target.
			name: "zero_target",
			line: "BSS_TM_RESP 02:00:00:00:00:9d status_code=1 bssid=00:00:00:00:00:00",
			mac:  "02:00:00:00:00:9d", status: 1, target: "", ok: true,
		},
		{name: "unrelated", line: "<3>AP-STA-CONNECTED 02:00:00:00:00:9a", ok: false},
		{name: "no_status", line: "BSS_TM_RESP 02:00:00:00:00:9a bssid=aa:bb:cc:dd:ee:ff", ok: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mac, status, target, ok := parseBTMResponse(c.line)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if !ok {
				return
			}
			if mac != c.mac || status != c.status || target != c.target {
				t.Fatalf("got mac=%q status=%d target=%q, want %q/%d/%q",
					mac, status, target, c.mac, c.status, c.target)
			}
		})
	}
}
