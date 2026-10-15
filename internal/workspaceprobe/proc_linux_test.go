//go:build linux

package workspaceprobe

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The helper runs during init on the kernel group leader so SYS_EXIT can leave
// that leader zombie while the sibling's private descriptor table stays live.
func init() {
	mode := os.Getenv("SANDBOX_PROC_THREAD_HELPER")
	if mode == "" {
		return
	}
	runtime.LockOSThread()
	if syscall.Gettid() != os.Getpid() {
		fmt.Println("helper was not initialized on the group leader")
		os.Exit(2)
	}
	ready := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		if err := syscall.Unshare(syscall.CLONE_FILES); err != nil {
			fmt.Println("unshare:", err)
			os.Exit(2)
		}
		fd, err := syscall.Open(os.Getenv("SANDBOX_PROC_THREAD_WORKSPACE"), syscall.O_WRONLY|syscall.O_CREAT, 0o600)
		if err != nil {
			fmt.Println("open:", err)
			os.Exit(2)
		}
		_ = fd // Keep the raw descriptor exclusively in this task's FD table.
		fmt.Println("ready")
		close(ready)
		for {
			_, _, _ = syscall.RawSyscall6(syscall.SYS_PPOLL, 0, 0, 0, 0, 0, 0)
		}
	}()
	<-ready
	if mode == "dead-leader" {
		_, _, _ = syscall.RawSyscall(syscall.SYS_EXIT, 0, 0, 0)
	}
	select {}
}

func TestProcLinuxPrivateThreadDescriptorsAndDeadLeader(t *testing.T) {
	for _, mode := range []string{"live-leader", "dead-leader"} {
		t.Run(mode, func(t *testing.T) {
			workspace := t.TempDir()
			path := filepath.Join(workspace, "data")
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), "SANDBOX_PROC_THREAD_HELPER="+mode, "SANDBOX_PROC_THREAD_WORKSPACE="+path)
			stdout, err := cmd.StdoutPipe()
			require.NoError(t, err)
			cmd.Stderr = os.Stderr
			require.NoError(t, cmd.Start())
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
			ready := make(chan string, 1)
			go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
			select {
			case line := <-ready:
				if line == "unshare: operation not permitted\n" {
					t.Skip("kernel sandbox disallows a private thread descriptor table")
				}
				require.Equal(t, "ready\n", line)
			case <-time.After(5 * time.Second):
				t.Fatal("private thread helper did not start")
			}
			pid := cmd.Process.Pid
			proc := procFS{root: "/proc"}
			if mode == "dead-leader" {
				require.Eventually(t, func() bool {
					stat, err := os.ReadFile(filepath.Join("/proc", fmt.Sprint(pid), "stat"))
					if err != nil {
						return false
					}
					identity, err := parseProcStat(pid, stat)
					return err == nil && identity.State == 'Z'
				}, 5*time.Second, 10*time.Millisecond, "leader must exit while siblings retain the descriptor")
			}
			require.NoError(t, proc.signal(pid, syscall.SIGSTOP))
			var identity processIdentity
			require.Eventually(t, func() bool {
				var err error
				identity, err = proc.identity(pid)
				return err == nil && isStopped(identity.State)
			}, 5*time.Second, 10*time.Millisecond, "all live tasks must be provably stopped")
			fds, err := proc.descriptors(pid)
			require.NoError(t, err)
			found := false
			for _, fd := range fds {
				if fd.Target == path && fd.Flags&syscall.O_ACCMODE == syscall.O_WRONLY {
					found = true
				}
			}
			assert.True(t, found, "the worker's writable FD must be included despite its private table")
			probe, _, _ := testProbe(t)
			probe.workspace = workspace
			probe.processes = proc
			require.ErrorIs(t, probe.verifyNoWritableWorkspaceFD([]processIdentity{identity}), ErrWritableWorkspaceFD)
		})
	}
}
