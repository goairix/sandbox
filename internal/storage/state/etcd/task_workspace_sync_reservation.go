package etcd

import (
	"context"

	"github.com/goairix/sandbox/internal/runtime/controltransport"
)

// ReserveTaskWorkspaceSync binds canonical target and policy to the original
// metadata reservation. It supplies neither provider effects nor settlement.
func (b *Backend) ReserveTaskWorkspaceSync(ctx context.Context, c *TaskClaim, prepared *PreparedTaskQuiescence, destination *controltransport.Destination, plan TaskWorkspaceSyncPlan) (TaskFinalSyncReservationResult, error) {
	result := TaskFinalSyncReservationResult{Outcome: OutcomeUnknown}
	metadata, err := plan.FinalSyncPlan()
	if err != nil || ctx == nil || !b.validTaskClaim(c) || prepared == nil || prepared.self != prepared || prepared.origin != b || prepared.claim != c || prepared.claimReference != c.reference {
		return result, ErrInvalidRecord
	}
	c.mu.Lock()
	task, err := taskCloseOriginalTask(c)
	if err == nil && task != prepared.pin.task {
		err = ErrInvalidRecord
	}
	if err == nil && task.WorkspaceHash != plan.workspace.Hash() {
		err = ErrIdentityMismatch
	}
	c.mu.Unlock()
	if err != nil {
		return result, err
	}
	return b.ReserveTaskFinalSync(ctx, c, prepared, destination, metadata)
}
