//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"golang.org/x/sys/unix"
)

// A real root-readable marker proves a missing CLOEXEC boundary, without placing
// any real credential in a test fixture. No public arbitrary-start API is used.
func TestTask3NativeRejectsInheritedManagementFD(t *testing.T) {
	f := newNativeFixture(t)
	path := "/journal/protected/management-marker"
	if err := os.WriteFile(path, []byte("test-only root marker"), 0600); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(path, unix.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC != 0 {
		t.Fatal("counterexample requires actual non-CLOEXEC root marker")
	}
	e, _ := f.start(t, controlprotocol.ExecutionRequest{Argv: []string{os.Getenv("SANDBOX_TASK3_USER"), "binary"}, Env: map[string]string{"GOMAXPROCS": "2"}, UID: 1000, GID: 1000, WorkDir: "/tmp", TimeoutSeconds: 5}, 10*time.Second)
	out, _ := drainExecution(t, e)
	if bytes.Contains(out, []byte(path)) {
		t.Fatalf("actual user inherited protected management marker FD: %q", out)
	}
	t.Logf("actual user has no inherited root marker descriptor: %q", out)
}
