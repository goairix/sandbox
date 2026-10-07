package launcher

import "testing"

// Constructor origin must be checked before returning even diagnostic snapshots.
// A fabricated handle used to disclose fields that consumers could mistake for
// an original current process boundary.
func TestKernelOriginRejectsFabricatedDiagnostics(t *testing.T) {
	b := &KernelBoundary{pid: 1, role: rolePID1, verified: KernelSnapshot{PID: 1, Threads: 2}}
	if got := b.Snapshot(); got != (KernelSnapshot{}) {
		t.Fatalf("fabricated boundary exposed diagnostics: %+v", got)
	}
}

func TestKernelOriginCopiedDiagnostics(t *testing.T) {
	original := &KernelBoundary{pid: 1, role: rolePID1, verified: KernelSnapshot{PID: 1, Threads: 2}}
	original.self = original
	copied := &KernelBoundary{self: original, pid: original.pid, role: original.role, verified: original.verified}
	// A held copied mutex must not be reached by a rejected copy.
	copied.mu.Lock()
	defer copied.mu.Unlock()
	if got := copied.Snapshot(); got != (KernelSnapshot{}) {
		t.Fatalf("copied boundary exposed diagnostics: %+v", got)
	}
	if got := original.Snapshot(); got.PID != 1 {
		t.Fatal("original diagnostic handle lost")
	}
}
