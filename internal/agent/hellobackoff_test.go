package agent

import (
	"testing"
	"time"
)

// The backoff must leave the early failures alone: both re-discovery and the
// autonomous-steering fallback count consecutive failures, and stretching them
// would delay "the controller is gone" by that same amount.
func TestHelloBackoffKeepsNominalCadenceUntilControlIsLost(t *testing.T) {
	base := 20 * time.Second
	limit := 60 * time.Second

	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, base},
		{1, base},
		{2, base},
		{3, base}, // rediscoverAfterFailures: reached, not yet exceeded
		{4, 2 * base},
		{5, limit},
		{9, limit},
	}
	for _, tc := range cases {
		a := &Agent{Config: Config{HelloBackoffMax: limit}}
		a.helloFailures = tc.failures
		if got := a.helloBackoff(base); got != tc.want {
			t.Errorf("failures=%d: backoff = %s, want %s", tc.failures, got, tc.want)
		}
	}
}

func TestHelloBackoffHonoursConfiguredCap(t *testing.T) {
	a := &Agent{Config: Config{HelloBackoffMax: 30 * time.Second}}
	a.helloFailures = 8
	if got := a.helloBackoff(20 * time.Second); got != 30*time.Second {
		t.Fatalf("backoff = %s, want the 30s cap", got)
	}
}

// A cap below the nominal interval must never make the agent poll faster than
// its configured heartbeat: that would turn an outage into a tighter loop.
func TestHelloBackoffNeverUndercutsInterval(t *testing.T) {
	a := &Agent{Config: Config{HelloBackoffMax: 5 * time.Second}}
	a.helloFailures = 6
	if got := a.helloBackoff(20 * time.Second); got != 20*time.Second {
		t.Fatalf("backoff = %s, want the nominal 20s", got)
	}
}

func TestHelloBackoffDefaultCap(t *testing.T) {
	a := &Agent{}
	a.helloFailures = 50
	if got := a.helloBackoff(20 * time.Second); got != defaultHelloBackoffMax {
		t.Fatalf("backoff = %s, want %s", got, defaultHelloBackoffMax)
	}
}

func TestLoadReadsHelloBackoffFromEnvironment(t *testing.T) {
	t.Setenv("AC_HELLO_BACKOFF_MAX", "45s")
	c := Load(Config{})
	if c.HelloBackoffMax != 45*time.Second {
		t.Fatalf("hello backoff cap = %s, want 45s", c.HelloBackoffMax)
	}

	// An unparseable value must not disable the cap silently, and an explicit
	// field always wins over the environment.
	t.Setenv("AC_HELLO_BACKOFF_MAX", "not-a-duration")
	if got := Load(Config{}); got.HelloBackoffMax != defaultHelloBackoffMax {
		t.Fatalf("with a bad value: cap = %s, want the default", got.HelloBackoffMax)
	}
	if got := Load(Config{HelloBackoffMax: 10 * time.Second}); got.HelloBackoffMax != 10*time.Second {
		t.Fatalf("explicit value overridden: %s", got.HelloBackoffMax)
	}
}
