//go:build !windows

package workspaceprobe

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTimedOutOSIOHelperIsKilledAndReaped(t *testing.T) {
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	require.NoError(t, err)
	defer devNull.Close()
	process, err := os.StartProcess("/bin/sleep", []string{"sleep", "30"}, &os.ProcAttr{
		Dir: "/", Files: []*os.File{devNull, devNull, devNull},
	})
	require.NoError(t, err)
	pid := process.Pid
	child := newOSIOChild(process)

	err = waitBoundedIO(child, 10*time.Millisecond, time.Second, time.Millisecond)
	require.ErrorIs(t, err, ErrIOTimeout)
	require.Error(t, syscall.Kill(pid, 0), "killed helper must no longer exist")
	var status syscall.WaitStatus
	_, waitErr := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	require.True(t, errors.Is(waitErr, syscall.ECHILD), "helper wait status must already be consumed: %v", waitErr)
}
