package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Runner abstracts command execution so the apply pipeline can be tested
// without an OpenWrt system. Every external command the agent needs (ubus, wifi)
// goes through here, and nothing else does.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner runs real commands on the device.
type ExecRunner struct{}

// Run returns stdout and folds stderr into the error so failures are actionable
// in the log without a second call.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	switch name {
	case "ubus", "uci", "iw", "ip":
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 500 * time.Millisecond
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			return stdout.String(), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return stdout.String(), nil
}
