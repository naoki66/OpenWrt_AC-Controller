package agent

import "testing"

func TestLoadTrimsControllerAddress(t *testing.T) {
	t.Setenv("AC_ADDR", "  env-controller:8090\n")

	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "environment", want: "env-controller:8090"},
		{name: "explicit", cfg: Config{ACAddr: "  configured-controller:8090\t"}, want: "configured-controller:8090"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Load(tt.cfg).ACAddr; got != tt.want {
				t.Fatalf("ACAddr = %q, want %q", got, tt.want)
			}
		})
	}
}
