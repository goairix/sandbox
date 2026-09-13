package redis

import (
	"context"

	"github.com/goairix/sandbox/internal/storage/state"
)

// ConfirmRecordAbsence is used only when a cleanup is about to be reported as
// complete without a remaining active record. Normal Load remains read-only.
func (r *ActiveSandboxRepository) ConfirmRecordAbsence(ctx context.Context, id string) error {
	if r.validateID(id) != nil {
		return state.ErrActiveSandboxCorrupt
	}
	if r.store.durability != DurabilityReplicaAck {
		return nil
	}
	absent, err := r.store.ConfirmAbsence(ctx, r.keys(id).record)
	if err != nil {
		return err
	}
	if !absent {
		return state.ErrActiveSandboxConflict
	}
	return nil
}
