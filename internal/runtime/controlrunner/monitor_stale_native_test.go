//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Test-owned negative IPC orchestration delays an initially future absolute
// authority until after expiry. Actual fixed production monitor must not spawn.
func TestTask3NativeQueuedAuthorityExpired(t *testing.T) {
	requireNative(t)
	reqR, reqW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reqR.Close()
	defer reqW.Close()
	ctlR, ctlW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer ctlR.Close()
	defer ctlW.Close()
	resultR, resultW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer resultR.Close()
	defer resultW.Close()
	diagnosticR, diagnosticW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer diagnosticR.Close()
	defer diagnosticW.Close()
	cmd := exec.Command(os.Getenv("SANDBOX_TASK3_MONITOR"), "monitor")
	cmd.Env = []string{"GOMAXPROCS=2"}
	cmd.ExtraFiles = []*os.File{reqR, ctlR, resultW}
	cmd.Stdout = diagnosticW
	cmd.Stderr = diagnosticW
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	// Register management wait before assertions, and bound all pipe IO/joins.
	t.Cleanup(func() {
		select {
		case <-waitDone:
		case <-time.After(3 * time.Second):
			t.Error("negative monitor wait did not join")
		}
	})
	reqR.Close()
	ctlR.Close()
	resultW.Close()
	diagnosticW.Close()
	mono, err := monitorMonotonic()
	if err != nil {
		t.Fatal(err)
	}
	start := monitorStart{Request: controlprotocol.ExecutionRequest{Argv: []string{os.Getenv("SANDBOX_TASK3_USER"), "quick"}, Env: map[string]string{"GOMAXPROCS": "2"}, UID: 1000, GID: 1000, WorkDir: "/tmp", TimeoutSeconds: 5}, AuthorityDeadlineNS: mono + int64(50*time.Millisecond), CommandDeadlineNS: mono + int64(5*time.Second)}
	time.Sleep(100 * time.Millisecond)
	reqW.SetWriteDeadline(time.Now().Add(time.Second))
	if err = writeMonitorRequest(reqW, start); err != nil {
		t.Fatal(err)
	}
	reqW.Close()
	resultR.SetReadDeadline(time.Now().Add(3 * time.Second))
	wire, err := io.ReadAll(io.LimitReader(resultR, 8193))
	if err != nil || len(wire) != 0 {
		t.Fatalf("expired request produced output/registration %q %v", wire, err)
	}
	diagnosticR.SetReadDeadline(time.Now().Add(time.Second))
	diagnostic, err := io.ReadAll(io.LimitReader(diagnosticR, 4096))
	if err != nil || !strings.Contains(string(diagnostic), "monitor deadline expired") {
		t.Fatalf("missing actual expiry refusal %q %v", diagnostic, err)
	}
	select {
	case err = <-waitDone:
		if err == nil {
			t.Fatal("stale production monitor succeeded")
		}
		waitDone <- err
	case <-time.After(time.Second):
		t.Fatal("management wait unjoined")
	}
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		children, err := os.ReadFile("/proc/self/task/" + task.Name() + "/children")
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || strings.TrimSpace(string(children)) != "" {
			t.Fatalf("actual child remained after stale monitor: %q %v", children, err)
		}
	}
	t.Logf("actual absolute queue expiry rejected before user spawn; no result frames, no PID1 children, management wait joined: %q", diagnostic)
}
