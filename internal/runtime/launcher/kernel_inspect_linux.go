//go:build linux

package launcher

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const maxStatusBytes = 64 << 10

func unavailable(op string, err error) error {
	return fmt.Errorf("%w: %s: %w", ErrKernelUnavailable, op, err)
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return b, nil
}

func parseUnsigned(s string, base, bits int) (uint64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty number")
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9') && !(base == 16 && ((c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'))) {
			return 0, fmt.Errorf("invalid unsigned number %q", s)
		}
	}
	return strconv.ParseUint(s, base, bits)
}

// parseThreadStatus rejects missing/duplicate fields and does not trust the status
// PID or thread count as the process identity or the observed enumeration count.
func parseThreadStatus(data []byte, pid, tid int) (KernelSnapshot, error) {
	var s KernelSnapshot
	if len(data) > maxStatusBytes {
		return s, fmt.Errorf("status too large")
	}
	seen := uint16(0)
	names := []string{"Pid", "Tgid", "Uid", "Gid", "CapPrm", "CapEff", "CapInh", "CapAmb", "CapBnd", "NoNewPrivs", "Threads"}
	scan := bufio.NewScanner(strings.NewReader(string(data)))
	scan.Buffer(make([]byte, 4096), maxStatusBytes+1)
	for scan.Scan() {
		key, value, ok := strings.Cut(scan.Text(), ":")
		if !ok {
			continue
		}
		idx := -1
		for i, n := range names {
			if key == n {
				idx = i
				break
			}
		}
		if idx < 0 {
			continue
		}
		bit := uint16(1) << uint(idx)
		if seen&bit != 0 {
			return s, fmt.Errorf("duplicate %s", key)
		}
		seen |= bit
		fields := strings.Fields(value)
		want := 1
		if idx == 2 || idx == 3 {
			want = 4
		}
		if len(fields) != want {
			return s, fmt.Errorf("invalid %s field count", key)
		}
		nums := [4]uint64{}
		for i, f := range fields {
			base, bits := 10, 32
			if idx >= 4 && idx <= 8 {
				base, bits = 16, 64
			}
			n, err := parseUnsigned(f, base, bits)
			if err != nil {
				return s, fmt.Errorf("%s: %w", key, err)
			}
			nums[i] = n
		}
		switch idx {
		case 0:
			if nums[0] == 0 || nums[0] > 0x7fffffff || int(nums[0]) != tid {
				return s, fmt.Errorf("invalid thread PID")
			}
		case 1:
			if nums[0] == 0 || nums[0] > 0x7fffffff || int(nums[0]) != pid {
				return s, fmt.Errorf("invalid thread Tgid")
			}
			s.PID = pid
		case 2:
			for i, n := range nums {
				s.UIDs[i] = uint32(n)
			}
		case 3:
			for i, n := range nums {
				s.GIDs[i] = uint32(n)
			}
		case 4:
			s.Permitted = nums[0]
		case 5:
			s.Effective = nums[0]
		case 6:
			s.Inheritable = nums[0]
		case 7:
			s.Ambient = nums[0]
		case 8:
			s.Bounding = nums[0]
		case 9:
			if nums[0] > 1 {
				return s, fmt.Errorf("invalid NoNewPrivs")
			}
			s.NoNewPrivileges = nums[0] == 1
		case 10:
			if nums[0] < 1 || nums[0] > 4096 {
				return s, fmt.Errorf("invalid Threads")
			}
			s.Threads = uint32(nums[0])
		}
	}
	if err := scan.Err(); err != nil {
		return s, err
	}
	if seen != (1<<len(names))-1 {
		return s, fmt.Errorf("missing status fields")
	}
	return s, nil
}

// walkKernelThreads retains one directory page and one status at a time. A vanished
// task invalidates the whole observation; the caller may retry from the beginning.
func walkKernelThreads(pid int, visit func(int, KernelSnapshot) error) (uint32, error) {
	dir, err := os.Open(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	var count uint32
	for {
		names, readErr := dir.Readdirnames(128)
		for _, name := range names {
			id, err := parseUnsigned(name, 10, 31)
			if err != nil || id == 0 {
				return 0, fmt.Errorf("invalid task directory name %q", name)
			}
			count++
			if count > 4096 {
				return 0, fmt.Errorf("too many threads")
			}
			b, err := readBounded(fmt.Sprintf("/proc/%d/task/%s/status", pid, name), maxStatusBytes)
			if err != nil {
				return 0, err
			}
			s, err := parseThreadStatus(b, pid, int(id))
			if err != nil {
				return 0, err
			}
			if err := visit(int(id), s); err != nil {
				return 0, err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return 0, readErr
		}
	}
	if count == 0 {
		return 0, fmt.Errorf("no observed threads")
	}
	return count, nil
}

func inspectKernel(role kernelRole, initial bool) (KernelSnapshot, error) {
	// Dumpability/subreaper are process properties. Securebits is checked on all
	// runtime threads by the narrowly scoped uniform-return GET below.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	for attempt := 0; attempt < 3; attempt++ {
		s, err := inspectKernelOnce(role, initial)
		if err == nil {
			return s, nil
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ESRCH) {
			return KernelSnapshot{}, err
		}
		if attempt == 2 {
			return KernelSnapshot{}, unavailable("unstable task enumeration after 3 attempts", err)
		}
	}
	panic("unreachable")
}

func inspectKernelOnce(role kernelRole, initial bool) (KernelSnapshot, error) {
	var result KernelSnapshot
	capData, err := readBounded("/proc/sys/kernel/cap_last_cap", 32)
	if err != nil {
		return result, unavailable("cap_last_cap", err)
	}
	last, err := parseUnsigned(strings.TrimSpace(string(capData)), 10, 32)
	if err != nil {
		return result, unavailable("cap_last_cap", err)
	}
	if last < 8 || last > 63 {
		return result, fmt.Errorf("%w: cap_last_cap=%d outside 8..63", ErrUnsafeKernel, last)
	}
	dump, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	if err != nil {
		return result, unavailable("GET_DUMPABLE", err)
	}
	// Unlike other GETs this returns only a scalar and the contract requires the
	// identical value zero on every thread. The Go runtime terminates the process
	// if thread results differ; it must never be replaced by a single-thread GET.
	secure, _, secureErr := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, unix.PR_GET_SECUREBITS, 0, 0, 0, 0, 0)
	if err := setterError("all-thread GET_SECUREBITS", secureErr); err != nil {
		return result, err
	}
	if secure > 0xffffffff {
		return result, fmt.Errorf("%w: securebits out of range", ErrUnsafeKernel)
	}
	var sub int32
	_, _, subErr := syscall.Syscall6(unix.SYS_PRCTL, unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&sub)), 0, 0, 0, 0)
	runtime.KeepAlive(&sub)
	if subErr != 0 {
		return result, unavailable("GET_CHILD_SUBREAPER", subErr)
	}
	if sub != 0 && sub != 1 {
		return result, fmt.Errorf("%w: subreaper out of range", ErrUnsafeKernel)
	}
	count, err := walkKernelThreads(os.Getpid(), func(tid int, s KernelSnapshot) error {
		s.CapLast = uint32(last)
		s.Dumpable = dump
		s.Securebits = uint32(secure)
		s.Subreaper = sub == 1
		if err := validateKernelSnapshot(s, role, initial); err != nil {
			return fmt.Errorf("thread %d: %w", tid, err)
		}
		result = s
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrUnsafeKernel) {
			return KernelSnapshot{}, err
		}
		return KernelSnapshot{}, unavailable("read threads", err)
	}
	result.Threads = count
	return result, nil
}
