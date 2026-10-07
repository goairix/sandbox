package launcher

import "context"

// pid1Seal is immutable bootstrap identity, separate from refreshed diagnostics.
// Only the supported, unambiguous cgroup-v2 mount topology is accepted.
type pid1Seal struct {
	procDev, procIno, namespaceDev, namespaceIno, cgroupDev, cgroupIno uint64
	procMount, cgroupMount, membership                                 string
}

// UserNamespaceQuiescenceObservation is an original process-local observation.
// It is not a boolean assertion supplied by a caller or a reusable drain grant.
type UserNamespaceQuiescenceObservation struct {
	self   *UserNamespaceQuiescenceObservation
	origin *KernelBoundary
	pid    int
	seal   pid1Seal
}

func (o *UserNamespaceQuiescenceObservation) RevalidateCurrent(ctx context.Context) error {
	if o == nil || o.self != o || o.origin == nil || o.origin.pid != o.pid || o.origin.seal == nil || *o.origin.seal != o.seal {
		return ErrKernelUnavailable
	}
	_, err := o.origin.ObserveNoUserDescendants(ctx)
	return err
}
