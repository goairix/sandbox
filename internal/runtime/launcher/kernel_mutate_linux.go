//go:build linux

package launcher

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func setterError(op string, err syscall.Errno) error {
	if err == 0 {
		return nil
	}
	if err == syscall.ENOTSUP || err == syscall.ENOSYS {
		return fmt.Errorf("%w: %s: %w", ErrUnsupported, op, err)
	}
	return unavailable(op, err)
}

func allThreadsPrctl(option int, arg uintptr) error {
	_, _, err := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, uintptr(option), arg, 0, 0, 0, 0)
	return setterError(fmt.Sprintf("all-thread prctl %d", option), err)
}
func allThreadsCaps(permitted, effective, inheritable uint64) error {
	header := &unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := &[2]unix.CapUserData{
		{Permitted: uint32(permitted), Effective: uint32(effective), Inheritable: uint32(inheritable)},
		{Permitted: uint32(permitted >> 32), Effective: uint32(effective >> 32), Inheritable: uint32(inheritable >> 32)},
	}
	// AllThreadsSyscall is marked uintptrescapes. These allocations remain stable
	// across the runtime's stop-the-world syscall dispatch and live until completion.
	_, _, err := syscall.AllThreadsSyscall(unix.SYS_CAPSET, uintptr(unsafe.Pointer(header)), uintptr(unsafe.Pointer(data)), 0)
	runtime.KeepAlive(header)
	runtime.KeepAlive(data)
	return setterError("all-thread capset", err)
}
func mutateKernel(role kernelRole, capLast uint32) error {
	if role == rolePID1 {
		if err := allThreadsCaps(0x1e0, 0x1e0, 0xe0); err != nil {
			return err
		}
	} else {
		if err := allThreadsCaps(0xe0, 0xe0, 0); err != nil {
			return err
		}
	}
	_, _, errno := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0, 0)
	if err := setterError("all-thread clear ambient", errno); err != nil {
		return err
	}
	if role == rolePID1 {
		for cap := uint32(0); cap <= capLast; cap++ {
			if err := allThreadsPrctl(unix.PR_CAPBSET_DROP, uintptr(cap)); err != nil {
				return err
			}
		}
		if err := allThreadsCaps(0xe0, 0xe0, 0xe0); err != nil {
			return err
		}
	}
	if err := allThreadsPrctl(unix.PR_SET_DUMPABLE, 0); err != nil {
		return err
	}
	return allThreadsPrctl(unix.PR_SET_CHILD_SUBREAPER, 1)
}
