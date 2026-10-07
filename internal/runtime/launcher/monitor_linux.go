//go:build linux && (amd64 || arm64)

package launcher

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

var monitorInitialization struct {
	sync.Mutex
	attempted bool
}

// ConfineMonitor irreversibly installs a fixed inherited policy on a prepared
// root monitor. Supported callers use CGO0 and exclusively Go-owned threads, with
// no children and no concurrent fork/spawn/wait owner. Once a valid-role attempt
// begins, any failure requires process termination; the layer is never repaired
// or rolled back. This handle grants no execution permission.
func ConfineMonitor(kernel *KernelBoundary, uid, gid uint32) (*MonitorBoundary, error) {
	if kernel == nil || kernel.self == nil {
		return nil, ErrKernelUnavailable
	}
	if kernel.self != kernel {
		return nil, fmt.Errorf("%w: copied kernel boundary", ErrUnsafeKernel)
	}
	kernel.mu.Lock()
	valid := kernel.role == roleMonitor && kernel.pid == os.Getpid() && kernel.pid > 1
	kernel.mu.Unlock()
	if !valid {
		return nil, fmt.Errorf("%w: confinement requires current prepared monitor", ErrUnsafeKernel)
	}
	monitorInitialization.Lock()
	defer monitorInitialization.Unlock()
	if monitorInitialization.attempted {
		return nil, fmt.Errorf("%w: monitor confinement already attempted", ErrUnsafeKernel)
	}
	monitorInitialization.attempted = true
	if err := validateMonitorIdentity(uid, gid); err != nil {
		return nil, err
	}
	if err := kernel.ValidateCurrent(); err != nil {
		return nil, err
	}
	// WNOWAIT never consumes a child's status. A successful WNOHANG result means
	// children exist, even when siginfo has no waitable PID. Only ECHILD is safe.
	var info unix.Siginfo
	err := unix.Waitid(unix.P_ALL, 0, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT|unix.WALL, nil)
	if !errors.Is(err, unix.ECHILD) {
		if err == nil {
			return nil, fmt.Errorf("%w: monitor already has children", ErrUnsafeKernel)
		}
		return nil, unavailable("monitor no-child waitid", err)
	}
	membership, err := inspectMonitorCgroup()
	if err != nil {
		return nil, err
	}
	before, err := inspectMonitorSeccomp(os.Getpid(), 0, true)
	if err != nil {
		return nil, err
	}
	if err = installMonitorPolicy(); err != nil {
		return nil, err
	}
	if _, err = inspectMonitorSeccomp(os.Getpid(), before+1, true); err != nil {
		return nil, err
	}
	m := &MonitorBoundary{kernel: kernel, pid: os.Getpid(), uid: uid, gid: gid, installedFilters: before + 1, cgroup: bytes.Clone(membership), confined: true}
	m.self = m
	if err = m.validateCurrentLocked(); err != nil {
		return nil, err
	}
	return m, nil
}

// ValidateCurrent inspects actual kernel state without repairing it. A concurrent
// method owner is rejected rather than waiting behind a drain operation.
func (m *MonitorBoundary) ValidateCurrent() error {
	if err := m.validateReceiver(os.Getpid()); err != nil {
		return err
	}
	if !m.mu.TryLock() {
		return fmt.Errorf("%w: monitor operation active", ErrKernelUnavailable)
	}
	defer m.mu.Unlock()
	return m.validateCurrentLocked()
}

// The caller owns m.mu (or the unpublished constructor handle). Drain uses this
// private method to avoid recursively acquiring its nonblocking operation lock.
func (m *MonitorBoundary) validateCurrentLocked() error {
	if err := m.validateReceiver(os.Getpid()); err != nil {
		return err
	}
	if !m.confined || m.kernel == nil || m.installedFilters == 0 {
		return ErrKernelUnavailable
	}
	if m.poisoned {
		return fmt.Errorf("%w: monitor poisoned", ErrUnsafeKernel)
	}
	if err := validateMonitorIdentity(m.uid, m.gid); err != nil {
		return err
	}
	if err := m.kernel.ValidateCurrent(); err != nil {
		return err
	}
	if _, err := inspectMonitorSeccomp(m.pid, m.installedFilters, false); err != nil {
		return err
	}
	membership, err := inspectMonitorCgroup()
	if err != nil {
		return err
	}
	if !bytes.Equal(membership, m.cgroup) {
		return fmt.Errorf("%w: monitor cgroup membership changed", ErrUnsafeKernel)
	}
	return nil
}
