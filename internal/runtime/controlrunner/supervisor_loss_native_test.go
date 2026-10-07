//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"os"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"golang.org/x/sys/unix"
)

// This selector intentionally cannot return Go PASS. The controller must match
// the trusted registration/loss marker to actual PID1 exit70 and non-OOM status.
func TestTask3NativeMonitorLoss(t *testing.T) {
	f := newNativeFixture(t)
	e, _ := f.start(t, controlprotocol.ExecutionRequest{Argv: []string{os.Getenv("SANDBOX_TASK3_USER"), "hold"}, Env: map[string]string{"GOMAXPROCS": "2"}, UID: 1000, GID: 1000, WorkDir: "/tmp", TimeoutSeconds: 25}, 25*time.Second)
	waitExecutionReady(t, e)
	e.mu.Lock()
	monitor := e.monitorPID
	e.mu.Unlock()
	fd, err := unix.PidfdOpen(monitor, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	nativePreLoss(t, e, "held_pidfd_SIGKILL", 9)
	if err = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil {
		t.Fatal(err)
	}
	// No wait4 is introduced here: production's sole Cmd.Wait owns this monitor.
	time.Sleep(35 * time.Second)
	t.Fatal("PID1 failed to isolate after monitor loss")
}
