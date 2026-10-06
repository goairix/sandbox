//go:build linux

package launcher

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestKernelWorkerCleanup(t *testing.T) {
	mode := os.Getenv(nativeModeEnv)
	if mode == "cleanup-fatal" || mode == "cleanup-negative" {
		nativeFatalCleanupFixture(t, mode)
		return
	}
	requireNativeBoundary(t)
	for _, mode := range []string{"cleanup-fatal", "cleanup-negative"} {
		t.Run(mode, func(t *testing.T) {
			p, err := startNativeProcess(mode, func(cmd *exec.Cmd) {
				cmd.Args = append(cmd.Args, "-test.run=^TestKernelWorkerCleanup$", "-test.v", "-test.count=1", "-test.timeout=15s")
			})
			if err != nil {
				t.Fatal(err)
			}
			var childPID int
			pidfd := -1
			// Registered before waiting for readiness, any Fatal or the helper's Fatal.
			// Stop the helper first; then its exact child can be reaped by namespace PID1.
			t.Cleanup(func() {
				if err := p.stop(); err != nil {
					t.Error(err)
				}
				if childPID == 0 {
					childPID, _ = cleanupChildPID(p.output.String())
				}
				if childPID > 0 {
					if pidfd < 0 {
						pidfd, err = unix.PidfdOpen(childPID, 0)
						if err != nil && !errors.Is(err, unix.ESRCH) {
							t.Error(err)
						}
					}
					if err := reapNativeCleanupChild(childPID, pidfd); err != nil {
						t.Error(err)
					}
				} else {
					t.Error("missing exact child identity for cleanup")
				}
				if pidfd >= 0 {
					if err := unix.Close(pidfd); err != nil {
						t.Error(err)
					}
				}
				for _, line := range strings.Split(strings.TrimSpace(p.output.String()), "\n") {
					t.Logf("expected-Fatal-helper output: %s", line)
				}
			})
			var phase string
			select {
			case phase = <-p.output.phases:
			case <-p.done:
				t.Fatalf("cleanup helper exited before child identity: %v", p.waitErr)
			case <-time.After(5 * time.Second):
				t.Fatal("cleanup helper readiness timeout")
			}
			childPID, err = cleanupChildPID(phase)
			if err != nil {
				t.Fatal(err)
			}
			pidfd, err = unix.PidfdOpen(childPID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err = p.release(); err != nil {
				t.Fatal(err)
			}
			err = p.wait()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("actual Fatal helper exit=%v want exit1", err)
			}
			output := p.output.String()
			if !strings.Contains(output, "intentional cleanup Fatal") {
				t.Fatal("helper did not reach intended Fatal")
			}
			data, procErr := readBounded(fmt.Sprintf("/proc/%d/status", childPID), maxStatusBytes)
			evidence := strings.Contains(output, "cleanup evidence verified: child waited exit0; workers joined")
			if mode == "cleanup-fatal" {
				if !errors.Is(procErr, os.ErrNotExist) {
					t.Fatalf("Fatal cleanup left actual child %d present (read=%v): %s", childPID, procErr, data)
				}
				if !evidence {
					t.Fatal("Fatal cleanup omitted actual wait/join evidence")
				}
			} else {
				if procErr != nil {
					t.Fatalf("missing-cleanup negative lacked actual live child: %v", procErr)
				}
				for _, line := range strings.Split(string(data), "\n") {
					if strings.HasPrefix(line, "State:") {
						t.Logf("independent negative child PID=%d %s", childPID, line)
						if strings.Contains(line, "Z (zombie)") {
							t.Fatal("negative child already zombie instead of live")
						}
					}
				}
				if evidence {
					t.Fatal("missing cleanup emitted false successful wait/join evidence")
				}
			}
			t.Logf("actual helper PID=%d exit=1 childPID=%d cleanupEvidence=%t", p.cmd.Process.Pid, childPID, evidence)
		})
	}
	if !t.Failed() {
		t.Log("kernel boundary verified: worker cleanup")
	}
}
func cleanupChildPID(output string) (int, error) {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "phase cleanup child ") {
			pid, err := strconv.Atoi(strings.TrimPrefix(line, "phase cleanup child "))
			if err != nil || pid <= 1 {
				return 0, fmt.Errorf("invalid cleanup child identity %q", line)
			}
			return pid, nil
		}
	}
	return 0, fmt.Errorf("missing cleanup child identity")
}
func reapNativeCleanupChild(pid, pidfd int) error {
	if pidfd >= 0 {
		err := unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0)
		if err != nil && !errors.Is(err, unix.ESRCH) {
			return err
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status unix.WaitStatus
		got, err := unix.Wait4(pid, &status, unix.WNOHANG, nil)
		if got == pid {
			fmt.Printf("independent cleanup child PID=%d actual wait status=%d signal=%v exit=%d\n", pid, status, status.Signal(), status.ExitStatus())
			return nil
		}
		if errors.Is(err, unix.ECHILD) {
			if _, statErr := os.Stat(fmt.Sprintf("/proc/%d", pid)); errors.Is(statErr, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("child %d still exists but not reaped: %w", pid, err)
		}
		if err != nil {
			return err
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("actual orphan %d did not join within 5 seconds", pid)
}
func nativeFatalCleanupFixture(t *testing.T, mode string) {
	if _, err := PrepareMonitor(); err != nil {
		t.Fatal(err)
	}
	stop, err := holdNativeThreads(2, nil)
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := startNativeProcess("cleanup-user", func(cmd *exec.Cmd) {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{}}, AmbientCaps: []uintptr{}}
	})
	if err != nil {
		t.Fatal(err)
	}
	acknowledged := false
	t.Cleanup(func() {
		if !acknowledged {
			if err := p.stop(); err != nil {
				t.Error(err)
			}
			return
		}
		if mode == "cleanup-negative" {
			fmt.Println("deliberate negative omitted child wait")
			return
		}
		// Use actual join results even though the test already failed via Fatal.
		// Joining workers precedes closing fixture FDs; child releases normally.
		result := stop()
		result = errors.Join(result, p.release())
		waitErr := p.wait()
		result = errors.Join(result, waitErr, p.stop())
		fmt.Printf("cleanup child PID=%d actual wait=%v\n", p.cmd.Process.Pid, waitErr)
		if result != nil {
			t.Errorf("Fatal cleanup failed: %v", result)
			return
		}
		fmt.Println("cleanup evidence verified: child waited exit0; workers joined")
	})
	if err = p.phase("phase cleanup user ready"); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("phase cleanup child %d\n", p.cmd.Process.Pid)
	if err = nativeContinue(); err != nil {
		t.Fatal(err)
	}
	acknowledged = true
	t.Fatal("intentional cleanup Fatal")
}
func nativeCleanupUser() (resultErr error) {
	stop, err := holdNativeThreads(2, nil)
	defer func() { resultErr = errors.Join(resultErr, stop()) }()
	if err != nil {
		return err
	}
	fmt.Println("phase cleanup user ready")
	if err = nativeContinue(); err != nil {
		// Deliberate missing-cleanup fixture survives the helper's closed stdin so
		// the independent PID1 oracle must kill and actually reap a live orphan.
		time.Sleep(30 * time.Second)
		return err
	}
	return nil
}
