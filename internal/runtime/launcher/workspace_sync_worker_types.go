package launcher

import "github.com/google/uuid"

// WorkspaceSyncWorkerReference is copied structural diagnostics. It neither
// authorizes provider work nor reconstructs an original KernelBoundary.
type WorkspaceSyncWorkerReference struct {
	Incarnation, BootID                  string
	PID                                  int
	StartTicks                           uint64
	UID, GID                             uint32
	PIDNamespaceDev, PIDNamespaceIno     uint64
	MountNamespaceDev, MountNamespaceIno uint64
	ExecutableDev, ExecutableIno         uint64
}

type workspaceSyncWorkerSeal struct {
	reference        WorkspaceSyncWorkerReference
	procDev, procIno uint64
}

func workerUUID(raw string, incarnation bool) bool {
	u, err := uuid.Parse(raw)
	return err == nil && u != uuid.Nil && u.String() == raw && (!incarnation || (u.Version() == 4 && u.Variant() == uuid.RFC4122))
}

// Validate checks only the shape of diagnostics, including caller-created ones.
func (r WorkspaceSyncWorkerReference) Validate() error {
	if !workerUUID(r.Incarnation, true) || !workerUUID(r.BootID, false) || r.PID < 1 || uint64(r.PID) > 0x7fffffff || r.StartTicks == 0 || r.UID == 0 || r.GID == 0 || r.PIDNamespaceDev == 0 || r.PIDNamespaceIno == 0 || r.MountNamespaceDev == 0 || r.MountNamespaceIno == 0 || r.ExecutableDev == 0 || r.ExecutableIno == 0 {
		return ErrUnsafeKernel
	}
	return nil
}

func (s workspaceSyncWorkerSeal) matchesSnapshot(k KernelSnapshot) bool {
	return s.reference.PID == k.PID && k.UIDs == [4]uint32{s.reference.UID, s.reference.UID, s.reference.UID, s.reference.UID} && k.GIDs == [4]uint32{s.reference.GID, s.reference.GID, s.reference.GID, s.reference.GID}
}

// WorkspaceSyncWorkerReference returns a value copy only from the original
// worker boundary. A reference is not a fresh kernel observation.
func (b *KernelBoundary) WorkspaceSyncWorkerReference() (WorkspaceSyncWorkerReference, error) {
	if b == nil || b.self != b {
		return WorkspaceSyncWorkerReference{}, ErrKernelUnavailable
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.role != roleWorkspaceSyncWorker || b.workerSeal == nil || b.pid != b.workerSeal.reference.PID || !b.workerSeal.matchesSnapshot(b.verified) {
		return WorkspaceSyncWorkerReference{}, ErrKernelUnavailable
	}
	r := b.workerSeal.reference
	if err := r.Validate(); err != nil {
		return WorkspaceSyncWorkerReference{}, err
	}
	return r, nil
}
