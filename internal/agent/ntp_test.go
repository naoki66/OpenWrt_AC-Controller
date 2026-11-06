package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/naoki66/ac-controller/internal/model"
)

func TestApplyNTPWritesUCIAndPersists(t *testing.T) {
	runner := &fakeRunner{}
	a := &Agent{
		Config: Config{Root: t.TempDir()},
		Runner: runner,
		Log:    testLog(),
	}
	if err := a.applyNTP(context.Background(), []string{"10.0.10.1", "NTP.Aliyun.com", "10.0.10.1", ""}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if want := []string{"10.0.10.1", "ntp.aliyun.com"}; !slices.Equal(a.state.NTPServers, want) {
		t.Fatalf("state = %v, want %v", a.state.NTPServers, want)
	}
	calls := strings.Join(runner.Calls(), "\n")
	for _, want := range []string{
		"uci -q set system.ntp=timeserver",
		"uci -q delete system.ntp.server",
		"uci -q add_list system.ntp.server=10.0.10.1",
		"uci -q add_list system.ntp.server=ntp.aliyun.com",
		"uci -q commit system",
		"/etc/init.d/sysntpd restart",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing %q in:\n%s", want, calls)
		}
	}

	// The same list is a no-op; a hello every 20s must not rewrite and restart
	// the NTP client each time.
	before := len(runner.Calls())
	if err := a.applyNTP(context.Background(), []string{"10.0.10.1", "ntp.aliyun.com"}); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if got := len(runner.Calls()); got != before {
		t.Errorf("repeat wrote %d extra command(s)", got-before)
	}
}

func TestApplyNTPIgnoresInvalidList(t *testing.T) {
	runner := &fakeRunner{}
	a := &Agent{Config: Config{Root: t.TempDir()}, Runner: runner, Log: testLog()}
	if err := a.applyNTP(context.Background(), []string{"not a host", "=bad", strings.Repeat("x", 300)}); err != nil {
		t.Fatalf("invalid list: %v", err)
	}
	if len(runner.Calls()) != 0 {
		t.Errorf("invalid list touched UCI: %v", runner.Calls())
	}
	if len(a.state.NTPServers) != 0 {
		t.Errorf("state changed on invalid list: %v", a.state.NTPServers)
	}
}

func TestApplyNTPFailureKeepsState(t *testing.T) {
	runner := &fakeRunner{failOn: map[string]error{
		"uci -q commit system": errors.New("uci: parse error"),
	}}
	a := &Agent{Config: Config{Root: t.TempDir()}, Runner: runner, Log: testLog()}
	if err := a.applyNTP(context.Background(), []string{"10.0.10.1"}); err == nil {
		t.Fatal("commit failure was reported as success")
	}
	if len(a.state.NTPServers) != 0 {
		t.Errorf("state advanced after failure: %v", a.state.NTPServers)
	}
}

func TestNormalizeNTPServers(t *testing.T) {
	got := model.NormalizeNTPServers([]string{
		" 10.0.10.1 ", "NTP.Aliyun.com", "10.0.10.1", "", "bad host", "2001:db8::1",
	})
	want := []string{"10.0.10.1", "ntp.aliyun.com", "2001:db8::1"}
	if !slices.Equal(got, want) {
		t.Errorf("normalized = %v, want %v", got, want)
	}
}
