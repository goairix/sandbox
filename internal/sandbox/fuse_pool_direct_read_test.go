package sandbox

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
)

type exactPoolReaderNoList struct {
	state.FUSEPoolRepository
	record state.FUSEPoolRecord
}

func (r exactPoolReaderNoList) ReadByPreparationID(_ context.Context, id string) (*state.FUSEPoolRecord, error) {
	if id != r.record.PreparationID {
		return nil, nil
	}
	return &r.record, nil
}

func (exactPoolReaderNoList) ListByPoolKey(context.Context, string) ([]state.FUSEPoolRecord, error) {
	return nil, errors.New("whole-pool read is forbidden for a point check")
}

func (exactPoolReaderNoList) DeleteCleanup(context.Context, string, string, uint64) (bool, error) {
	return false, nil
}

func TestReturnPreparedTerminalUsesExactRecordRead(t *testing.T) {
	original := state.FUSEPoolRecord{
		PreparationID: "preparation", RuntimeID: "runtime", RuntimeUID: "runtime-uid",
		PoolKey: "shared-pool", MaintainerToken: "maintainer", ReservationToken: "reservation",
		State: state.FUSEPoolReserved, Revision: 3,
	}
	pool := &FUSEPool{repo: exactPoolReaderNoList{record: original}}
	terminal, err := pool.verifyReturnPreparedTerminal(original)
	require.NoError(t, err)
	require.False(t, terminal)
}

func TestCleanupDeleteConflictUsesExactRecordRead(t *testing.T) {
	claimed := state.FUSEPoolRecord{
		PreparationID: "preparation", RuntimeID: "runtime", RuntimeUID: "runtime-uid",
		PoolKey: "shared-pool", MaintainerToken: "maintainer", CleanupToken: "cleanup",
		State: state.FUSEPoolCleanup, Revision: 7,
	}
	pool := &FUSEPool{repo: exactPoolReaderNoList{record: claimed}}
	err := pool.CompleteClaimedCleanup(context.Background(), claimed)
	require.ErrorIs(t, err, state.ErrFUSEPoolConflict)
	require.NotContains(t, err.Error(), "whole-pool read")
}

func TestExactPoolRecordReadRemainsScopedToPoolKey(t *testing.T) {
	repo := exactPoolReaderNoList{record: state.FUSEPoolRecord{
		PreparationID: "preparation", PoolKey: "other-pool",
	}}
	record, err := readFUSEPoolRecord(context.Background(), repo, "expected-pool", "preparation")
	require.NoError(t, err)
	require.Nil(t, record)
}

type pagePoolReaderNoList struct {
	exactPoolReaderNoList
	pages map[string][]state.FUSEPoolRecord
}

func (r pagePoolReaderNoList) ListStatePage(_ context.Context, _ string, _ state.FUSEPoolState, after string, limit int) ([]state.FUSEPoolRecord, string, error) {
	if limit != 64 {
		return nil, "", errors.New("maintenance page size changed")
	}
	page := r.pages[after]
	if after == "" {
		return page, "next", nil
	}
	return page, "", nil
}

func TestPoolStateTraversalUsesBoundedPages(t *testing.T) {
	repo := pagePoolReaderNoList{pages: map[string][]state.FUSEPoolRecord{
		"":     {{PreparationID: "first", PoolKey: "pool", State: state.FUSEPoolPrepared}},
		"next": {{PreparationID: "second", PoolKey: "pool", State: state.FUSEPoolPrepared}},
	}}
	var ids []string
	err := forEachFUSEPoolState(context.Background(), repo, "pool", state.FUSEPoolPrepared, func(record state.FUSEPoolRecord) error {
		ids = append(ids, record.PreparationID)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"first", "second"}, ids)
}

type emptyPagePoolNoList struct {
	state.FUSEPoolRepository
}

func (emptyPagePoolNoList) ListByPoolKey(context.Context, string) ([]state.FUSEPoolRecord, error) {
	return nil, errors.New("whole-pool read is forbidden during stop")
}

func (emptyPagePoolNoList) ListStatePage(context.Context, string, state.FUSEPoolState, string, int) ([]state.FUSEPoolRecord, string, error) {
	return nil, "", nil
}

func TestPoolOwnedStopUsesStatePages(t *testing.T) {
	pool := NewFUSEPool(newFUSEMockRuntime(), emptyPagePoolNoList{}, fusePoolConfig(), fixedFUSESpec("pool-key"))
	require.NoError(t, pool.drainOwned(context.Background()))
}
