package sandbox

import (
	"context"
	"errors"

	"github.com/goairix/sandbox/internal/runtime"
)

// releaseInterruptedSync is only used after admission drained and the exact
// runtime was removed. It also covers the Acquire-success/publish-crash gap.
// Matching values are compare-deleted; the fencing generation is never reset.
func (c *WorkspaceCoordinator) releaseInterruptedSync(ctx context.Context, req WorkspaceLeaseRequest, expected WorkspaceOwner) error {
	if err := c.validateRequest(req); err != nil || req.MountType != WorkspaceMountSync || req.RuntimeUID == "" {
		return errors.Join(ErrInvalidWorkspaceLease, err)
	}
	keys, err := workspaceStateKeys(req)
	if err != nil {
		return err
	}
	raw, err := c.store.Get(ctx, keys.owner)
	if err != nil {
		return err
	}
	if raw != nil {
		var owner WorkspaceOwner
		if strictDecodeFlatJSONObject(raw, &owner) != nil || validateStoredOwner(owner) != nil ||
			owner.MountType != req.MountType || owner.SandboxID != req.SandboxID || owner.RuntimeID != req.RuntimeID ||
			owner.RuntimeUID != req.RuntimeUID || owner.Runtime != req.Runtime || owner.Prefix != req.Prefix ||
			owner.Provider != req.Provider || owner.Bucket != req.Bucket || owner.StorageIdentityHash != storageIdentityHash(req.StorageIdentity) ||
			(expected.Generation > 0 && owner != expected) {
			return ErrWorkspaceOwnerLost
		}
		lease, err := c.Restore(ctx, owner)
		if err != nil {
			return err
		}
		return c.Release(ctx, lease, runtime.TerminationEvidence{})
	}
	// Release may already have deleted the owner when its response was lost.
	// Never synthesize a new capability or remove another actor's live lease.
	leaseRaw, err := c.store.Get(ctx, keys.lease)
	if err != nil || leaseRaw == nil {
		return err
	}
	record, err := parseActiveWorkspaceLeaseRecord(leaseRaw)
	if err != nil || record.MountType != req.MountType || record.SandboxID != req.SandboxID || record.RuntimeID != req.RuntimeID ||
		record.RuntimeUID != req.RuntimeUID || record.Prefix != req.Prefix || record.Runtime != req.Runtime ||
		record.Provider != req.Provider || record.Bucket != req.Bucket || record.StorageIdentityHash != storageIdentityHash(req.StorageIdentity) ||
		(expected.Generation > 0 && record.Generation != expected.Generation) {
		return ErrWorkspaceLeaseLost
	}
	deleted, err := c.store.CompareAndDelete(ctx, keys.lease, leaseRaw)
	if err != nil {
		return err
	}
	if !deleted {
		current, err := c.store.Get(ctx, keys.lease)
		if err != nil || current != nil {
			return errors.Join(ErrWorkspaceLeaseLost, err)
		}
	}
	return nil
}
