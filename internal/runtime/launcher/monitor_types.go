package launcher

import (
	"fmt"
	"sync"
)

// MonitorBoundary is process-local confinement and cleanup evidence, never start
// permission or proof of remote-write settlement. It must not be copied. Methods
// reject copies, including copies within the same process, by receiver identity.
// No background work is performed while this handle is idle.
type MonitorBoundary struct {
	mu               sync.Mutex
	self             *MonitorBoundary
	kernel           *KernelBoundary
	pid              int
	uid, gid         uint32
	installedFilters uint32
	cgroup           []byte
	confined         bool
	poisoned         bool
	drainStarted     bool
	reaped           []ProcessExit // bounded to 4096 by the sole drain owner
}

// ProcessExit records the actual namespace PID and raw wait status.
type ProcessExit struct {
	PID        int
	WaitStatus uint32
}

// LocalDrainObservation describes local child reaping only. It is not a business
// End, journal admission, or proof that remote writes have settled.
type LocalDrainObservation struct {
	MonitorPID int
	Reaped     []ProcessExit
}

func validateMonitorIdentity(uid, gid uint32) error {
	if uid == 0 || gid == 0 || uid > 2147483647 || gid > 2147483647 {
		return fmt.Errorf("%w: monitor user identity outside 1..2147483647", ErrUnsafeKernel)
	}
	return nil
}

// Only immutable seal fields are inspected before taking the nonblocking owner.
func (m *MonitorBoundary) validateReceiver(pid int) error {
	if m == nil || m.self == nil || m.pid == 0 {
		return ErrKernelUnavailable
	}
	if m.self != m || m.pid != pid {
		return fmt.Errorf("%w: copied monitor or another PID", ErrUnsafeKernel)
	}
	return nil
}
