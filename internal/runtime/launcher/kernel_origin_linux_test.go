//go:build linux

package launcher

import (
	"errors"
	"os"
	"testing"
)

func TestKernelOriginRejectsBeforeLock(t *testing.T) {
	original := &KernelBoundary{pid: os.Getpid(), role: roleMonitor}
	original.self = original
	copied := &KernelBoundary{self: original, pid: original.pid, role: original.role}
	copied.mu.Lock()
	defer copied.mu.Unlock()
	if err := copied.ValidateCurrent(); !errors.Is(err, ErrUnsafeKernel) {
		t.Fatalf("copy accepted: %v", err)
	}
	if _, err := ConfineMonitor(copied, 1000, 1000); !errors.Is(err, ErrUnsafeKernel) {
		t.Fatalf("copy reached confinement: %v", err)
	}
}

func TestKernelOriginNative(t *testing.T) {
	original := requireNativeBoundary(t)
	if err := original.ValidateCurrent(); err != nil {
		t.Fatal(err)
	}
	copy := &KernelBoundary{self: original, pid: original.pid, role: original.role, verified: original.verified}
	copy.mu.Lock()
	defer copy.mu.Unlock()
	if err := copy.ValidateCurrent(); !errors.Is(err, ErrUnsafeKernel) {
		t.Fatalf("copy of current PID1 accepted: %v", err)
	}
	if copy.Snapshot() != (KernelSnapshot{}) {
		t.Fatal("copied snapshot available")
	}
	if original.Snapshot().PID != 1 {
		t.Fatal("original current PID1 diagnostics unavailable")
	}
	t.Log("actual PID1 original validated; copied current boundary rejected before held lock and kernel inspection")
}
