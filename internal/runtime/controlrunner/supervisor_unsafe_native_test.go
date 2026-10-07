//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"encoding/json"
	"fmt"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"golang.org/x/sys/unix"
	"os"
	"testing"
	"time"
)

func nativePreLoss(t *testing.T, e *Execution, method string, signal int) int {
	t.Helper()
	fields := observeActualUser(t, e)
	e.mu.Lock()
	monitor, root, command := e.monitorPID, e.rootPID, e.record.Context.CommandID
	e.mu.Unlock()
	event := map[string]any{"version": 1, "command_id": command, "monitor_pid": monitor, "root_pid": root, "root_observed": map[string]any{"uids": []int{1000, 1000, 1000, 1000}, "gids": []int{1000, 1000, 1000, 1000}, "groups": []int{}, "caps": map[string]uint64{"inheritable": 0, "permitted": 0, "effective": 0, "bounding": 0, "ambient": 0}, "no_new_privs": 1, "seccomp": 2, "ppid": monitor, "cgroup": fields["cgroup"]}, "trigger": map[string]any{"method": method, "signal": signal}}
	wire, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "sandbox-pre-loss %s\n", wire)
	return monitor
}

// Both selectors intentionally require actual controller-observed exit70. A Go
// timeout or return PASS is never successful namespace-isolation evidence.
func TestTask3NativeMonitorWatchdog(t *testing.T) {
	f := newNativeFixture(t)
	e, _ := f.start(t, controlprotocol.ExecutionRequest{Argv: []string{os.Getenv("SANDBOX_TASK3_USER"), "hold"}, Env: map[string]string{"GOMAXPROCS": "2"}, UID: 1000, GID: 1000, WorkDir: "/tmp", TimeoutSeconds: 60}, 4*time.Second)
	waitExecutionReady(t, e)
	e.mu.Lock()
	monitor := e.monitorPID
	e.mu.Unlock()
	fd, err := unix.PidfdOpen(monitor, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	nativePreLoss(t, e, "held_pidfd_SIGSTOP", 19)
	if err = unix.PidfdSendSignal(fd, unix.SIGSTOP, nil, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(38 * time.Second)
	t.Fatal("absolute watchdog failed to isolate stopped monitor")
}
func TestTask3NativeOutputBackpressure(t *testing.T) {
	f := newNativeFixture(t)
	e, _ := f.start(t, controlprotocol.ExecutionRequest{Argv: []string{os.Getenv("SANDBOX_TASK3_USER"), "flood"}, Env: map[string]string{"GOMAXPROCS": "2"}, UID: 1000, GID: 1000, WorkDir: "/tmp", TimeoutSeconds: 25}, 25*time.Second)
	nativePreLoss(t, e, "bounded_output_backpressure", 0)
	// Deliberately never read e.output: the trusted test channel cannot be forged
	// by user bytes; production holds only the bounded two-frame output queue.
	time.Sleep(38 * time.Second)
	t.Fatal("bounded stream failure failed to isolate namespace")
}
