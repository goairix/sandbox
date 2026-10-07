//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"bytes"
	"context"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"os"
	"testing"
	"time"
)

func TestTask3NativeDescendantsAndConcurrent(t *testing.T) {
	f := newNativeFixture(t)
	request := controlprotocol.ExecutionRequest{Argv: []string{os.Getenv("SANDBOX_TASK3_USER"), "double"}, Env: map[string]string{"GOMAXPROCS": "2"}, UID: 1000, GID: 1000, WorkDir: "/tmp", TimeoutSeconds: 5}
	e, _ := f.start(t, request, 10*time.Second)
	out, _ := drainExecution(t, e)
	if !bytes.Contains(out, []byte("DOUBLE_READY")) || e.record.RootWaitStatus != 0 || !e.record.DrainConfirmed {
		t.Fatalf("double fork result %+v output%q", e.record, out)
	}
	request.Argv[1] = "hold"
	request.TimeoutSeconds = 2
	a, _ := f.start(t, request, 10*time.Second)
	request.TimeoutSeconds = 8
	b, _ := f.start(t, request, 10*time.Second)
	observeActualUser(t, a)
	observeActualUser(t, b)
	drainExecution(t, a)
	b.mu.Lock()
	pid := b.rootPID
	b.mu.Unlock()
	if _, err := os.Stat(procPath(pid)); err != nil {
		t.Fatalf("same-UID B was killed by A drain %v", err)
	}
	if a.record.RootWaitStatus != 9 || !a.record.DrainConfirmed {
		t.Fatalf("A result %+v", a.record)
	}
	if err := f.s.CloseAdmission(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	drainExecution(t, b)
	if b.record.RootWaitStatus != 9 || !b.record.DrainConfirmed {
		t.Fatalf("B cancel result %+v", b.record)
	}
	t.Log("actual setsid/doublefork root early exit drained; concurrent same-UID B survived A and separately canceled/drained")
}
