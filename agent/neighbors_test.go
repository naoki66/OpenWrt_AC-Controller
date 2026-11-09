package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// neighborRunner impersonates hostapd's 11k ubus surface and keeps the list per
// BSS, so a test can observe whether a write actually took effect. A runner that
// only counted calls could not tell "set succeeded" from "set landed on an
// instance about to be replaced", which is the failure the retry loop exists for.
type neighborRunner struct {
	mu     sync.Mutex
	calls  []string
	absent []string              // hostapd objects that do not exist
	lists  map[string][][]string // current rrm_nr_list per interface
	own    map[string]string     // raw rrm_nr_get_own payload per interface
	drop   int                   // swallow this many rrm_nr_set calls
	wifiUp int                   // `wifi up` invocations
	hook   func()                // invoked after every recorded call
}

func (r *neighborRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	key := name + " " + strings.Join(args, " ")

	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, key)
	if r.hook != nil {
		// Lets a test make a BSS appear part-way through, the way a radio
		// finishing ACS does.
		r.hook()
	}

	if key == "ubus list" {
		present := map[string]bool{}
		for k := range r.own {
			present[k] = true
		}
		for k := range r.lists {
			present["hostapd."+k] = true
		}
		// An absent object is not in the listing, exactly as hostapd behaves
		// while a radio is still coming up.
		for _, a := range r.absent {
			delete(present, a)
		}
		objs := make([]string, 0, len(present))
		for k := range present {
			objs = append(objs, k)
		}
		sort.Strings(objs)
		return strings.Join(objs, "\n") + "\n", nil
	}

	// `ubus list hostapd.X` is the existence probe.
	if rest, ok := strings.CutPrefix(key, "ubus list "); ok {
		for _, a := range r.absent {
			if a == rest {
				return "", errors.New("not found")
			}
		}
		return rest + " @abc\n", nil
	}

	if rest, ok := strings.CutPrefix(key, "ubus call hostapd."); ok {
		parts := strings.SplitN(rest, " ", 2)
		ifname := parts[0]
		method := ""
		if len(parts) > 1 {
			method = parts[1]
		}
		switch {
		case strings.HasPrefix(method, "rrm_nr_list"):
			return marshalList(r.lists[ifname]), nil

		case strings.HasPrefix(method, "rrm_nr_set"):
			if r.drop > 0 {
				// Accepted but lost, exactly like a write to a hostapd instance
				// that is being rebuilt.
				r.drop--
				return "{}", nil
			}
			payload := strings.TrimSpace(strings.TrimPrefix(method, "rrm_nr_set"))
			var arg struct {
				List [][]string `json:"list"`
			}
			if err := json.Unmarshal([]byte(payload), &arg); err != nil {
				return "", fmt.Errorf("bad payload %q: %w", payload, err)
			}
			r.lists[ifname] = arg.List
			return "{}", nil

		case strings.HasPrefix(method, "rrm_nr_get_own"):
			if out, ok := r.own["hostapd."+ifname]; ok {
				return out, nil
			}
			return `{"value":[]}`, nil
		}
		return "", errors.New("unexpected hostapd method: " + method)
	}

	// `wifi up` is the wireless recovery path; the stub records it instead of
	// pretending to reconfigure radios.
	if key == "wifi up" {
		r.wifiUp++
		return "", nil
	}

	return "", errors.New("unexpected command: " + key)
}

func (r *neighborRunner) wifiUpCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.wifiUp
}

func marshalList(l [][]string) string {
	if l == nil {
		l = [][]string{}
	}
	blob, _ := json.Marshal(map[string]any{"list": l})
	return string(blob)
}

func (r *neighborRunner) setCalls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		if strings.Contains(c, "rrm_nr_set") {
			out = append(out, c)
		}
	}
	return out
}

func (r *neighborRunner) currentList(ifname string) [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lists[ifname]
}

// newNeighborRunner seeds empty lists, which also makes hasInterface succeed.
func newNeighborRunner(ifnames ...string) *neighborRunner {
	r := &neighborRunner{lists: map[string][][]string{}, own: map[string]string{}}
	for _, n := range ifnames {
		r.lists[n] = nil
	}
	return r
}

const ownReportFixture = `{
	"value": [
		"00:00:00:00:00:23",
		"SSID-A_5G",
		"000000000023ef58200081240e0603033200"
	]
}`

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A fresh agent must report its own BSSes so the controller can build peer lists.
func TestNeighborCollectReadsEveryBSS(t *testing.T) {
	r := &neighborRunner{own: map[string]string{
		"hostapd.phy0.1-ap0": ownReportFixture,
		"hostapd.phy0.0-ap0": ownReportFixture,
	}}
	c := &NeighborCollector{Runner: r, Log: testLog()}

	got := c.Collect(context.Background())
	if len(got) != 2 {
		t.Fatalf("collected %d entries, want 2", len(got))
	}
	// Sorted by interface name, so the hello payload is stable.
	if got[0].Ifname != "phy0.0-ap0" || got[1].Ifname != "phy0.1-ap0" {
		t.Errorf("order = %s,%s", got[0].Ifname, got[1].Ifname)
	}
	if got[0].BSSID != "00:00:00:00:00:23" || got[0].SSID != "SSID-A_5G" {
		t.Errorf("entry = %+v", got[0])
	}
	if got[0].Report != "000000000023ef58200081240e0603033200" {
		t.Errorf("report body was altered: %q", got[0].Report)
	}
}

// A BSS that is mid-reload or lacks rr must not blind the others.
func TestNeighborCollectSkipsBrokenBSS(t *testing.T) {
	r := &neighborRunner{own: map[string]string{
		"hostapd.phy0.1-ap0": ownReportFixture,
		"hostapd.phy0.2-ap0": `{"value":[]}`,
	}}
	c := &NeighborCollector{Runner: r, Log: testLog()}

	if got := c.Collect(context.Background()); len(got) != 1 {
		t.Fatalf("collected %d entries, want 1", len(got))
	}
}

// The controller's list is authoritative: it must land in hostapd verbatim, and
// it must actually be readable afterwards.
func TestEnsureNeighborsWritesWhenHostapdIsEmpty(t *testing.T) {
	r := newNeighborRunner("phy0.1-ap0")
	a := &Applier{StateDir: t.TempDir(), Runner: r, Log: testLog(), Sleep: func(time.Duration) {}}

	body := []byte(`{"phy0.1-ap0":[["aa:bb:cc:dd:ee:01","SSID-A_5G","00aabbccddee01"]]}`)
	a.EnsureNeighbors(context.Background(), body)

	if n := len(r.setCalls()); n != 1 {
		t.Fatalf("rrm_nr_set calls = %d, want 1", n)
	}
	got := r.currentList("phy0.1-ap0")
	if len(got) != 1 || got[0][0] != "aa:bb:cc:dd:ee:01" || got[0][2] != "00aabbccddee01" {
		t.Errorf("hostapd list = %v", got)
	}
}

// Idempotence: a cycle that finds hostapd already correct must not rewrite it.
func TestEnsureNeighborsIsIdempotent(t *testing.T) {
	r := newNeighborRunner("phy0.1-ap0")
	r.lists["phy0.1-ap0"] = [][]string{{"AA:BB:CC:DD:EE:01", "SSID-A_5G", "x"}}
	a := &Applier{StateDir: t.TempDir(), Runner: r, Log: testLog(), Sleep: func(time.Duration) {}}

	body := []byte(`{"phy0.1-ap0":[["aa:bb:cc:dd:ee:01","SSID-A_5G","x"]]}`)
	a.EnsureNeighbors(context.Background(), body)

	if n := len(r.setCalls()); n != 0 {
		t.Errorf("rewrote an already-correct list %d time(s)", n)
	}
}

// The reason the retry loop exists: `wifi reload` returns before hostapd has
// rebuilt every radio, so a write can land on an instance about to be replaced.
// On real hardware this left all 11 lists empty after an apply.
func TestEnsureNeighborsRetriesWhenHostapdDropsTheWrite(t *testing.T) {
	r := newNeighborRunner("phy0.1-ap0")
	r.drop = 2 // the first two writes are accepted but lost
	a := &Applier{StateDir: t.TempDir(), Runner: r, Log: testLog(), Sleep: func(time.Duration) {}}

	body := []byte(`{"phy0.1-ap0":[["aa:bb:cc:dd:ee:01","SSID-A_5G","x"]]}`)
	a.EnsureNeighbors(context.Background(), body)

	if n := len(r.setCalls()); n != neighbourSetAttempts {
		t.Errorf("rrm_nr_set calls = %d, want %d (retried until it stuck)", n, neighbourSetAttempts)
	}
	if got := r.currentList("phy0.1-ap0"); len(got) != 1 {
		t.Errorf("list still empty after retries: %v", got)
	}
}

// A BSS that never accepts the write must not spin forever.
func TestEnsureNeighborsGivesUpAfterAttempts(t *testing.T) {
	r := newNeighborRunner("phy0.1-ap0")
	r.drop = 99
	a := &Applier{StateDir: t.TempDir(), Runner: r, Log: testLog(), Sleep: func(time.Duration) {}}

	a.EnsureNeighbors(context.Background(), []byte(`{"phy0.1-ap0":[["aa:bb:cc:dd:ee:01","SSID-A_5G","x"]]}`))

	if n := len(r.setCalls()); n != neighbourSetAttempts {
		t.Errorf("rrm_nr_set calls = %d, want exactly %d", n, neighbourSetAttempts)
	}
}

// A BSS named in the package but absent locally must be skipped, not written to
// a hostapd object that does not exist.
func TestEnsureNeighborsSkipsAbsentInterface(t *testing.T) {
	r := newNeighborRunner()
	r.absent = []string{"hostapd.phy9.9-ap9"}
	a := &Applier{StateDir: t.TempDir(), Runner: r, Log: testLog(), Sleep: func(time.Duration) {}}

	a.EnsureNeighbors(context.Background(), []byte(`{"phy9.9-ap9":[["aa:bb:cc:dd:ee:01","X","00"]]}`))

	if n := len(r.setCalls()); n != 0 {
		t.Errorf("wrote to a missing BSS %d time(s)", n)
	}
}

// The lists only live in hostapd memory, so they must survive a reboot: that is
// what the on-disk cache is for.
func TestNeighborCacheRoundTrips(t *testing.T) {
	dir := t.TempDir()
	a := &Applier{StateDir: dir, Runner: newNeighborRunner(), Log: testLog()}

	body := []byte(`{"phy0.1-ap0":[["aa:bb:cc:dd:ee:01","SSID-A_5G","00"]]}`)
	a.cacheNeighbors(body)

	if got := a.cachedNeighbors(); string(got) != string(body) {
		t.Errorf("cache = %q, want %q", got, body)
	}
	if _, err := os.Stat(filepath.Join(dir, neighborsCacheFile)); err != nil {
		t.Errorf("cache file missing: %v", err)
	}
}

func TestCachedNeighborsMissingIsNil(t *testing.T) {
	a := &Applier{StateDir: t.TempDir(), Log: testLog()}
	if got := a.cachedNeighbors(); got != nil {
		t.Errorf("cachedNeighbors = %q, want nil", got)
	}
}

func TestSameNeighborListIgnoresCaseAndOrder(t *testing.T) {
	want := [][]string{
		{"aa:bb:cc:dd:ee:01", "SSID-A", "00"},
		{"aa:bb:cc:dd:ee:02", "SSID-A", "00"},
	}
	cur := `{"list":[["AA:BB:CC:DD:EE:02","SSID-A","z"],["AA:BB:CC:DD:EE:01","SSID-A","y"]]}`
	if !sameNeighborList(cur, want) {
		t.Error("same set reported as different")
	}

	shorter := `{"list":[["AA:BB:CC:DD:EE:01","SSID-A","y"]]}`
	if sameNeighborList(shorter, want) {
		t.Error("missing peer reported as equal")
	}
	if sameNeighborList("garbage", want) {
		t.Error("unparseable list reported as equal")
	}
}

// The failure this guards: the health check passes because the radio reports up,
// while a BSS doing ACS is not serviced yet. hostapd then discards the list that
// was written to it, and that BSS stays empty until the periodic refresh — five
// minutes of 11k silently doing nothing.
func TestSettleNeighborsWaitsForALateBSS(t *testing.T) {
	r := newNeighborRunner("phy0.0-ap0", "phy0.1-ap0")
	// phy0.0-ap0 is the slow one: it does not exist yet.
	r.absent = []string{"hostapd.phy0.0-ap0"}
	r.hook = func() {
		if len(r.calls) >= 4 {
			r.absent = nil // ACS finished, the BSS shows up
		}
	}
	a := &Applier{StateDir: t.TempDir(), Runner: r, Log: testLog(), Sleep: func(time.Duration) {}}

	body := []byte(`{"phy0.0-ap0":[["aa:bb:cc:dd:ee:01","SSID-A","00"]],` +
		`"phy0.1-ap0":[["aa:bb:cc:dd:ee:02","SSID-A_5G","00"]]}`)
	a.settleNeighbors(context.Background(), body)

	if got := r.currentList("phy0.0-ap0"); len(got) != 1 {
		t.Errorf("late BSS never got its list: %v", got)
	}
	if got := r.currentList("phy0.1-ap0"); len(got) != 1 {
		t.Errorf("fast BSS never got its list: %v", got)
	}
}

// A BSS that already holds its list must not be rewritten: settle is a no-op
// when there is nothing to catch up on.
func TestSettleNeighborsIsCheapWhenInSync(t *testing.T) {
	r := newNeighborRunner("phy0.1-ap0")
	a := &Applier{StateDir: t.TempDir(), Runner: r, Log: testLog(), Sleep: func(time.Duration) {}}

	body := []byte(`{"phy0.1-ap0":[["aa:bb:cc:dd:ee:02","SSID-A_5G","00"]]}`)
	a.EnsureNeighbors(context.Background(), body)
	before := len(r.setCalls())

	a.settleNeighbors(context.Background(), body)
	if got := len(r.setCalls()); got != before {
		t.Errorf("settle wrote %d extra time(s) to an in-sync BSS", got-before)
	}
}

// A BSS that never appears must not wedge the apply. It is reported and left to
// the periodic refresh.
func TestSettleNeighborsGivesUpOnAMissingBSS(t *testing.T) {
	r := newNeighborRunner()
	r.absent = []string{"hostapd.phy9.9-ap9"}
	a := &Applier{
		StateDir: t.TempDir(), Runner: r, Log: testLog(),
		Sleep:           func(time.Duration) {},
		NeighbourSettle: 5 * time.Millisecond,
	}

	done := make(chan struct{})
	go func() {
		a.settleNeighbors(context.Background(), []byte(`{"phy9.9-ap9":[["aa:bb:cc:dd:ee:01","X","00"]]}`))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("settle did not return for a BSS that never appears")
	}
}

// Boot is the worst case for that race: every radio is doing ACS, so the first
// restore pass finds most BSSes still missing. Without the settle window the
// lists would stay empty until the periodic refresh, fifteen cycles later.
func TestFirstCycleRestoreSettlesNeighbours(t *testing.T) {
	r := newNeighborRunner("phy0.0-ap0")
	r.absent = []string{"hostapd.phy0.0-ap0"}
	r.hook = func() {
		if len(r.calls) >= 4 {
			r.absent = nil // ACS finished, the BSS shows up
		}
	}
	ap := &Applier{StateDir: t.TempDir(), Runner: r, Log: testLog(), Sleep: func(time.Duration) {}}
	ap.cacheNeighbors([]byte(`{"phy0.0-ap0":[["aa:bb:cc:dd:ee:01","SSID-A","00"]]}`))

	a := &Agent{Applier: ap, Log: testLog()}
	a.cycles = 1
	a.restoreNeighborsIfDue(context.Background())

	if got := r.currentList("phy0.0-ap0"); len(got) != 1 {
		t.Errorf("boot restore left the BSS without a list: %v", got)
	}
}

// A dry run is a rehearsal: it stages files and never touches the device, not
// even hostapd's in-memory state.
func TestEnsureNeighborsIsSkippedInDryRun(t *testing.T) {
	r := newNeighborRunner("phy0.1-ap0")
	a := &Applier{StateDir: t.TempDir(), Runner: r, Log: testLog(), Sleep: func(time.Duration) {}, DryRun: true}

	a.EnsureNeighbors(context.Background(), []byte(`{"phy0.1-ap0":[["aa:bb:cc:dd:ee:01","SSID-A_5G","00"]]}`))

	if got := len(r.setCalls()); got != 0 {
		t.Errorf("dry run wrote %d time(s) to hostapd", got)
	}
}

// Bringing the radios up disconnects every client. That must never be a side
// effect of a rehearsal, however broken wireless looks.
func TestEnsureWirelessIsSkippedInDryRun(t *testing.T) {
	r := newNeighborRunner()
	r.absent = []string{"hostapd.phy0.0-ap0"}
	a := &Applier{StateDir: t.TempDir(), Runner: r, Log: testLog(), Sleep: func(time.Duration) {}, DryRun: true}

	if a.EnsureWireless(context.Background(), []byte(`{"phy0.0-ap0":[["aa:bb:cc:dd:ee:01","SSID-A","00"]]}`)) {
		t.Error("dry run acted on missing wireless")
	}
	if got := r.wifiUpCalls(); got != 0 {
		t.Errorf("dry run ran wifi up %d time(s)", got)
	}
}

// A reboot can leave hostapd serving nothing at all — measured twice on real
// hardware, where every BSS came up during boot and was then torn down again.
// Nothing else recovers it: a reboot does not change the revision.
func TestEnsureWirelessBringsUpRadiosWhenNoBSSExists(t *testing.T) {
	r := newNeighborRunner()
	r.absent = []string{"hostapd.phy0.0-ap0", "hostapd.phy0.1-ap0"}
	a := &Applier{StateDir: t.TempDir(), Runner: r, Log: testLog(), Sleep: func(time.Duration) {}}

	body := []byte(`{"phy0.0-ap0":[["aa:bb:cc:dd:ee:01","SSID-A","00"]],` +
		`"phy0.1-ap0":[["aa:bb:cc:dd:ee:02","SSID-A_5G","00"]]}`)
	if !a.EnsureWireless(context.Background(), body) {
		t.Fatal("EnsureWireless did not act with every expected BSS missing")
	}
	if got := r.wifiUpCalls(); got != 1 {
		t.Errorf("wifi up calls = %d, want 1", got)
	}
}

// One surviving BSS means the radios are up. Restarting wireless to "fix" that
// would disconnect every client on the AP for nothing.
func TestEnsureWirelessSkipsWhenAnyBSSExists(t *testing.T) {
	r := newNeighborRunner("phy0.1-ap0")
	r.absent = []string{"hostapd.phy0.0-ap0"}
	a := &Applier{StateDir: t.TempDir(), Runner: r, Log: testLog(), Sleep: func(time.Duration) {}}

	body := []byte(`{"phy0.0-ap0":[["aa:bb:cc:dd:ee:01","SSID-A","00"]],` +
		`"phy0.1-ap0":[["aa:bb:cc:dd:ee:02","SSID-A_5G","00"]]}`)
	if a.EnsureWireless(context.Background(), body) {
		t.Error("EnsureWireless acted while a BSS was still up")
	}
	if got := r.wifiUpCalls(); got != 0 {
		t.Errorf("wifi up calls = %d, want 0", got)
	}
}

// A radio that never comes up must not be restarted on every cycle.
func TestEnsureWirelessIsThrottled(t *testing.T) {
	r := newNeighborRunner()
	r.absent = []string{"hostapd.phy0.0-ap0"}
	a := &Applier{
		StateDir: t.TempDir(), Runner: r, Log: testLog(), Sleep: func(time.Duration) {},
		WirelessRetryEvery: time.Hour,
	}
	body := []byte(`{"phy0.0-ap0":[["aa:bb:cc:dd:ee:01","SSID-A","00"]]}`)

	a.EnsureWireless(context.Background(), body)
	a.EnsureWireless(context.Background(), body)

	if got := r.wifiUpCalls(); got != 1 {
		t.Errorf("wifi up calls = %d, want 1 (second attempt throttled)", got)
	}
}

// The periodic refresh must stay cheap: it writes lists and recovers radios, but
// it must not run the settle loop. A BSS that never comes up cannot be allowed to
// hold the heartbeat loop for a whole settle window.
func TestPeriodicRestoreStaysASinglePass(t *testing.T) {
	r := newNeighborRunner("phy0.1-ap0")
	r.absent = []string{"hostapd.phy0.0-ap0"}
	ap := &Applier{
		StateDir: t.TempDir(), Runner: r, Log: testLog(),
		Sleep:           func(time.Duration) {},
		NeighbourSettle: 20 * time.Millisecond,
	}
	ap.cacheNeighbors([]byte(`{"phy0.0-ap0":[["aa:bb:cc:dd:ee:01","SSID-A","00"]],` +
		`"phy0.1-ap0":[["aa:bb:cc:dd:ee:02","SSID-A_5G","00"]]}`))

	a := &Agent{Applier: ap, Log: testLog()}
	a.cycles = neighborRefreshCycles
	a.restoreNeighborsIfDue(context.Background())

	// A settle loop would have spun for the whole window, and a running radio
	// must never be torn down by the periodic pass.
	if got := r.wifiUpCalls(); got != 0 {
		t.Errorf("periodic restore ran wifi up %d time(s) with a BSS still up", got)
	}
	if got := len(r.calls); got > 10 {
		t.Errorf("periodic restore made %d ubus calls, want a single pass", got)
	}
}

// Boot with the radios down: the settle window expires with nothing to write to,
// and that is the signal that wireless never came up. Nothing else would ever
// reload it, because a reboot does not change the revision.
func TestFirstCycleRestoreBringsUpRadiosWhenNothingIsRunning(t *testing.T) {
	r := newNeighborRunner("phy0.0-ap0")
	r.absent = []string{"hostapd.phy0.0-ap0"}
	r.hook = func() {
		// The BSS only shows up once the radios are brought up, not on its own.
		if r.wifiUp > 0 {
			r.absent = nil
		}
	}
	ap := &Applier{
		StateDir: t.TempDir(), Runner: r, Log: testLog(),
		Sleep:           func(time.Duration) {},
		NeighbourSettle: 50 * time.Millisecond,
	}
	ap.cacheNeighbors([]byte(`{"phy0.0-ap0":[["aa:bb:cc:dd:ee:01","SSID-A","00"]]}`))

	a := &Agent{Applier: ap, Log: testLog()}
	a.cycles = 1
	a.restoreNeighborsIfDue(context.Background())

	if got := r.wifiUpCalls(); got != 1 {
		t.Errorf("wifi up calls = %d, want 1", got)
	}
	if got := r.currentList("phy0.0-ap0"); len(got) != 1 {
		t.Errorf("BSS still without a list after recovery: %v", got)
	}
}

// Existence used to be asked per BSS: eleven `ubus list hostapd.X` calls to
// learn one fact. `ubus list` returns the whole object table for the price of
// one spawn, so a sweep must read it exactly once and reuse it for every
// hasInterface call inside that sweep.
func TestEnsureNeighborsListsHostapdOnce(t *testing.T) {
	r := newNeighborRunner("phy0.0-ap0", "phy0.1-ap0", "phy0.2-ap0")
	a := &Applier{StateDir: t.TempDir(), Runner: r, Log: testLog(), Sleep: func(time.Duration) {}}

	body := []byte(`{"phy0.0-ap0":[["aa:bb:cc:dd:ee:01","SSID-A","00"]],` +
		`"phy0.1-ap0":[["aa:bb:cc:dd:ee:02","SSID-A_5G","00"]],` +
		`"phy0.2-ap0":[["aa:bb:cc:dd:ee:03","HOME_6G","00"]]}`)
	a.EnsureNeighbors(context.Background(), body)

	if n := countCalls(r.calls, "ubus list"); n != 1 {
		t.Fatalf("ubus list calls = %d, want 1 per sweep", n)
	}
	for _, ifname := range []string{"phy0.0-ap0", "phy0.1-ap0", "phy0.2-ap0"} {
		if got := r.currentList(ifname); len(got) != 1 {
			t.Errorf("%s never got its list: %v", ifname, got)
		}
	}
}

// The cached set must not outlive the sweep that read it: a BSS that vanishes
// between two passes has to be seen as gone, because EnsureWireless turns
// "nothing is running" into `wifi up`, which drops every client on the AP.
func TestEnsureWirelessSeesBSSVanishBetweenSweeps(t *testing.T) {
	r := newNeighborRunner("phy0.1-ap0")
	a := &Applier{StateDir: t.TempDir(), Runner: r, Log: testLog(), Sleep: func(time.Duration) {}}
	ctx := context.Background()
	body := []byte(`{"phy0.1-ap0":[["aa:bb:cc:dd:ee:01","SSID-A_5G","00"]]}`)

	if a.EnsureWireless(ctx, body) {
		t.Fatal("must not bring radios up while a BSS is being served")
	}

	// hostapd tore every BSS down after a reboot, the failure this guard exists
	// for.
	r.mu.Lock()
	r.lists = map[string][][]string{}
	r.mu.Unlock()

	if !a.EnsureWireless(ctx, body) {
		t.Fatal("a vanished BSS must trigger recovery, not a cached all-clear")
	}
}

// TestNeighborCollectSocketMatchesCLI holds the invariant this collector lives
// by: the report body is opaque, so whatever the socket returns has to reach
// hostapd byte for byte. Only the transport may change.
func TestNeighborCollectSocketMatchesCLI(t *testing.T) {
	own := map[string]string{
		"hostapd.phy0.0-ap0": ownReportFixture,
		"hostapd.phy0.1-ap0": ownReportFixture,
	}
	cli := &NeighborCollector{Runner: &neighborRunner{own: own}, Log: testLog()}
	want := cli.Collect(context.Background())
	if len(want) != 2 {
		t.Fatalf("CLI collected %d entries, want 2", len(want))
	}

	// `ubus list` still comes from the CLI here, which is the production shape:
	// only the per-BSS read moved.
	runner := &neighborRunner{own: own}
	socket := &NeighborCollector{
		Runner: runner,
		Log:    testLog(),
		Ubus:   testUbusClient(fakeUbusReplies(t, own)),
	}
	got := socket.Collect(context.Background())

	for _, call := range runner.calls {
		if strings.HasPrefix(call, "ubus call") {
			t.Fatalf("socket path still forked: %v", runner.calls)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("socket entries differ from the CLI entries://n got %+v\nwant %+v", got, want)
	}
	if got[0].Report != "000000000023ef58200081240e0603033200" {
		t.Fatalf("report body was altered: %q", got[0].Report)
	}
}

func TestNeighborCollectFallsBackToCLI(t *testing.T) {
	own := map[string]string{"hostapd.phy0.1-ap0": ownReportFixture}
	broken := &fakeTransport{
		lookupFn: func(context.Context, string) (uint32, error) {
			return 0, errors.New("dial unix /var/run/ubus/ubus.sock: connect: no such file or directory")
		},
	}
	runner := &neighborRunner{own: own}
	c := &NeighborCollector{Runner: runner, Log: testLog(), Ubus: testUbusClient(broken)}

	if got := c.Collect(context.Background()); len(got) != 1 {
		t.Fatalf("fallback collected %d entries, want 1", len(got))
	}
	var sawCall bool
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "ubus call") {
			sawCall = true
		}
	}
	if !sawCall {
		t.Fatal("the CLI was never used, so this did not test the fallback")
	}
}
