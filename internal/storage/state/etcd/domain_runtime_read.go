package etcd

import "context"

// SandboxRuntimeReadResult is caller-owned historical metadata, never native
// authority. Current health and expiry must be checked independently.
type SandboxRuntimeReadResult struct {
	Publication *RuntimeEnrolledPublicationEntry
	Dispatch    *RuntimeDispatchEntry
}

// ReadSandboxRuntime reads the full retained enrolled chain using only this
// Backend's original current view. Copied Records cannot select a history key.
func (b *Backend) ReadSandboxRuntime(ctx context.Context, view *SandboxReadView) (*SandboxRuntimeReadResult, error) {
	if err := b.ConfirmSandboxView(ctx, view); err != nil {
		return nil, err
	}
	call, err := b.registerEnrolledPublication(ctx)
	if err != nil {
		return nil, err
	}
	defer call.finish()
	c, owner, placement := view.records.Control, view.records.Owner, view.records.Placement
	w := runtimeHistoryLocator{hash: c.WorkspaceHash, partition: placement.Partition}
	result, err := b.loadRuntimePublicationEnrolledAt(call.context, w, c.IntentID)
	if err != nil {
		return nil, err
	}
	if result == nil || result.Publication == nil || result.Dispatch == nil {
		return nil, ErrConflict
	}
	r := result.Publication.Record
	if c.Runtime == nil || owner.Runtime == nil || *c.Runtime != r.Runtime || *owner.Runtime != r.Runtime || r.SandboxID != c.SandboxID || r.IntentID != c.IntentID || r.WorkspaceHash != c.WorkspaceHash || r.Generation != c.Generation || r.RestoreEpoch != c.RestoreEpoch || r.Snapshot != c.Snapshot || r.DataGateEpoch != c.DataGateEpoch {
		return nil, ErrConflict
	}
	if err := b.ConfirmSandboxView(call.context, view); err != nil {
		return nil, err
	}
	if err := call.context.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
