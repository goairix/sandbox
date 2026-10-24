package etcd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
)

func consumedPoolRecord(t *testing.T, repository *FUSEPoolRepository) *state.FUSEPoolRecord {
	t.Helper()
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	require.NoError(t, repository.CreatePreparingWithAdmission(ctx,
		preparingPoolRecord("preparation"), "holder", 1, 10*time.Second))
	_, err = repository.BindPreparingRuntime(ctx, "preparation", "runtime", "uid", "holder", 1)
	require.NoError(t, err)
	_, err = repository.Transition(ctx, "preparation", state.FUSEPoolPreparing, state.FUSEPoolPrepared, "maintainer", 2)
	require.NoError(t, err)
	reserved, err := repository.ReservePrepared(ctx, "shared-pool", "reservation", 5*time.Second)
	require.NoError(t, err)
	binding, err := repository.Transition(ctx, "preparation", state.FUSEPoolReserved, state.FUSEPoolBinding, "reservation", reserved.Revision)
	require.NoError(t, err)
	consumed, err := repository.Transition(ctx, "preparation", state.FUSEPoolBinding, state.FUSEPoolConsumed, "reservation", binding.Revision)
	require.NoError(t, err)
	return consumed
}

func TestPoolClaimConsumedCleanupKeepsOwnership(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	consumed := consumedPoolRecord(t, repository)
	ctx := context.Background()
	claim := func(runtimeUID, cleanupToken string) (*state.FUSEPoolRecord, error) {
		return repository.ClaimCleanup(ctx, consumed.PreparationID, state.FUSEPoolConsumed,
			consumed.MaintainerToken, consumed.ReservationToken, consumed.Revision,
			consumed.RuntimeID, runtimeUID, cleanupToken, 5*time.Second)
	}
	_, err := claim("wrong-uid", "cleanup")
	require.ErrorIs(t, err, state.ErrFUSEPoolConflict)
	claimed, err := claim(consumed.RuntimeUID, "cleanup")
	require.NoError(t, err)
	require.Equal(t, state.FUSEPoolCleanup, claimed.State)
	require.Equal(t, state.FUSEPoolCleanupTerminating, claimed.CleanupPhase)
	require.Equal(t, consumed.Revision+1, claimed.Revision)
	require.Equal(t, consumed.RuntimeUID, claimed.RuntimeUID)
	require.Nil(t, claimed.TerminationEvidence)
	owner, err := s.client.Get(ctx, repository.uidOwnerKey(consumed.RuntimeUID))
	require.NoError(t, err)
	require.Len(t, owner.Kvs, 1)
	require.Equal(t, consumed.PreparationID, string(owner.Kvs[0].Value))
	count, err := repository.CountPreparingAndPrepared(ctx, "shared-pool")
	require.NoError(t, err)
	require.Zero(t, count)
	_, err = claim(consumed.RuntimeUID, "other")
	require.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)
	again, err := repository.ClaimCleanup(ctx, consumed.PreparationID, state.FUSEPoolCleanup,
		consumed.MaintainerToken, consumed.ReservationToken, consumed.Revision,
		consumed.RuntimeID, consumed.RuntimeUID, "cleanup", 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, claimed.Revision, again.Revision)
}

func TestPoolExpiredCleanupLeaseAllowsNewOwnerWithoutReleasingRuntime(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	consumed := consumedPoolRecord(t, repository)
	ctx := context.Background()
	first, err := repository.ClaimCleanup(ctx, consumed.PreparationID, state.FUSEPoolConsumed,
		consumed.MaintainerToken, consumed.ReservationToken, consumed.Revision,
		consumed.RuntimeID, consumed.RuntimeUID, "cleanup-a", time.Second)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		read, err := s.client.Get(ctx, repository.cleanupDeadlineKey(consumed.PreparationID))
		return err == nil && len(read.Kvs) == 0
	}, 4*time.Second, 50*time.Millisecond)
	second, err := repository.ClaimCleanup(ctx, consumed.PreparationID, state.FUSEPoolCleanup,
		consumed.MaintainerToken, consumed.ReservationToken, first.Revision,
		consumed.RuntimeID, consumed.RuntimeUID, "cleanup-b", 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, first.Revision+1, second.Revision)
	require.Equal(t, "cleanup-b", second.CleanupToken)
	require.Equal(t, state.FUSEPoolCleanupTerminating, second.CleanupPhase)
	owner, err := s.client.Get(ctx, repository.uidOwnerKey(consumed.RuntimeUID))
	require.NoError(t, err)
	require.Equal(t, consumed.PreparationID, string(owner.Kvs[0].Value))
	_, err = repository.ClaimCleanup(ctx, consumed.PreparationID, state.FUSEPoolCleanup,
		consumed.MaintainerToken, consumed.ReservationToken, second.Revision,
		consumed.RuntimeID, consumed.RuntimeUID, "cleanup-a", 5*time.Second)
	require.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)
}

func TestPoolClaimCleanupLostReplyIsResolvedBySameToken(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	consumed := consumedPoolRecord(t, repository)
	ctx := context.Background()
	realKV := s.client.KV
	fault := &lostReplyKV{KV: realKV}
	fault.drop.Store(true)
	s.client.KV = fault
	claimed, err := repository.ClaimCleanup(ctx, consumed.PreparationID, state.FUSEPoolConsumed,
		consumed.MaintainerToken, consumed.ReservationToken, consumed.Revision,
		consumed.RuntimeID, consumed.RuntimeUID, "cleanup", 5*time.Second)
	s.client.KV = realKV
	require.Nil(t, claimed)
	require.ErrorContains(t, err, "reply lost after commit")
	again, err := repository.ClaimCleanup(ctx, consumed.PreparationID, state.FUSEPoolConsumed,
		consumed.MaintainerToken, consumed.ReservationToken, consumed.Revision,
		consumed.RuntimeID, consumed.RuntimeUID, "cleanup", 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, consumed.Revision+1, again.Revision)
	require.Equal(t, state.FUSEPoolCleanup, again.State)
}

func TestPoolRepeatedCleanupClaimRejectsMissingUIDOwner(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	consumed := consumedPoolRecord(t, repository)
	ctx := context.Background()
	claimed, err := repository.ClaimCleanup(ctx, consumed.PreparationID, state.FUSEPoolConsumed,
		consumed.MaintainerToken, consumed.ReservationToken, consumed.Revision,
		consumed.RuntimeID, consumed.RuntimeUID, "cleanup", 5*time.Second)
	require.NoError(t, err)
	_, err = s.client.Delete(ctx, repository.uidOwnerKey(consumed.RuntimeUID))
	require.NoError(t, err)
	again, err := repository.ClaimCleanup(ctx, consumed.PreparationID, state.FUSEPoolCleanup,
		consumed.MaintainerToken, consumed.ReservationToken, claimed.Revision,
		consumed.RuntimeID, consumed.RuntimeUID, "cleanup", 5*time.Second)
	require.Nil(t, again)
	require.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
}
