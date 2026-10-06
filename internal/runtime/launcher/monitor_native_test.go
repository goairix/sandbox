//go:build linux && (amd64 || arm64)

package launcher

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Fixed modes exist only in the test binary. These are never a product launcher.
func nativeConfinementMode(mode string) (bool, error) {
	switch mode {
	case "confinement-monitor":
		return true, nativeConfinedMonitor()
	case "confinement-user":
		return true, nativeConfinedUser()
	case "confinement-leaf":
		if os.Getuid() != 1000 || os.Getgid() != 1000 {
			return true, fmt.Errorf("ordinary exec lost user identity")
		}
		fmt.Println("confinement ordinary user exec succeeded")
		return true, nil
	case "confinement-hold":
		fmt.Println("phase confinement child ready")
		return true, nativeContinue()
	case "confinement-child-live", "confinement-child-zombie":
		return true, nativeConfinementExistingChild(mode)
	case "confinement-invalid-uid", "confinement-invalid-gid", "confinement-large-uid", "confinement-large-gid", "confinement-nonuniform", "confinement-setter":
		return true, nativeConfinementReject(mode)
	default:
		return false, fmt.Errorf("unknown fixed confinement mode")
	}
}

func nativeSeccompObservations(pid int) (map[int]seccompState, error) {
	states := map[int]seccompState{}
	_, err := walkKernelThreads(pid, func(tid int, _ KernelSnapshot) error {
		data, err := readBounded(fmt.Sprintf("/proc/%d/task/%d/status", pid, tid), maxStatusBytes)
		if err != nil {
			return err
		}
		s, err := parseSeccompStatus(data)
		if err != nil {
			return err
		}
		states[tid] = s
		fmt.Printf("observed confinement pid=%d tid=%d Seccomp=%d Seccomp_filters=%d\n", pid, tid, s.mode, s.filters)
		return nil
	})
	return states, err
}

func TestMonitorNativeConfinement(t *testing.T) {
	parent := requireNativeBoundary(t)
	if m, err := ConfineMonitor(parent, 1000, 1000); m != nil || !errors.Is(err, ErrUnsafeKernel) {
		t.Fatalf("PID1 role accepted: %v %v", m, err)
	}
	p, err := startNativeProcess("confinement-monitor", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.stop(); err != nil {
			t.Error(err)
		}
		t.Logf("confinement monitor pid=%d joined actual wait=%s output:\n%s", p.cmd.Process.Pid, p.waitResult(), p.output.String())
	})
	var before, confined map[int]seccompState
	for _, phase := range []string{"prepared", "installed", "newthreads"} {
		if err = p.phase("phase confinement " + phase); err != nil {
			t.Fatal(err)
		}
		if err = observeNativeRoot(p.cmd.Process.Pid, 0); err != nil {
			t.Fatal(err)
		}
		states, err := nativeSeccompObservations(p.cmd.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if phase == "prepared" {
			before = states
		} else {
			var count uint32
			for _, s := range before {
				count = s.filters
				break
			}
			for tid, s := range states {
				if s.mode != 2 || s.filters != count+1 {
					t.Fatalf("%s tid=%d actual=%+v baseline=%d", phase, tid, s, count)
				}
			}
			if phase == "installed" {
				confined = states
			} else {
				added := 0
				for tid := range states {
					if _, ok := confined[tid]; !ok {
						added++
					}
				}
				t.Logf("parent observed newly created confined threads=%d", added)
				if added < 2 {
					t.Fatalf("no actual new-thread inheritance evidence: %d", added)
				}
			}
		}
		if err = p.release(); err != nil {
			t.Fatal(err)
		}
	}
	if err = p.wait(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.output.String(), "monitor confinement verified: inherited policy and user exec") {
		t.Fatal("missing completion")
	}
	if err = parent.ValidateCurrent(); err != nil {
		t.Fatal(err)
	}
}

func nativeConfinedMonitor() (result error) {
	k, err := PrepareMonitor()
	if err != nil {
		return err
	}
	stop, err := holdNativeThreads(3, nil)
	defer func() { result = errors.Join(result, stop()) }()
	if err != nil {
		return err
	}
	for _, bad := range []*KernelBoundary{nil, {}, {role: rolePID1, pid: os.Getpid()}} {
		if m, e := ConfineMonitor(bad, 1000, 1000); m != nil || e == nil {
			return fmt.Errorf("invalid confinement kernel accepted")
		}
	}
	fmt.Println("phase confinement prepared")
	if err = nativeContinue(); err != nil {
		return err
	}
	m, err := ConfineMonitor(k, 1000, 1000)
	if err != nil {
		return err
	}
	fmt.Printf("monitor sealed pid=%d uid=%d gid=%d filters=%d cgroup=%q\nphase confinement installed\n", m.pid, m.uid, m.gid, m.installedFilters, m.cgroup)
	if err = nativeContinue(); err != nil {
		return err
	}
	later, err := holdNativeThreads(8, nil)
	defer func() { result = errors.Join(result, later()) }()
	if err != nil {
		return err
	}
	if err = m.ValidateCurrent(); err != nil {
		return err
	}
	fmt.Println("phase confinement newthreads")
	if err = nativeContinue(); err != nil {
		return err
	}
	if again, e := ConfineMonitor(k, 1000, 1000); again != nil || !errors.Is(e, ErrUnsafeKernel) {
		return fmt.Errorf("repeat accepted: %v", e)
	}
	// Deliberate exported-struct copy: reflection avoids a misleading vet copylock
	// diagnostic in this test of the forbidden operation. No method owns mu here.
	copied := new(MonitorBoundary)
	reflect.ValueOf(copied).Elem().Set(reflect.ValueOf(m).Elem())
	if err = copied.ValidateCurrent(); !errors.Is(err, ErrUnsafeKernel) {
		return fmt.Errorf("same-PID copy accepted: %v", err)
	}
	m.mu.Lock()
	err = m.ValidateCurrent()
	m.mu.Unlock()
	if !errors.Is(err, ErrKernelUnavailable) {
		return fmt.Errorf("concurrent owner accepted: %v", err)
	}
	if err = nativeConfinementDeniedCalls(); err != nil {
		return err
	}
	if err = nativeCgroupWriteDenied(); err != nil {
		return err
	}
	// A trusted additional layer is permitted. ValidateCurrent must inspect it and
	// accept the stronger count, never require equality or reinstall our layer.
	if err = nativeConfinementFilter(false, true); err != nil {
		return err
	}
	if err = m.ValidateCurrent(); err != nil {
		return err
	}
	count, err := inspectMonitorSeccomp(os.Getpid(), m.installedFilters+1, true)
	if err != nil {
		return err
	}
	p, err := startNativeProcess("confinement-user", func(cmd *exec.Cmd) {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{}}}
	})
	if err != nil {
		return err
	}
	defer func() {
		result = errors.Join(result, p.stop())
		fmt.Printf("confined user pid=%d joined wait=%s output:\n%s", p.cmd.Process.Pid, p.waitResult(), p.output.String())
	}()
	if err = p.phase("phase confined user ready"); err != nil {
		return err
	}
	if _, err = walkKernelThreads(p.cmd.Process.Pid, func(tid int, s KernelSnapshot) error {
		fmt.Printf("monitor observed user tid=%d UID=%v GID=%v P/E/I/A/B=%x/%x/%x/%x/%x NNP=%t\n", tid, s.UIDs, s.GIDs, s.Permitted, s.Effective, s.Inheritable, s.Ambient, s.Bounding, s.NoNewPrivileges)
		if s.UIDs != [4]uint32{1000, 1000, 1000, 1000} || s.GIDs != [4]uint32{1000, 1000, 1000, 1000} || s.Permitted|s.Effective|s.Inheritable|s.Ambient|s.Bounding != 0 || !s.NoNewPrivileges {
			return fmt.Errorf("user credential boundary mismatch")
		}
		return nil
	}); err != nil {
		return err
	}
	if _, err = inspectMonitorSeccomp(p.cmd.Process.Pid, count, true); err != nil {
		return err
	}
	if _, err = nativeSeccompObservations(p.cmd.Process.Pid); err != nil {
		return err
	}
	membership, err := readBounded(fmt.Sprintf("/proc/%d/cgroup", p.cmd.Process.Pid), maxCgroupBytes)
	if err != nil {
		return err
	}
	if !bytes.Equal(membership, m.cgroup) {
		return fmt.Errorf("user cgroup changed: %q", membership)
	}
	if err = p.release(); err != nil {
		return err
	}
	if err = p.wait(); err != nil {
		return err
	}
	if err = m.ValidateCurrent(); err != nil {
		return err
	}
	fmt.Println("monitor confinement verified: inherited policy and user exec")
	return nil
}

func nativeConfinedUser() (result error) {
	stop, err := holdNativeThreads(3, nil)
	defer func() { result = errors.Join(result, stop()) }()
	if err != nil {
		return err
	}
	fmt.Println("phase confined user ready")
	if err = nativeContinue(); err != nil {
		return err
	}
	if err = nativeConfinementDeniedCalls(); err != nil {
		return err
	}
	if err = nativeCgroupWriteDenied(); err != nil {
		return err
	}
	output, err := runNativeChild("confinement-leaf")
	fmt.Printf("ordinary user fork/exec wait=%v output=%s", err, output)
	if err != nil {
		return err
	}
	if !bytes.Contains(output, []byte("confinement ordinary user exec succeeded")) {
		return fmt.Errorf("leaf missing")
	}
	return nil
}

func nativeConfinementDeniedCalls() error {
	check := func(name string, nr uintptr, a0 uintptr, want syscall.Errno) error {
		_, _, got := syscall.Syscall6(nr, a0, 0, 0, 0, 0, 0)
		fmt.Printf("confinement syscall=%s nr=%d arg0=%x errno=%d (%s) want=%d\n", name, nr, a0, got, got, want)
		if got != want {
			return fmt.Errorf("%s errno=%v want=%v", name, got, want)
		}
		return nil
	}
	for _, call := range []struct {
		name    string
		nr, arg uintptr
	}{{"unshare", unix.SYS_UNSHARE, 0}, {"setns", unix.SYS_SETNS, ^uintptr(0)}, {"ptrace", unix.SYS_PTRACE, unix.PTRACE_PEEKDATA}, {"process_vm_readv", unix.SYS_PROCESS_VM_READV, uintptr(os.Getpid())}, {"process_vm_writev", unix.SYS_PROCESS_VM_WRITEV, uintptr(os.Getpid())}} {
		if err := check(call.name, call.nr, call.arg, syscall.EACCES); err != nil {
			return err
		}
	}
	// Every probe is invalid even without seccomp: CLONE_THREAD requires
	// CLONE_SIGHAND (and CLONE_VM). No raw-fork child can continue into Go.
	// https://man7.org/linux/man-pages/man2/clone.2.html (EINVAL).
	for _, flag := range []uintptr{unix.CLONE_NEWNS, unix.CLONE_NEWCGROUP, unix.CLONE_NEWUTS, unix.CLONE_NEWIPC, unix.CLONE_NEWUSER, unix.CLONE_NEWPID, unix.CLONE_NEWNET, unix.CLONE_NEWTIME} {
		if err := check("clone namespace", unix.SYS_CLONE, flag|unix.CLONE_THREAD, syscall.EACCES); err != nil {
			return err
		}
	}
	return check("clone3", unix.SYS_CLONE3, 0, syscall.ENOSYS)
}

func nativeCgroupWriteDenied() error {
	const path = "/sys/fs/cgroup/cgroup.procs"
	if _, err := os.Stat(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if f != nil {
		f.Close()
	}
	fmt.Printf("confinement cgroup write-open uid=%d path=%s error=%v\n", os.Getuid(), path, err)
	if !errors.Is(err, unix.EROFS) && !errors.Is(err, unix.EACCES) && !errors.Is(err, unix.EPERM) {
		return fmt.Errorf("cgroup write not denied: %v", err)
	}
	return nil
}

func TestMonitorNativeRejections(t *testing.T) {
	requireNativeBoundary(t)
	for _, mode := range []string{"confinement-child-live", "confinement-child-zombie", "confinement-invalid-uid", "confinement-invalid-gid", "confinement-large-uid", "confinement-large-gid", "confinement-nonuniform", "confinement-setter"} {
		t.Run(mode, func(t *testing.T) {
			output, err := runNativeChild(mode)
			t.Logf("%s wait=%v output:\n%s", mode, err, output)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(output, []byte("monitor confinement verified: "+mode+" rejected without boundary")) {
				t.Fatal("missing rejection evidence")
			}
		})
	}
}

func nativeConfinementUnchanged(before map[int]seccompState) error {
	after, err := nativeSeccompObservations(os.Getpid())
	if err != nil {
		return err
	}
	for tid, old := range before {
		if now, ok := after[tid]; ok && now != old {
			return fmt.Errorf("rejected confinement mutated tid=%d: %+v -> %+v", tid, old, now)
		}
	}
	return nil
}

func nativeConfinementExistingChild(mode string) (result error) {
	k, err := PrepareMonitor()
	if err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(binary)
	cmd.Env = []string{nativeModeEnv + "=confinement-hold", "GOMAXPROCS=2"}
	cmd.Dir = "/"
	cmd.WaitDelay = time.Second
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	output := nativeOutput{phases: make(chan string, 16)}
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err = cmd.Start(); err != nil {
		input.Close()
		return err
	}
	waited := false
	// This function is the sole wait owner. No Cmd.Wait goroutine competes with
	// ConfineMonitor's non-consuming waitid, including the zombie fixture.
	defer func() {
		input.Close()
		if !waited {
			_ = cmd.Process.Kill()
			err := cmd.Wait()
			fmt.Printf("existing child forced cleanup wait=%v\n", err)
		}
		fmt.Printf("existing child pid=%d joined=%t output=%s", cmd.Process.Pid, cmd.ProcessState != nil, output.String())
	}()
	select {
	case phase := <-output.phases:
		if phase != "phase confinement child ready" {
			return fmt.Errorf("bad phase %s", phase)
		}
	case <-time.After(5 * time.Second):
		return fmt.Errorf("child not ready")
	}
	if mode == "confinement-child-zombie" {
		if _, err = io.WriteString(input, "continue\n"); err != nil {
			return err
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			var info unix.Siginfo
			err = unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT|unix.WALL, nil)
			if err != nil {
				return err
			}
			if info.Signo != 0 {
				fmt.Printf("existing child waitable non-consuming signo=%d\n", info.Signo)
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("child did not become waitable")
			}
			time.Sleep(time.Millisecond)
		}
	}
	before, err := nativeSeccompObservations(os.Getpid())
	if err != nil {
		return err
	}
	m, err := ConfineMonitor(k, 1000, 1000)
	fmt.Printf("existing child mode=%s returned boundary=%t error=%v\n", mode, m != nil, err)
	if m != nil || !errors.Is(err, ErrUnsafeKernel) || !strings.Contains(err.Error(), "already has children") {
		return fmt.Errorf("existing child accepted/wrong cause: %v", err)
	}
	if err = nativeConfinementUnchanged(before); err != nil {
		return err
	}
	if mode == "confinement-child-live" {
		if _, err = io.WriteString(input, "continue\n"); err != nil {
			return err
		}
	}
	err = cmd.Wait()
	waited = true
	fmt.Printf("existing child sole-owner wait=%v raw=%v\n", err, cmd.ProcessState.Sys())
	if err != nil {
		return err
	}
	if again, e := ConfineMonitor(k, 1000, 1000); again != nil || !errors.Is(e, ErrUnsafeKernel) || !strings.Contains(e.Error(), "already attempted") {
		return fmt.Errorf("failed attempt was reusable: %v", e)
	}
	fmt.Printf("monitor confinement verified: %s rejected without boundary\n", mode)
	return nil
}

func nativeConfinementReject(mode string) (result error) {
	k, err := PrepareMonitor()
	if err != nil {
		return err
	}
	uid, gid := uint32(1000), uint32(1000)
	switch mode {
	case "confinement-invalid-uid":
		uid = 0
	case "confinement-invalid-gid":
		gid = 0
	case "confinement-large-uid":
		uid = 2147483648
	case "confinement-large-gid":
		gid = 2147483648
	case "confinement-nonuniform":
		stop, e := holdNativeThreads(1, func() error { return nativeConfinementFilter(false, false) })
		defer func() { result = errors.Join(result, stop()) }()
		if e != nil {
			return e
		}
	case "confinement-setter":
		if err = nativeConfinementFilter(true, true); err != nil {
			return err
		}
	}
	before, err := nativeSeccompObservations(os.Getpid())
	if err != nil {
		return err
	}
	m, err := ConfineMonitor(k, uid, gid)
	fmt.Printf("negative %s boundary=%t error=%v\n", mode, m != nil, err)
	want := ErrUnsafeKernel
	if mode == "confinement-setter" {
		want = ErrKernelUnavailable
	}
	if m != nil || !errors.Is(err, want) {
		return fmt.Errorf("unexpected reject result: %v", err)
	}
	if mode == "confinement-setter" && !errors.Is(err, unix.EPERM) {
		return fmt.Errorf("installer cause lost: %v", err)
	}
	if err = nativeConfinementUnchanged(before); err != nil {
		return err
	}
	if again, e := ConfineMonitor(k, 1000, 1000); again != nil || !errors.Is(e, ErrUnsafeKernel) || !strings.Contains(e.Error(), "already attempted") {
		return fmt.Errorf("failed attempt was reusable: %v", e)
	}
	fmt.Printf("monitor confinement verified: %s rejected without boundary\n", mode)
	return nil
}

// Real test-only installer: all-Go-thread prctl for uniform extra/failing
// layers, or a pinned worker's single-thread filter for a nonuniform baseline.
// Independently installed identical filters are distinct trees; later TSYNC
// cannot generally synchronize them (Linux seccomp(2), TSYNC section).
func nativeConfinementFilter(denySetter, all bool) error {
	p := []unix.SockFilter{{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW}}
	if denySetter {
		p = []unix.SockFilter{
			{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
			{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_PRCTL, Jf: 3},
			{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
			{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.PR_SET_SECCOMP, Jf: 1},
			{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
			{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		}
	}
	program := unix.SockFprog{Len: uint16(len(p)), Filter: &p[0]}
	var ret uintptr
	var errno syscall.Errno
	if all {
		ret, _, errno = syscall.AllThreadsSyscall6(unix.SYS_PRCTL, unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)), 0, 0, 0)
	} else {
		ret, _, errno = syscall.Syscall6(unix.SYS_PRCTL, unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)), 0, 0, 0)
	}
	runtime.KeepAlive(&program)
	runtime.KeepAlive(p)
	if errno != 0 {
		return errno
	}
	if ret != 0 {
		return fmt.Errorf("test filter unexpected return=%d", ret)
	}
	fmt.Printf("confinement fixture filter denySetter=%t all=%t tid=%d\n", denySetter, all, unix.Gettid())
	return nil
}
