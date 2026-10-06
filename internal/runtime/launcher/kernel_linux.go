//go:build linux

package launcher

import (
	"fmt"
	"os"
	"sync"
)

var kernelInitialization struct {
	sync.Mutex
	attempted bool
}

// BootstrapPID1 irreversibly drops startup privileges on every Go runtime thread.
// The deployment must have already set no_new_privs. On any error the caller must
// terminate this process; it must never open an execution gate or start user code.
func BootstrapPID1() (*KernelBoundary, error) { return initializeKernel(rolePID1) }

// PrepareMonitor accepts only a fresh root monitor exec inheriting the PID1
// boundary. It removes inheritable capabilities before any later user exec.
func PrepareMonitor() (*KernelBoundary, error) { return initializeKernel(roleMonitor) }

func initializeKernel(role kernelRole) (*KernelBoundary, error) {
	kernelInitialization.Lock()
	defer kernelInitialization.Unlock()
	if kernelInitialization.attempted {
		return nil, fmt.Errorf("%w: initialization already attempted", ErrUnsafeKernel)
	}
	kernelInitialization.attempted = true
	before, err := inspectKernel(role, true)
	if err != nil {
		return nil, err
	}
	if err := mutateKernel(role, before.CapLast); err != nil {
		return nil, err
	}
	after, err := inspectKernel(role, false)
	if err != nil {
		return nil, err
	}
	return &KernelBoundary{role: role, pid: after.PID, verified: after}, nil
}

// ValidateCurrent refreshes diagnostics by inspecting the actual kernel. It makes
// no changes and does not turn this handle into an execution authorization.
func (b *KernelBoundary) ValidateCurrent() error {
	if b == nil {
		return ErrKernelUnavailable
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pid == 0 || b.role == 0 {
		return ErrKernelUnavailable
	}
	if b.pid != os.Getpid() {
		return fmt.Errorf("%w: boundary belongs to another PID", ErrUnsafeKernel)
	}
	s, err := inspectKernel(b.role, false)
	if err != nil {
		return err
	}
	b.verified = s
	return nil
}
