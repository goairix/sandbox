//go:build linux

package launcher

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// nativeProcess always registers cancellation and starts its Wait goroutine before
// returning a live process to any caller that can Fatal or return an error.
type nativeProcess struct {
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	input   io.WriteCloser
	output  nativeOutput
	done    chan struct{}
	waitErr error // published by closing done
}

type nativeOutput struct {
	mu      sync.Mutex
	all     bytes.Buffer
	pending string
	phases  chan string
}

func (w *nativeOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.all.Write(p)
	w.pending += string(p)
	for {
		line, rest, ok := strings.Cut(w.pending, "\n")
		if !ok {
			break
		}
		w.pending = rest
		if strings.HasPrefix(line, "phase ") {
			select {
			case w.phases <- line:
			default:
				return 0, fmt.Errorf("too many native phases")
			}
		}
	}
	return len(p), nil
}
func (w *nativeOutput) String() string { w.mu.Lock(); defer w.mu.Unlock(); return w.all.String() }
func startNativeProcess(mode string, configure func(*exec.Cmd)) (*nativeProcess, error) {
	binary, err := os.Executable()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &nativeProcess{cancel: cancel, done: make(chan struct{}), output: nativeOutput{phases: make(chan string, 16)}}
	p.cmd = exec.CommandContext(ctx, binary)
	p.cmd.Env = []string{nativeModeEnv + "=" + mode, "GOMAXPROCS=2"}
	p.cmd.Dir = "/"
	p.cmd.WaitDelay = time.Second
	p.cmd.Stdout = &p.output
	p.cmd.Stderr = &p.output
	if configure != nil {
		configure(p.cmd)
	}
	p.input, err = p.cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if mode == "user" {
		if err = observeNativeCloseOnExec(); err != nil {
			p.input.Close()
			cancel()
			return nil, err
		}
	}
	if err = p.cmd.Start(); err != nil {
		p.input.Close()
		cancel()
		return nil, err
	}
	go func() { p.waitErr = p.cmd.Wait(); close(p.done) }()
	return p, nil
}
func (p *nativeProcess) phase(want string) error {
	select {
	case got := <-p.output.phases:
		if got != want {
			return fmt.Errorf("phase got %q want %q", got, want)
		}
		return nil
	case <-p.done:
		select {
		case got := <-p.output.phases:
			if got == want {
				return nil
			}
		default:
		}
		return fmt.Errorf("child exited before %s: %v\n%s", want, p.waitResult(), p.output.String())
	case <-time.After(5 * time.Second):
		return fmt.Errorf("child timed out before %s", want)
	}
}
func (p *nativeProcess) release() error { _, err := io.WriteString(p.input, "continue\n"); return err }
func (p *nativeProcess) wait() error {
	select {
	case <-p.done:
		return p.waitErr
	case <-time.After(5 * time.Second):
		return fmt.Errorf("child %d wait timed out", p.cmd.Process.Pid)
	}
}
func (p *nativeProcess) waitResult() string {
	select {
	case <-p.done:
		return fmt.Sprint(p.waitErr)
	default:
		return "not joined"
	}
}
func (p *nativeProcess) stop() error {
	p.input.Close()
	p.cancel()
	select {
	case <-p.done:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("child %d cleanup did not wait", p.cmd.Process.Pid)
	}
}
func nativeContinue() error {
	var b [1]byte
	for {
		n, err := os.Stdin.Read(b[:])
		if err != nil {
			return err
		}
		if n == 1 && b[0] == '\n' {
			return nil
		}
	}
}

// Dropping Credential or clearing fewer than all five capability sets must fail
// against live proc evidence collected by the trusted monitor, not user claims.
func TestKernelUserIsolation(t *testing.T) {
	b := requireNativeBoundary(t)
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	p, err := startNativeProcess("monitor-user", func(cmd *exec.Cmd) {
		cmd.Env = append(cmd.Env, "LAUNCHER_TEST_MANAGEMENT_SECRET="+hex.EncodeToString(nonce))
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.stop(); err != nil {
			t.Error(err)
		}
		t.Logf("monitor PID=%d actual wait=%v output:\n%s", p.cmd.Process.Pid, p.waitResult(), p.output.String())
	})
	for _, stage := range []struct {
		phase       string
		inheritable uint64
	}{{"phase monitor initial", 0xe0}, {"phase monitor prepared", 0}} {
		if err = p.phase(stage.phase); err != nil {
			t.Fatal(err)
		}
		if err = observeNativeRoot(p.cmd.Process.Pid, stage.inheritable); err != nil {
			t.Fatal(err)
		}
		if err = p.release(); err != nil {
			t.Fatal(err)
		}
	}
	if err = p.wait(); err != nil {
		t.Fatalf("user isolation actual monitor exit: %v", err)
	}
	if !strings.Contains(p.output.String(), "kernel boundary verified: user isolation") {
		t.Fatal("missing completed user isolation evidence")
	}
	if err = b.ValidateCurrent(); err != nil {
		t.Fatal(err)
	}
}
func observeNativeRoot(pid int, inheritable uint64) error {
	_, err := walkKernelThreads(pid, func(tid int, s KernelSnapshot) error {
		fmt.Printf("parent observed root pid=%d tid=%d UID=%v GID=%v P/E/I/A/B=%x/%x/%x/%x/%x NNP=%t\n", pid, tid, s.UIDs, s.GIDs, s.Permitted, s.Effective, s.Inheritable, s.Ambient, s.Bounding, s.NoNewPrivileges)
		if s.UIDs != [4]uint32{} || s.GIDs != [4]uint32{} || s.Permitted != 0xe0 || s.Effective != 0xe0 || s.Inheritable != inheritable || s.Ambient != 0 || s.Bounding != 0 || !s.NoNewPrivileges {
			return fmt.Errorf("root exec thread %d violates role: %+v", tid, s)
		}
		return nil
	})
	return err
}
func nativeUserMonitor() (resultErr error) {
	initial, err := inspectKernel(roleMonitor, true)
	if err != nil {
		return err
	}
	fmt.Printf("user monitor initial %+v\nphase monitor initial\n", initial)
	if err = nativeContinue(); err != nil {
		return err
	}
	b, err := PrepareMonitor()
	if err != nil {
		return err
	}
	fmt.Printf("user monitor prepared %+v\nphase monitor prepared\n", b.Snapshot())
	if err = nativeContinue(); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "launcher-management-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	secret := dir + "/secret"
	if err = os.WriteFile(secret, []byte(os.Getenv("LAUNCHER_TEST_MANAGEMENT_SECRET")), 0600); err != nil {
		return err
	}
	management, err := os.Open(secret)
	if err != nil {
		return err
	}
	defer management.Close()
	p, err := startNativeProcess("user", func(cmd *exec.Cmd) {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{}}, AmbientCaps: []uintptr{}}
		cmd.Args = append(cmd.Args, strconv.Itoa(os.Getpid()), secret, strconv.Itoa(int(management.Fd())))
	})
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, p.stop())
		fmt.Printf("user PID=%d actual wait=%v output:\n%s", p.cmd.Process.Pid, p.waitResult(), p.output.String())
	}()
	if err = p.phase("phase user ready"); err != nil {
		return err
	}
	count, err := walkKernelThreads(p.cmd.Process.Pid, func(tid int, s KernelSnapshot) error {
		fmt.Printf("parent observed user pid=%d tid=%d UID=%v GID=%v P/E/I/A/B=%x/%x/%x/%x/%x NNP=%t\n", s.PID, tid, s.UIDs, s.GIDs, s.Permitted, s.Effective, s.Inheritable, s.Ambient, s.Bounding, s.NoNewPrivileges)
		if s.UIDs != [4]uint32{1000, 1000, 1000, 1000} || s.GIDs != [4]uint32{1000, 1000, 1000, 1000} || s.Permitted != 0 || s.Effective != 0 || s.Inheritable != 0 || s.Ambient != 0 || s.Bounding != 0 || !s.NoNewPrivileges {
			return fmt.Errorf("user thread %d violates UID/GID=1000, zero capabilities, NNP: %+v", tid, s)
		}
		data, err := readBounded(fmt.Sprintf("/proc/%d/task/%d/status", s.PID, tid), maxStatusBytes)
		if err != nil {
			return err
		}
		seen := 0
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "Groups:") {
				seen++
				if strings.TrimSpace(strings.TrimPrefix(line, "Groups:")) != "" {
					return fmt.Errorf("user thread %d retained groups: %s", tid, line)
				}
			}
		}
		if seen != 1 {
			return fmt.Errorf("user thread %d Groups missing or duplicated", tid)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if count < 4 {
		return fmt.Errorf("user extra threads not observed: %d", count)
	}
	if err = auditNativeUser(b, p.cmd.Process.Pid); err != nil {
		return err
	}
	if err = p.release(); err != nil {
		return err
	}
	if err = p.wait(); err != nil {
		return fmt.Errorf("user actual wait: %w", err)
	}
	if !strings.Contains(p.output.String(), "user hostile attempts denied") {
		return fmt.Errorf("missing hostile syscall outcomes")
	}
	if err = b.ValidateCurrent(); err != nil {
		return err
	}
	fmt.Println("kernel boundary verified: user isolation")
	return nil
}

// fs credentials are per-thread. Run this test-only audit on a pinned goroutine;
// never return a thread with unverified restored credentials to the runtime pool.
func auditNativeUser(b *KernelBoundary, pid int) (result error) {
	runtime.LockOSThread()
	defer func() {
		syscall.RawSyscall(unix.SYS_SETFSUID, 0, 0, 0)
		syscall.RawSyscall(unix.SYS_SETFSGID, 0, 0, 0)
		restoreErr := checkNativeFSIDs(0)
		if restoreErr == nil {
			restoreErr = b.ValidateCurrent()
		}
		result = errors.Join(result, restoreErr)
		if restoreErr == nil {
			runtime.UnlockOSThread()
		}
		// On failure remain pinned; nativeUserMonitor returns immediately
		// and TestMain exits the helper, never pooling a dirty thread.
	}()
	syscall.RawSyscall(unix.SYS_SETFSGID, 1000, 0, 0)
	syscall.RawSyscall(unix.SYS_SETFSUID, 1000, 0, 0)
	if result = checkNativeFSIDs(1000); result != nil {
		return
	}
	data, err := readBounded(fmt.Sprintf("/proc/%d/environ", pid), 4096)
	if err != nil {
		result = err
		return
	}
	if string(data) != nativeModeEnv+"=user\x00GOMAXPROCS=2\x00" {
		result = fmt.Errorf("user inherited unexpected environment %q", data)
		return
	}
	fmt.Printf("parent observed user environment exact=%q\n", data)
	cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if err != nil || cwd != "/" {
		result = fmt.Errorf("user cwd=%q err=%v", cwd, err)
		return
	}
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		result = err
		return
	}
	stdio := map[string]string{}
	for _, entry := range entries {
		target, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, entry.Name()))
		if err != nil {
			result = err
			return
		}
		fmt.Printf("parent observed user fd=%s target=%s\n", entry.Name(), target)
		switch entry.Name() {
		case "0", "1", "2":
			if !strings.HasPrefix(target, "pipe:[") {
				result = fmt.Errorf("unexpected stdio target %q", target)
				return
			}
			stdio[entry.Name()] = target
		default:
			if target == "/sys/fs/cgroup/cpu.max" {
				// Go 1.25 opens this exact cgroup-v2 file anew at startup,
				// before reading GOMAXPROCS; parent FDs were CLOEXEC.
				if result = observeNativeCPUFD(pid, entry.Name()); result != nil {
					return
				}
				continue
			}
			if target != "anon_inode:[eventpoll]" && target != "anon_inode:[eventfd]" {
				result = fmt.Errorf("non-stdio management/other FD inherited: %s=%s", entry.Name(), target)
				return
			}
		}
	}
	if len(stdio) != 3 || stdio["1"] != stdio["2"] || stdio["0"] == stdio["1"] {
		result = fmt.Errorf("unexpected stdio wiring: %v", stdio)
	}
	return result
}
func checkNativeFSIDs(want uint32) error {
	tid := unix.Gettid()
	data, err := readBounded(fmt.Sprintf("/proc/self/task/%d/status", tid), maxStatusBytes)
	if err != nil {
		return err
	}
	s, err := parseThreadStatus(data, os.Getpid(), tid)
	if err != nil {
		return err
	}
	fmt.Printf("parent audit tid=%d observed UID=%v GID=%v\n", tid, s.UIDs, s.GIDs)
	if s.UIDs != [4]uint32{0, 0, 0, want} || s.GIDs != [4]uint32{0, 0, 0, want} {
		return fmt.Errorf("fs credential transition not applied: UID=%v GID=%v wantFS=%d", s.UIDs, s.GIDs, want)
	}
	return nil
}
func nativeUser() (resultErr error) {
	stop, err := holdNativeThreads(3, nil)
	defer func() { resultErr = errors.Join(resultErr, stop()) }()
	if err != nil {
		return err
	}
	fmt.Println("phase user ready")
	if err = nativeContinue(); err != nil {
		return err
	}
	if len(os.Args) != 4 {
		return fmt.Errorf("invalid fixed user arguments")
	}
	parent, err := strconv.Atoi(os.Args[1])
	if err != nil {
		return err
	}
	denied := func(name string, err error) error {
		fmt.Printf("hostile %s actual error=%v\n", name, err)
		if !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.EACCES) {
			return fmt.Errorf("hostile %s not denied by permissions: %v", name, err)
		}
		return nil
	}
	if err = denied("setuid0", unix.Setuid(0)); err != nil {
		return err
	}
	h := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	d := [2]unix.CapUserData{{Permitted: 1 << 5, Effective: 1 << 5}}
	if err = denied("capraise", unix.Capset(&h, &d[0])); err != nil {
		return err
	}
	err = unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 0, 0, 0, 0)
	fmt.Printf("hostile clearNNP actual error=%v\n", err)
	if !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.EPERM) {
		return fmt.Errorf("NNP disable unexpectedly returned %v", err)
	}
	for _, pid := range []int{1, parent} {
		if err = denied(fmt.Sprintf("ptrace%d", pid), unix.PtraceAttach(pid)); err != nil {
			return err
		}
		_, err = os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err = denied(fmt.Sprintf("environ%d", pid), err); err != nil {
			return err
		}
		_, err = os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
		if err = denied(fmt.Sprintf("fd-directory%d", pid), err); err != nil {
			return err
		}
	}
	_, err = os.ReadFile(os.Args[2])
	if err = denied("root0700/0600secret", err); err != nil {
		return err
	}
	fd, err := strconv.Atoi(os.Args[3])
	if err != nil {
		return err
	}
	_, err = os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", parent, fd))
	if err = denied("managementFD", err); err != nil {
		return err
	}
	target, linkErr := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	fmt.Printf("hostile inherited management fd=%d actual target=%q error=%v\n", fd, target, linkErr)
	if linkErr == nil && target != "anon_inode:[eventpoll]" && target != "anon_inode:[eventfd]" {
		return fmt.Errorf("management descriptor inherited: %q", target)
	}
	if linkErr != nil && !errors.Is(linkErr, os.ErrNotExist) {
		return linkErr
	}
	fmt.Println("user hostile attempts denied")
	return nil
}

// All pre-existing non-stdio descriptors must be close-on-exec. This includes
// the real management secret, runtime cgroup descriptor and IPC pipe endpoints.
// os/exec creates only its explicit stdio wiring; ExtraFiles is never populated.
func observeNativeCloseOnExec() error {
	dir, err := os.Open("/proc/self/fd")
	if err != nil {
		return err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(128)
	if err != nil && err != io.EOF {
		return err
	}
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil {
			return err
		}
		if fd <= 2 {
			continue
		}
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil {
			return err
		}
		target, err := os.Readlink("/proc/self/fd/" + name)
		if err != nil {
			return err
		}
		fmt.Printf("parent pre-exec fd=%d target=%s FD_CLOEXEC=%t\n", fd, target, flags&unix.FD_CLOEXEC != 0)
		if flags&unix.FD_CLOEXEC == 0 {
			return fmt.Errorf("parent non-stdio fd%d not CLOEXEC", fd)
		}
	}
	if len(names) == 128 {
		return fmt.Errorf("parent FD enumeration unexpectedly full")
	}
	return nil
}
func observeNativeCPUFD(pid int, fd string) error {
	membership, err := readBounded(fmt.Sprintf("/proc/%d/cgroup", pid), 4096)
	if err != nil {
		return err
	}
	if string(membership) != "0::/\n" {
		return fmt.Errorf("unexpected cgroup-v2 membership %q", membership)
	}
	data, err := readBounded(fmt.Sprintf("/proc/%d/fdinfo/%s", pid, fd), 4096)
	if err != nil {
		return err
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "flags:") {
			flags, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "flags:")), 8, 64)
			if err != nil {
				return err
			}
			count++
			if flags&unix.O_CLOEXEC == 0 || flags&unix.O_ACCMODE != unix.O_RDONLY {
				return fmt.Errorf("runtime cgroup fd flags invalid: %o", flags)
			}
			fmt.Printf("parent observed runtime-owned cpu.max fd=%s flags=%o cgroup=%q\n", fd, flags, membership)
		}
	}
	if count != 1 {
		return fmt.Errorf("missing/duplicate cgroup fd flags")
	}
	return nil
}
