package etcd

import (
	"context"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

func TestControllerStagesObserveAcquireAndRenew(t *testing.T) {
	store := testStore(t)
	type observation struct {
		operation string
		stage     string
		result    string
		duration  time.Duration
	}
	var observed []observation
	store.controllerStageObserver = func(_ context.Context, operation, stage, result string, duration time.Duration) {
		observed = append(observed, observation{operation, stage, result, duration})
	}

	repository, err := NewActiveSandboxRepository(store, "controller-observation")
	require.NoError(t, err)
	record := validActiveRecord("observed-controller")
	require.NoError(t, repository.Publish(context.Background(), record))
	request := state.ActiveSandboxControllerLease{
		SandboxID: record.SandboxID, Token: "observed", InstanceID: "api-a", Generation: record.Generation,
	}
	acquired, won, err := repository.AcquireController(context.Background(), request, 15*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	_, err = repository.RenewController(context.Background(), *acquired, 15*time.Second)
	require.NoError(t, err)

	require.Len(t, observed, 7)
	for index, want := range [][2]string{
		{"acquire", "read"}, {"acquire", "grant"}, {"acquire", "commit"},
		{"renew", "read"}, {"renew", "grant"}, {"renew", "commit"}, {"renew", "revoke"},
	} {
		require.Equal(t, want[0], observed[index].operation)
		require.Equal(t, want[1], observed[index].stage)
		require.Equal(t, "success", observed[index].result)
		require.GreaterOrEqual(t, observed[index].duration, time.Duration(0))
	}
}

func TestControllerStagesObserveReadDeadline(t *testing.T) {
	store := testStore(t)
	var operation, stage, result string
	store.controllerStageObserver = func(_ context.Context, gotOperation, gotStage, gotResult string, _ time.Duration) {
		operation, stage, result = gotOperation, gotStage, gotResult
	}
	repository, err := NewActiveSandboxRepository(store, "controller-observation-deadline")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = repository.AcquireController(ctx, state.ActiveSandboxControllerLease{
		SandboxID: "observed-controller", Token: "observed", InstanceID: "api-a", Generation: 1,
	}, 15*time.Second)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, "acquire", operation)
	require.Equal(t, "read", stage)
	require.Equal(t, "error", result)
}
