package apparmorloader

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"time"
)

const trustedParserPath = "/sbin/apparmor_parser"

// ExecParser never accepts parser flags, shell snippets or tenant paths. A deadline
// is mandatory even for callers outside Loader. Cache use is disabled because the
// policy arrives over stdin; AppArmor securityfs must be mounted at its usual path.
func ExecParser(ctx context.Context, policy []byte, load bool) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
	}
	return runParser(ctx, trustedParserPath, policy, load)
}

func runParser(ctx context.Context, binary string, policy []byte, load bool) error {
	flags := []string{"--add", "--skip-cache"}
	if !load {
		flags = append(flags, "--skip-kernel-load")
	}
	command := exec.CommandContext(ctx, binary, flags...)
	command.WaitDelay = time.Second
	command.Stdin = bytes.NewReader(policy)
	// Parser diagnostics may repeat policy contents; do not expose or accumulate them.
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("trusted apparmor_parser failed: %w", err)
	}
	return ctx.Err()
}
