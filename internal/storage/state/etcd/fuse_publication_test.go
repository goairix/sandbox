package etcd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

func TestFUSEBoundSessionPublicationAndCleanupShareOneEtcdFence(t *testing.T) {
	for _, cleanupFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "publication first", true: "cleanup first"}[cleanupFirst], func(t *testing.T) {
			store := testStore(t)
			repository := NewFUSEPoolRepository(store)
			consumed := consumedPoolRecord(t, repository)
			ctx := context.Background()
			binding := state.FUSEPoolPublication{
				PoolKey: consumed.PoolKey, PreparationID: consumed.PreparationID,
				RuntimeID: consumed.RuntimeID, RuntimeUID: consumed.RuntimeUID,
				ReservationToken: consumed.ReservationToken,
			}
			publish := func() error {
				return store.SetFUSEBound(ctx, "sandbox:session:v2:bound", []byte("session"), time.Minute, binding)
			}
			claim := func() error {
				_, err := repository.ClaimCleanup(ctx, consumed.PreparationID, state.FUSEPoolConsumed,
					consumed.MaintainerToken, consumed.ReservationToken, consumed.Revision,
					consumed.RuntimeID, consumed.RuntimeUID, "cleanup", 5*time.Second)
				return err
			}
			if cleanupFirst {
				require.NoError(t, claim())
				require.ErrorIs(t, publish(), state.ErrFUSEPoolConflict)
				value, err := store.Get(ctx, "sandbox:session:v2:bound")
				require.NoError(t, err)
				require.Nil(t, value)
				return
			}
			require.NoError(t, publish())
			require.NoError(t, claim())
			value, err := store.Get(ctx, "sandbox:session:v2:bound")
			require.NoError(t, err)
			require.Equal(t, []byte("session"), value)
		})
	}
}

func TestFUSEBoundPublicationRejectsWrongRuntimeWithoutWrite(t *testing.T) {
	store := testStore(t)
	repository := NewFUSEPoolRepository(store)
	consumed := consumedPoolRecord(t, repository)
	binding := state.FUSEPoolPublication{
		PoolKey: consumed.PoolKey, PreparationID: consumed.PreparationID,
		RuntimeID: consumed.RuntimeID, RuntimeUID: "wrong-runtime",
		ReservationToken: consumed.ReservationToken,
	}
	err := store.SetFUSEBound(context.Background(), "sandbox:session:v2:wrong", []byte("session"), 0, binding)
	require.True(t, errors.Is(err, state.ErrFUSEPoolConflict))
	value, err := store.Get(context.Background(), "sandbox:session:v2:wrong")
	require.NoError(t, err)
	require.Nil(t, value)
}

func TestFUSEBoundCreateRejectsCleanupAndExistingKey(t *testing.T) {
	store := testStore(t)
	repository := NewFUSEPoolRepository(store)
	consumed := consumedPoolRecord(t, repository)
	ctx := context.Background()
	binding := state.FUSEPoolPublication{
		PoolKey: consumed.PoolKey, PreparationID: consumed.PreparationID,
		RuntimeID: consumed.RuntimeID, RuntimeUID: consumed.RuntimeUID,
		ReservationToken: consumed.ReservationToken,
	}
	created, err := store.SetNXFUSEBound(ctx, "sandbox:ephemeral:v1:bound", []byte("first"), 0, binding)
	require.NoError(t, err)
	require.True(t, created)
	created, err = store.SetNXFUSEBound(ctx, "sandbox:ephemeral:v1:bound", []byte("second"), 0, binding)
	require.NoError(t, err)
	require.False(t, created)
	_, err = repository.ClaimCleanup(ctx, consumed.PreparationID, state.FUSEPoolConsumed,
		consumed.MaintainerToken, consumed.ReservationToken, consumed.Revision,
		consumed.RuntimeID, consumed.RuntimeUID, "cleanup", 5*time.Second)
	require.NoError(t, err)
	created, err = store.SetNXFUSEBound(ctx, "sandbox:ephemeral:v1:other", []byte("third"), 0, binding)
	require.ErrorIs(t, err, state.ErrFUSEPoolConflict)
	require.False(t, created)
	value, err := store.Get(ctx, "sandbox:ephemeral:v1:bound")
	require.NoError(t, err)
	require.Equal(t, []byte("first"), value)
}
