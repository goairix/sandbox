//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"os"
	"testing"
	"time"
)

func TestTask3NativeQuickExit(t *testing.T) {
	f := newNativeFixture(t)
	for i := 0; i < 8; i++ {
		e, _ := f.start(t, controlprotocol.ExecutionRequest{Argv: []string{os.Getenv("SANDBOX_TASK3_USER"), "quick"}, Env: map[string]string{"GOMAXPROCS": "2"}, UID: 1000, GID: 1000, WorkDir: "/tmp", TimeoutSeconds: 5}, 10*time.Second)
		drainExecution(t, e)
		e.mu.Lock()
		state, record := e.state, e.record
		e.mu.Unlock()
		if state != "local_terminal" || record.RootWaitStatus != 0 || !record.DrainConfirmed {
			t.Fatalf("quick actual root lost: %s %+v", state, record)
		}
	}
	t.Log("eight actual quick users each ordinary attributable raw0 terminal and full drain, no namespace isolation")
}
