package sandbox

import (
	"context"
	"testing"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

type unconfirmedCleanupRepository struct{ *memoryFUSEPoolRepository }

func (r *unconfirmedCleanupRepository) ConfirmReturnPreparedTerminal(context.Context, string, *state.FUSEPoolRecord) error {
	return state.ErrDurabilityUnconfirmed
}

func (r *unconfirmedCleanupRepository) DeleteCleanup(ctx context.Context, id, token string, revision uint64) (bool, error) {
	deleted, err := r.memoryFUSEPoolRepository.DeleteCleanup(ctx, id, token, revision)
	if err != nil {
		return deleted, err
	}
	return deleted, state.ErrDurabilityUnconfirmed
}

func TestReturnPreparedTerminalRequiresDurabilityConfirmation(t *testing.T) {
	for _, absent := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepared", true: "absent"}[absent], func(t *testing.T) {
			repo := &unconfirmedCleanupRepository{newMemoryFUSEPoolRepository()}
			original := state.FUSEPoolRecord{PreparationID: "preparation-return-ack", RuntimeID: "runtime-a", RuntimeUID: "uid-a", PoolKey: "pool-key", MaintainerToken: "api-a", State: state.FUSEPoolReserved, ReservationToken: "reservation-a", Revision: 3}
			if !absent {
				returned := original
				returned.State, returned.ReservationToken, returned.Revision = state.FUSEPoolPrepared, "", 4
				repo.seed(returned)
			}
			pool := NewFUSEPool(newFUSEMockRuntime(), repo, fusePoolConfig(), fixedFUSESpec("pool-key"))
			require.ErrorIs(t, pool.ReturnPrepared(context.Background(), original), state.ErrDurabilityUnconfirmed)
		})
	}
}

func TestCompleteClaimedCleanupDoesNotReplaceReplicaAckWithPrimaryAbsence(t *testing.T) {
	repo := &unconfirmedCleanupRepository{newMemoryFUSEPoolRepository()}
	claimed := state.FUSEPoolRecord{PreparationID: "preparation-ack", PoolKey: "pool-key", State: state.FUSEPoolCleanup, CleanupToken: "cleanup-ack", Revision: 4}
	repo.seed(claimed)
	pool := NewFUSEPool(newFUSEMockRuntime(), repo, fusePoolConfig(), fixedFUSESpec("pool-key"))
	require.ErrorIs(t, pool.CompleteClaimedCleanup(context.Background(), claimed), state.ErrDurabilityUnconfirmed)
	_, exists := repo.record(claimed.PreparationID)
	require.False(t, exists, "primary deletion alone must not prove replication")
	require.ErrorIs(t, pool.CompleteClaimedCleanup(context.Background(), claimed), state.ErrDurabilityUnconfirmed, "retrying absent cleanup must not skip confirmation")
}
