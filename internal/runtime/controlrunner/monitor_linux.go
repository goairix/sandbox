//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Every path component must be protected from user replacement. The root-owned
// regular ELF is fixed at constructor time and rechecked before each monitor.
func inspectExecutable(path string) (os.FileInfo, error) {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return nil, fmt.Errorf("unsafe executable path %s", current)
		}
		if current == path {
			if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
				return nil, ErrInvalidConfiguration
			}
		} else if !info.IsDir() {
			return nil, ErrInvalidConfiguration
		}
		if current == "/" {
			break
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var magic [4]byte
	if _, err = f.ReadAt(magic[:], 0); err != nil {
		return nil, err
	}
	if magic != [4]byte{0x7f, 'E', 'L', 'F'} {
		return nil, fmt.Errorf("monitor executable must be ELF")
	}
	var stat unix.Statfs_t
	if err = unix.Fstatfs(int(f.Fd()), &stat); err != nil {
		return nil, err
	}
	// A protected read-only mount makes executable bytes immutable for this birth.
	if stat.Flags&unix.ST_RDONLY == 0 {
		return nil, fmt.Errorf("monitor executable filesystem must be read-only")
	}
	return f.Stat()
}

// RunMonitor is the fixed root-only binary entry. It accepts no payload argument
// and consumes only inherited protected request/control/result descriptors 3..5.
func RunMonitor() error {
	if os.Getppid() != 1 || os.Getpid() <= 1 || os.Geteuid() != 0 {
		return ErrUnavailable
	}
	started := time.Now()
	kernel, err := launcher.PrepareMonitor()
	if err != nil {
		return err
	}
	files := make([]*os.File, 3)
	for i := range files {
		fd := 3 + i
		unix.CloseOnExec(fd)
		if err = unix.SetNonblock(fd, true); err != nil {
			return err
		}
		files[i] = os.NewFile(uintptr(fd), "monitor-ipc")
		defer files[i].Close()
	}
	request, control, result := files[0], files[1], files[2]
	if err = request.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	start, err := readMonitorRequest(request)
	if err != nil {
		return err
	}
	var extra [1]byte
	if n, e := request.Read(extra[:]); n != 0 || e != io.EOF {
		return fmt.Errorf("monitor request trailing data or open sender")
	}
	if err = request.Close(); err != nil {
		return err
	}
	req := start.Request
	boundary, err := launcher.ConfineMonitor(kernel, req.UID, req.GID)
	if err != nil {
		return err
	}
	now, err := monitorMonotonic()
	if err != nil {
		return err
	}
	if start.AuthorityDeadlineNS <= now || start.AuthorityDeadlineNS-now > int64(30*time.Second) || start.CommandDeadlineNS <= now || start.CommandDeadlineNS-now > int64(3600*time.Second) {
		return fmt.Errorf("monitor deadline expired or outside bound")
	}
	life, err := launcher.NewRootLifecycle(start.AuthorityDeadlineNS)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(start.CommandDeadlineNS-now))
	defer cancel()
	path, err := resolveUserExecutable(req)
	if err != nil {
		return err
	}
	var stdin, stdout, stderr, input, output, errorOutput *os.File
	var eof byte
	if req.TTY {
		input, stdin, eof, err = openPTY()
		if err != nil {
			return err
		}
		stdout = stdin
		stderr = stdin
		output = input
	} else {
		stdin, input, err = os.Pipe()
		if err != nil {
			return err
		}
		output, stdout, err = os.Pipe()
		if err != nil {
			stdin.Close()
			input.Close()
			return err
		}
		errorOutput, stderr, err = os.Pipe()
		if err != nil {
			stdin.Close()
			input.Close()
			output.Close()
			stdout.Close()
			return err
		}
	}
	for _, f := range []*os.File{stdin, stdout, stderr, input, output, errorOutput} {
		if f != nil {
			defer f.Close()
		}
	}
	cmd := &exec.Cmd{Path: path, Args: append([]string(nil), req.Argv...), Dir: req.WorkDir, Env: requestEnvironment(req), Stdin: stdin, Stdout: stdout, Stderr: stderr, SysProcAttr: &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: req.UID, Gid: req.GID, Groups: []uint32{}}}}
	if req.TTY {
		cmd.SysProcAttr.Setsid = true
		cmd.SysProcAttr.Setctty = true
		cmd.SysProcAttr.Ctty = 0
	}
	if err = sealExecDescriptors(); err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("user spawn failed (no attributable root status): %w", err)
	}
	root := cmd.Process.Pid
	rootDone := make(chan struct{})
	var observation launcher.RootExitObservation
	var waitErr error
	go func() {
		defer close(rootDone)
		observation, waitErr = boundary.SuperviseRootWithLifecycle(ctx, root, life)
	}()
	// Never Cmd.Wait for user; Release closes Go's process handle only.
	if err = cmd.Process.Release(); err != nil {
		cancel()
		<-rootDone
		return errors.Join(err, waitErr)
	}
	stdin.Close()
	if !req.TTY {
		stdout.Close()
		stderr.Close()
	}
	writer := monitorWriter{file: result}
	work := make(chan error, 4)
	workers := 0
	launch := func(f func() error) { workers++; go func() { work <- f() }() }
	launch(func() error { return relayMonitorOutput(output, monitorStdout, req.TTY, &writer) })
	if errorOutput != nil {
		launch(func() error { return relayMonitorOutput(errorOutput, monitorStderr, false, &writer) })
	}
	launch(func() error {
		if err := input.SetWriteDeadline(time.Now().Add(time.Duration(start.CommandDeadlineNS - now))); err != nil {
			return err
		}
		if _, err := input.Write(req.Stdin); err != nil {
			return err
		}
		if req.TTY {
			_, err := input.Write([]byte{eof, eof})
			return err
		}
		return input.Close()
	})
	readinessCtx, readinessCancel := context.WithTimeout(ctx, time.Second)
	readinessErr := life.WaitRegistered(readinessCtx)
	readinessCancel()
	completed := false
	if readinessErr != nil {
		cancel()
		<-rootDone
		if waitErr != nil || !validCompletedRoot(observation, root, os.Getpid()) {
			err = errors.Join(readinessErr, waitErr)
		} else {
			completed = true
		}
	}
	ready := collectMonitorReady(root, time.Since(started))
	ready.Completed = completed
	data, marshalErr := json.Marshal(ready)
	if err == nil {
		err = marshalErr
	}
	if err == nil {
		err = writer.frame(monitorStarted, data)
	}
	controlDone := make(chan error, 1)
	if err == nil && !completed {
		go func() { controlDone <- runMonitorControl(ctx, cancel, control, life, &writer) }()
	} else {
		controlDone <- nil
	}
	if err != nil {
		cancel()
	}
	<-rootDone
	cancel()
	control.Close()
	if !req.TTY {
		input.Close()
	}
	select {
	case e := <-controlDone:
		err = errors.Join(err, e)
	case <-time.After(time.Second):
		err = errors.Join(err, fmt.Errorf("control owner did not join"))
	}
	joined := 0
	joinTimer := time.NewTimer(time.Second)
	for joined < workers {
		select {
		case e := <-work:
			joined++
			err = errors.Join(err, e)
		case <-joinTimer.C:
			err = errors.Join(err, fmt.Errorf("stream owner join deadline"))
			output.Close()
			if errorOutput != nil {
				errorOutput.Close()
			}
			input.Close()
			// Closing streams interrupts pollable IO; make a final bounded join attempt.
			final := time.NewTimer(time.Second)
			for joined < workers {
				select {
				case <-work:
					joined++
				case <-final.C:
					return err
				}
			}
			final.Stop()
		}
	}
	joinTimer.Stop()
	if err = errors.Join(err, waitErr); err != nil {
		return err
	}
	data, err = json.Marshal(completionFromRoot(observation))
	if err != nil {
		return err
	}
	return writer.frame(monitorResult, data)
}

type monitorWriter struct {
	mu   sync.Mutex
	file *os.File
}

func (w *monitorWriter) frame(kind byte, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.file.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	return writeMonitorFrame(w.file, kind, data)
}
func monitorMonotonic() (int64, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, err
	}
	return ts.Nano(), nil
}
func relayMonitorOutput(f *os.File, kind byte, tty bool, w *monitorWriter) error {
	b := make([]byte, 32768)
	for {
		n, err := f.Read(b)
		if n > 0 {
			if e := w.frame(kind, b[:n]); e != nil {
				return e
			}
		}
		if errors.Is(err, io.EOF) || (tty && errors.Is(err, unix.EIO)) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
func runMonitorControl(ctx context.Context, cancel context.CancelFunc, f *os.File, life *launcher.RootLifecycle, w *monitorWriter) error {
	for {
		kind, data, err := readMonitorFrame(f)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			cancel()
			return err
		}
		switch kind {
		case monitorCancel:
			cancel()
			return nil
		case monitorRenew:
			if len(data) != 16 {
				cancel()
				return fmt.Errorf("invalid renewal control")
			}
			deadline := int64(binary.BigEndian.Uint64(data[8:]))
			bounded, stop := context.WithTimeout(ctx, time.Second)
			err = life.Renew(bounded, deadline)
			stop()
			if err != nil {
				cancel()
				return err
			}
			if err = w.frame(monitorRenewAck, data); err != nil {
				cancel()
				return err
			}
		default:
			cancel()
			return fmt.Errorf("invalid monitor control")
		}
	}
}
func requestEnvironment(r controlprotocol.ExecutionRequest) []string {
	keys := make([]string, 0, len(r.Env))
	for k := range r.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, k+"="+r.Env[k])
	}
	return env
}
func resolveUserExecutable(r controlprotocol.ExecutionRequest) (string, error) {
	name := r.Argv[0]
	if strings.Contains(name, "/") {
		if !filepath.IsAbs(name) {
			name = filepath.Join(r.WorkDir, name)
		}
		return name, nil
	}
	value, present := r.Env["PATH"]
	if !present {
		return "", fmt.Errorf("bare argv0 requires signed PATH")
	}
	for _, dir := range strings.Split(value, ":") {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(r.WorkDir, dir)
		}
		path := filepath.Join(dir, name)
		info, e := os.Stat(path)
		if e == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return path, nil
		}
	}
	return "", fmt.Errorf("argv0 absent from signed PATH")
}
func collectMonitorReady(root int, elapsed time.Duration) monitorReady {
	r := monitorReady{RootPID: root, MonitorPID: os.Getpid(), FDCount: -1, ElapsedNS: elapsed.Nanoseconds()}
	if files, e := os.ReadDir("/proc/self/fd"); e == nil {
		r.FDCount = 0
		for _, file := range files {
			if len(r.FDs) >= 64 {
				break
			}
			fd, err := strconv.Atoi(file.Name())
			if err != nil {
				continue
			}
			target, err := os.Readlink("/proc/self/fd/" + file.Name())
			if err != nil {
				continue
			}
			flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
			if err != nil {
				continue
			}
			r.FDs = append(r.FDs, monitorFD{FD: fd, Target: target, Flags: flags})
			r.FDCount++
		}
	}
	read := func(path string) string {
		f, e := os.Open(path)
		if e != nil {
			return "unavailable: " + e.Error()
		}
		defer f.Close()
		b, e := io.ReadAll(io.LimitReader(f, 4096))
		if e != nil {
			return "unavailable: " + e.Error()
		}
		return string(b)
	}
	raw := read("/proc/self/status")
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "VmRSS:") || strings.HasPrefix(line, "VmHWM:") || strings.HasPrefix(line, "Threads:") {
			r.Status += line + "\n"
		}
	}
	r.Cgroup = read("/proc/self/cgroup")
	r.MemoryCurrent = read("/sys/fs/cgroup/memory.current")
	r.MemoryPeak = read("/sys/fs/cgroup/memory.peak")
	r.PidsCurrent = read("/sys/fs/cgroup/pids.current")
	return r
}

// Go ForkExec maps explicit descriptors but expects other inherited descriptors
// already CLOEXEC. Seal all current non-standard descriptors, preserving their
// availability in this process and explicit ExtraFiles mappings in the child.
// Kernel support is mandatory; no enumeration fallback can close the race.
func sealExecDescriptors() error {
	if err := unix.CloseRange(3, ^uint(0), unix.CLOSE_RANGE_CLOEXEC); err != nil {
		return fmt.Errorf("required close_range CLOEXEC boundary: %w", err)
	}
	return nil
}

func validCompletedRoot(o launcher.RootExitObservation, root, monitor int) bool {
	status := unix.WaitStatus(o.RootWaitStatus)
	return o.RootPID == root && root > 1 && o.Drain.MonitorPID == monitor && (status.Exited() || status.Signaled())
}

// A full or broken diagnostic pipe cannot defer namespace isolation. A short
// write is deliberately best effort; no lock, retry loop or unbounded writer.
func writeIsolationDiagnostic(wire []byte) {
	if unix.SetNonblock(2, true) == nil {
		_, _ = unix.Write(2, wire)
	}
}
