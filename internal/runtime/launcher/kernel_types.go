// Package launcher verifies a Linux process's kernel privilege boundary at startup.
// A boundary is diagnostic evidence, never authorization to execute work.
package launcher

import (
	"errors"
	"fmt"
	"sync"
)

var (
	ErrUnsupported       = errors.New("launcher kernel boundary unsupported")
	ErrUnsafeKernel      = errors.New("unsafe launcher kernel state")
	ErrKernelUnavailable = errors.New("launcher kernel state unavailable")
)

type KernelSnapshot struct {
	PID                                                  int
	UIDs, GIDs                                           [4]uint32
	Permitted, Effective, Inheritable, Ambient, Bounding uint64
	CapLast                                              uint32
	NoNewPrivileges                                      bool
	Dumpable                                             int
	Subreaper                                            bool
	Securebits                                           uint32
	Threads                                              uint32
}

type kernelRole uint8

const (
	rolePID1    kernelRole = 1
	roleMonitor kernelRole = 2
)

type KernelBoundary struct {
	mu       sync.Mutex
	self     *KernelBoundary
	role     kernelRole
	pid      int
	verified KernelSnapshot
	seal     *pid1Seal
}

func (b *KernelBoundary) Snapshot() KernelSnapshot {
	if b == nil || b.self != b {
		return KernelSnapshot{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.verified
}

func validateKernelSnapshot(s KernelSnapshot, role kernelRole, initial bool) error {
	if role != rolePID1 && role != roleMonitor {
		return fmt.Errorf("%w: unknown role", ErrUnsafeKernel)
	}
	if (role == rolePID1 && s.PID != 1) || (role == roleMonitor && s.PID <= 1) {
		return fmt.Errorf("%w: role/PID mismatch", ErrUnsafeKernel)
	}
	if s.UIDs != [4]uint32{} || s.GIDs != [4]uint32{} {
		return fmt.Errorf("%w: non-root credentials", ErrUnsafeKernel)
	}
	if s.CapLast < 8 || s.CapLast > 63 || s.Threads < 1 || s.Threads > 4096 {
		return fmt.Errorf("%w: capability/thread bounds", ErrUnsafeKernel)
	}
	if !s.NoNewPrivileges || s.Securebits != 0 {
		return fmt.Errorf("%w: no_new_privs/securebits", ErrUnsafeKernel)
	}
	if s.Dumpable < 0 || s.Dumpable > 1 || (!initial && (s.Dumpable != 0 || !s.Subreaper)) {
		return fmt.Errorf("%w: dumpable/subreaper", ErrUnsafeKernel)
	}
	p, i, b := uint64(0xe0), uint64(0), uint64(0)
	if role == rolePID1 && initial {
		p, b = 0x1e0, 0x1e0
	}
	if (role == rolePID1 && !initial) || (role == roleMonitor && initial) {
		i = 0xe0
	}
	if s.Permitted != p || s.Effective != p || s.Inheritable != i || s.Bounding != b || s.Ambient != 0 {
		return fmt.Errorf("%w: capability sets P=%x E=%x I=%x A=%x B=%x", ErrUnsafeKernel, s.Permitted, s.Effective, s.Inheritable, s.Ambient, s.Bounding)
	}
	return nil
}
