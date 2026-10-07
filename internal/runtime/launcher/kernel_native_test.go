//go:build linux

package launcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const nativeModeEnv = "SANDBOX_LAUNCHER_TEST_MODE"

// TestMain modes belong only to this compiled test binary. There is no product
// raw-start API. A controller must run positive as namespace PID1 with CGO0.
func TestMain(m *testing.M) {
	mode := os.Getenv(nativeModeEnv)
	switch mode {
	case "", "positive", "cleanup-fatal", "cleanup-negative":
		os.Exit(m.Run())
	case "monitor-user":
		if err := nativeUserMonitor(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	case "cleanup-user":
		if err := nativeCleanupUser(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	case "user":
		if err := nativeUser(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	case "monitor":
		if err := nativeMonitor(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	case "drain-monitor", "drain-user", "drain-middle", "drain-leaf", "drain-sender":
		if err := nativeDrainMode(mode); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	case "confinement-monitor", "confinement-user", "confinement-leaf", "confinement-hold", "confinement-child-live", "confinement-child-zombie", "confinement-invalid-uid", "confinement-invalid-gid", "confinement-large-uid", "confinement-large-gid", "confinement-nonuniform", "confinement-setter":
		if _, err := nativeConfinementMode(mode); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	case "reject-nnp", "reject-missing-cap", "reject-extra-cap", "reject-role", "reject-nonpid1", "reject-thread", "reject-securebits", "reject-setter", "reject-partial":
		if err := nativeReject(mode); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	default:
		fmt.Fprintln(os.Stderr, "unknown fixed native test mode")
		os.Exit(2)
	}
}

func nativeObservations() (map[int]KernelSnapshot, error) {
	result := make(map[int]KernelSnapshot)
	_, err := walkKernelThreads(os.Getpid(), func(tid int, s KernelSnapshot) error {
		s.Threads = 0
		result[tid] = s
		fmt.Printf("native thread=%d state=%+v\n", tid, s)
		return nil
	})
	return result, err
}

func nativeMonitor() error {
	before, err := inspectKernel(roleMonitor, true)
	if err != nil {
		return fmt.Errorf("monitor exec initial: %w", err)
	}
	fmt.Printf("monitor exec initial %+v\n", before)
	b, err := PrepareMonitor()
	if err != nil {
		return err
	}
	if err := b.ValidateCurrent(); err != nil {
		return err
	}
	if err := validateKernelSnapshot(b.Snapshot(), 2, false); err != nil {
		return err
	}
	copy := b.Snapshot()
	copy.Permitted = 0
	if b.Snapshot().Permitted != 0xe0 {
		return fmt.Errorf("snapshot not a copy")
	}
	if other, err := PrepareMonitor(); other != nil || !errors.Is(err, ErrUnsafeKernel) {
		return fmt.Errorf("repeat monitor accepted: %v", err)
	}
	if other, err := BootstrapPID1(); other != nil || !errors.Is(err, ErrUnsafeKernel) {
		return fmt.Errorf("monitor changed role: %v", err)
	}
	fmt.Printf("monitor final %+v\nkernel boundary verified: monitor\n", b.Snapshot())
	return nil
}

// holdNativeThreads registers its complete stop/join operation before exposing
// workers to tests; locked workers exit without UnlockOSThread to retire their OS
// threads. Every caller defers stop before an error or Fatal can occur.
func holdNativeThreads(n int, change func() error) (func() error, error) {
	release := make(chan struct{})
	ready := make(chan error, n)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			runtime.LockOSThread()
			defer wg.Done()
			var err error
			if change != nil {
				err = change()
			}
			ready <- err
			<-release
		}()
	}
	go func() { wg.Wait(); close(done) }()
	var once sync.Once
	stop := func() error {
		once.Do(func() { close(release) })
		select {
		case <-done:
			return nil
		case <-time.After(5 * time.Second):
			return fmt.Errorf("native threads did not join")
		}
	}
	for i := 0; i < n; i++ {
		select {
		case err := <-ready:
			if err != nil {
				return stop, err
			}
		case <-time.After(5 * time.Second):
			return stop, fmt.Errorf("native thread not ready")
		}
	}
	return stop, nil
}

var nativeBootstrapOnce sync.Once
var nativeBoundary *KernelBoundary

func requireNativeBoundary(t *testing.T) *KernelBoundary {
	t.Helper()
	if os.Getenv(nativeModeEnv) != "positive" {
		t.Skip("requires controller-owned CGO0 native PID1 fixture")
	}
	nativeBootstrapOnce.Do(func() { initializeNativeBoundary(t) })
	if nativeBoundary == nil {
		t.Fatal("native bootstrap did not establish a boundary")
	}
	return nativeBoundary
}

func initializeNativeBoundary(t *testing.T) {
	if os.Getenv(nativeModeEnv) != "positive" {
		t.Skip("requires controller-owned CGO0 native PID1 fixture")
	}
	if os.Getpid() != 1 {
		t.Fatal("native positive must actually be PID1")
	}
	stop, err := holdNativeThreads(3, nil)
	defer func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	}()
	if err != nil {
		t.Fatal(err)
	}
	initial, err := inspectKernel(rolePID1, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("PID1 initial %+v", initial)
	b, err := BootstrapPID1()
	if err != nil || b == nil {
		t.Fatalf("bootstrap: boundary=%v error=%v", b, err)
	}
	if err := b.ValidateCurrent(); err != nil {
		t.Fatal(err)
	}
	t.Logf("PID1 final %+v", b.Snapshot())
	if bad, err := BootstrapPID1(); bad != nil || !errors.Is(err, ErrUnsafeKernel) {
		t.Fatalf("repeat bootstrap: %v %v", bad, err)
	}
	if bad, err := PrepareMonitor(); bad != nil || !errors.Is(err, ErrUnsafeKernel) {
		t.Fatalf("PID1 monitor role: %v %v", bad, err)
	}
	// Newly created threads must inherit the already reduced state as well.
	stopLater, err := holdNativeThreads(2, nil)
	defer func() {
		if err := stopLater(); err != nil {
			t.Error(err)
		}
	}()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ValidateCurrent(); err != nil {
		t.Fatal(err)
	}
	nativeBoundary = b
}

func TestKernelNative(t *testing.T) {
	requireNativeBoundary(t)
	for _, mode := range []string{"monitor", "reject-nonpid1"} {
		out, err := runNativeChild(mode)
		t.Logf("child %s: %s", mode, out)
		if err != nil {
			t.Fatalf("child %s exit: %v", mode, err)
		}
		if !bytes.Contains(out, []byte("kernel boundary verified")) {
			t.Fatalf("missing child marker: %s", out)
		}
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	status, err := readBounded("/proc/self/status", maxStatusBytes)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := os.Open("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	fds, err := fd.Readdirnames(128)
	fd.Close()
	if err != nil && len(fds) == 0 {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			t.Log(line)
		}
	}
	t.Logf("allocated=%d observedFDs=%d", memory.Alloc, len(fds))
	if !t.Failed() {
		t.Log("kernel boundary verified: PID1 and root monitor exec")
	}
}

func runNativeChild(mode string) ([]byte, error) {
	binary, err := os.Executable()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary)
	cmd.Env = []string{nativeModeEnv + "=" + mode, "GOMAXPROCS=2"}
	cmd.Dir = "/"
	cmd.WaitDelay = time.Second
	// No ExtraFiles, shell, credentials or ambient additions: this is the trusted
	// root monitor exec whose inheritance the actual kernel must establish.
	return cmd.CombinedOutput()
}

func nativeReject(mode string) (resultErr error) {
	if mode == "reject-nonpid1" {
		if os.Getpid() <= 1 {
			return fmt.Errorf("nonpid1 fixture incorrectly PID1")
		}
	} else if os.Getpid() != 1 {
		return fmt.Errorf("negative fixture must actually be PID1")
	}
	if mode == "reject-thread" || mode == "reject-securebits" {
		stop, err := holdNativeThreads(1, func() error {
			if mode == "reject-securebits" {
				err := unix.Prctl(unix.PR_SET_SECUREBITS, 1, 0, 0, 0)
				fmt.Printf("native divergent securebits thread=%d set=1 error=%v\n", unix.Gettid(), err)
				return err
			}
			h := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
			d := [2]unix.CapUserData{{Permitted: 0x1e0, Effective: 0x1c0}}
			return unix.Capset(&h, &d[0])
		})
		defer func() {
			if err := stop(); err != nil {
				resultErr = errors.Join(resultErr, err)
			}
		}()
		if err != nil {
			return err
		}
	}
	if mode == "reject-setter" || mode == "reject-partial" {
		initial, err := inspectKernel(rolePID1, true)
		if err != nil {
			return fmt.Errorf("setter failure fixture: %w", err)
		}
		fmt.Printf("setter failure fixture initial %+v\n", initial)
		if err := denyNativeSetter(mode); err != nil {
			return fmt.Errorf("seccomp fixture setup: %w", err)
		}
	}
	before, err := nativeObservations()
	if err != nil {
		return err
	}
	for _, s := range before {
		switch mode {
		case "reject-nnp":
			if s.NoNewPrivileges {
				return fmt.Errorf("negative nnp fixture actually NNP=1")
			}
		case "reject-missing-cap":
			if s.Permitted != 0xe0 || s.Effective != 0xe0 || s.Bounding != 0xe0 {
				return fmt.Errorf("missing-cap fixture expected e0: %+v", s)
			}
		case "reject-extra-cap":
			if s.Permitted != 0x1e1 || s.Effective != 0x1e1 || s.Bounding != 0x1e1 {
				return fmt.Errorf("extra-cap fixture expected 1e1: %+v", s)
			}
		}
	}
	var b *KernelBoundary
	if mode == "reject-role" {
		b, err = PrepareMonitor()
	} else {
		b, err = BootstrapPID1()
	}
	if mode == "reject-setter" || mode == "reject-partial" {
		if b != nil || !errors.Is(err, ErrKernelUnavailable) || !errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("setter failure %s returned boundary=%v err=%v", mode, b, err)
		}
	} else if b != nil || !errors.Is(err, ErrUnsafeKernel) {
		return fmt.Errorf("negative %s returned boundary=%v err=%v", mode, b, err)
	}
	fmt.Printf("negative %s rejected: %v\n", mode, err)
	after, err := nativeObservations()
	if err != nil {
		return err
	}
	// Runtime may add/retire threads. Every surviving observed thread must retain
	// exactly its original credentials, masks and NNP; new threads are also logged.
	for tid, s := range before {
		if mode == "reject-partial" {
			s.Inheritable = 0xe0
		}
		if now, ok := after[tid]; ok && !reflect.DeepEqual(s, now) {
			return fmt.Errorf("rejected call mutated thread %d: %+v -> %+v", tid, s, now)
		}
	}
	if mode == "reject-partial" {
		for tid, s := range after {
			if s.Permitted != 0x1e0 || s.Effective != 0x1e0 || s.Inheritable != 0xe0 || s.Bounding != 0x1e0 || s.Ambient != 0 {
				return fmt.Errorf("partial failure thread %d unexpected state %+v", tid, s)
			}
		}
		fmt.Println("kernel boundary verified: reject-partial rejected without boundary after real EPERM; inheritable=e0 retained without rollback")
	} else {
		fmt.Printf("kernel boundary verified: %s rejected without boundary or credential mutation\n", mode)
	}
	return nil
}

// denyNativeSetter installs a real seccomp filter across every actual thread.
// This test fixture makes the kernel reject a specific setter; it does not replace
// a syscall, a proc read, or a production function with a fake implementation.
func denyNativeSetter(mode string) error {
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0}, // seccomp_data.nr
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: uint32(unix.SYS_CAPSET), Jf: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
	if mode == "reject-partial" {
		filter = []unix.SockFilter{
			{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
			{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: uint32(unix.SYS_PRCTL), Jf: 3},
			{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16}, // seccomp_data.args[0], native little endian fixture
			{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.PR_CAPBSET_DROP, Jf: 1},
			{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
			{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		}
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	result, _, errno := syscall.Syscall6(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&program)), 0, 0, 0)
	runtime.KeepAlive(&program)
	runtime.KeepAlive(filter)
	if errno != 0 {
		return errno
	}
	if result != 0 {
		return fmt.Errorf("seccomp TSYNC failed at thread %d", result)
	}
	fmt.Printf("native seccomp TSYNC installed for %s\n", mode)
	return nil
}

func TestKernelProcParser(t *testing.T) {
	valid := "Pid:\t11\nTgid:\t1\nUid:\t0 0 0 0\nGid:\t0 0 0 0\nCapPrm:\t00000000000001e0\nCapEff:\t00000000000001e0\nCapInh:\t0000000000000000\nCapAmb:\t0000000000000000\nCapBnd:\t00000000000001e0\nNoNewPrivs:\t1\nThreads:\t4\n"
	s, err := parseThreadStatus([]byte(valid), 1, 11)
	if err != nil || s.PID != 1 || s.Permitted != 0x1e0 || s.Threads != 4 || !s.NoNewPrivileges {
		t.Fatalf("valid: %+v %v", s, err)
	}
	for _, name := range []string{"Pid", "Tgid", "Uid", "Gid", "CapPrm", "CapEff", "CapInh", "CapAmb", "CapBnd", "NoNewPrivs", "Threads"} {
		start := strings.Index(valid, name+":")
		end := start + strings.Index(valid[start:], "\n") + 1
		for _, data := range []string{valid[:start] + valid[end:], valid + valid[start:end], valid[:start] + name + ":\t-1\n" + valid[end:], valid[:start] + name + ":\t+1\n" + valid[end:], valid[:start] + name + ":\t18446744073709551616\n" + valid[end:]} {
			if _, err := parseThreadStatus([]byte(data), 1, 11); err == nil {
				t.Errorf("accepted malformed %s: %q", name, data)
			}
		}
	}
	for _, replacement := range []struct{ old, new string }{{"Threads:\t4", "Threads:\t0"}, {"Threads:\t4", "Threads:\t4097"}, {"NoNewPrivs:\t1", "NoNewPrivs:\t2"}, {"Pid:\t11", "Pid:\t12"}, {"Tgid:\t1", "Tgid:\t2"}, {"Uid:\t0 0 0 0", "Uid:\t0 0 0"}, {"CapPrm:\t00000000000001e0", "CapPrm:\t0x1e0"}} {
		if _, err := parseThreadStatus([]byte(strings.Replace(valid, replacement.old, replacement.new, 1)), 1, 11); err == nil {
			t.Errorf("accepted %s", replacement.new)
		}
	}
	if _, err := parseThreadStatus(bytes.Repeat([]byte("x"), maxStatusBytes+1), 1, 11); err == nil {
		t.Fatal("oversize accepted")
	}
	if err := setterError("test", syscall.EPERM); !errors.Is(err, ErrKernelUnavailable) || !errors.Is(err, syscall.EPERM) {
		t.Fatalf("cause lost: %v", err)
	}
	if err := setterError("test", syscall.ENOTSUP); !errors.Is(err, ErrUnsupported) || !errors.Is(err, syscall.ENOTSUP) {
		t.Fatalf("unsupported cause lost: %v", err)
	}
}
