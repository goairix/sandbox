package etcd

import (
	"context"
	"errors"

	"github.com/goairix/sandbox/internal/runtime/launcher"
)

// BindTaskWorkspaceArchiveLocal consumes the original received archive once.
// The returned slot is a diagnostic locator; recovery independently verifies
// its immutable manifest and the current claimant before any filesystem work.
func (b *Backend) BindTaskWorkspaceArchiveLocal(ctx context.Context, received *ReceivedTaskWorkspaceArchive, plan TaskWorkspaceSyncPlan, worker *launcher.KernelBoundary, root string) (slot uint32, resultErr error) {
	if received == nil || received.self != received || received.origin != b || received.owner == nil || received.owner.backend != b || received.owner.spool == nil || received.owner.spool.recovery == nil {
		return 0, ErrInvalidRecord
	}
	o, err := b.beginLocalRuntimeReleasePurpose(ctx, received.owner.claim, "bind")
	if err != nil {
		return 0, err
	}
	defer o.finish()
	source, err := b.PrepareTaskWorkspaceSyncSource(o.ctx, received, plan)
	if err != nil {
		return 0, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, source.Close())
		if resultErr != nil {
			slot = 0
		}
	}()
	target, err := openTaskWorkspaceSyncLocalTarget(o.ctx, worker, plan, root)
	if err != nil {
		return 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, target.Close()) }()
	if err := source.bindLocalFinalizationTarget(o.ctx, target); err != nil {
		return 0, err
	}
	return received.owner.spool.recovery.slot, nil
}
