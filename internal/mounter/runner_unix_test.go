//go:build darwin || linux

package mounter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCommandRunnerTimeoutKillsAndReapsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child-pid")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (CommandRunner{}).Run(ctx, []string{"/bin/sh", "-c", "sleep 30 & child=$!; echo $child > \"$1\"; wait", "workspace-mounter-test", pidFile})
	}()
	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(pidFile)
		return err == nil && len(raw) != 0
	}, 3*time.Second, 10*time.Millisecond)
	raw, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	childPID, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.Eventually(t, func() bool {
		err := syscall.Kill(childPID, 0)
		return errors.Is(err, syscall.ESRCH)
	}, 3*time.Second, 10*time.Millisecond)
}
