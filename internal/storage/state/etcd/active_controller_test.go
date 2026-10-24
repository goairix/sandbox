package etcd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
)

func TestControllerLeaseFencesOldHolderAfterExpiry(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-controller")
	require.NoError(t, repository.Publish(ctx, record))
	first := state.ActiveSandboxControllerLease{
		SandboxID: record.SandboxID, Token: "first", InstanceID: "api-1", Generation: record.Generation,
	}
	acquired, won, err := repository.AcquireController(ctx, first, time.Second)
	require.NoError(t, err)
	require.True(t, won)
	require.Greater(t, acquired.ExpiresAt, time.Now())
	second := first
	second.Token = "second"
	second.InstanceID = "api-2"
	_, won, err = repository.AcquireController(ctx, second, 5*time.Second)
	require.NoError(t, err)
	require.False(t, won)
	require.Eventually(t, func() bool {
		_, won, err := repository.AcquireController(ctx, second, 5*time.Second)
		return err == nil && won
	}, 4*time.Second, 50*time.Millisecond)
	_, err = repository.RenewController(ctx, *acquired, 5*time.Second)
	require.ErrorIs(t, err, state.ErrActiveSandboxStaleToken)
	require.ErrorIs(t, repository.ReleaseController(ctx, *acquired), state.ErrActiveSandboxStaleToken)
	require.NoError(t, repository.ReleaseController(ctx, second))
	_, won, err = repository.AcquireController(ctx, first, 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
}

func TestControllerRenewPreservesHolderAndChangesLease(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-controller-renew")
	require.NoError(t, repository.Publish(ctx, record))
	holder := state.ActiveSandboxControllerLease{
		SandboxID: record.SandboxID, Token: "holder", InstanceID: "api-1", Generation: record.Generation,
	}
	acquired, won, err := repository.AcquireController(ctx, holder, time.Second)
	require.NoError(t, err)
	require.True(t, won)
	renewed, err := repository.RenewController(ctx, *acquired, 5*time.Second)
	require.NoError(t, err)
	require.Greater(t, renewed.ExpiresAt, acquired.ExpiresAt)
	time.Sleep(2200 * time.Millisecond)
	other := holder
	other.Token = "other"
	_, won, err = repository.AcquireController(ctx, other, 5*time.Second)
	require.NoError(t, err)
	require.False(t, won)
	require.NoError(t, repository.ReleaseController(ctx, *renewed))
}
