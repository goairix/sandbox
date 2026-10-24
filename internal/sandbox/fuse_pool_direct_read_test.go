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
