package etcd

import (
	"context"
	"encoding/json"
	"github.com/goairix/sandbox/internal/storage/state"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func nextActiveChange(t *testing.T, ch <-chan state.ActiveChangeBatch, match func(state.ActiveChangeBatch) bool) state.ActiveChangeBatch {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case batch, ok := <-ch:
			require.True(t, ok)
			if match(batch) {
				return batch
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for active change")
			return state.ActiveChangeBatch{}
		}
	}
}

func TestActiveChangesPreserveRevisionBatches(t *testing.T) {
	_, r, view := sharedDataFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed, err := r.ObserveOperations(ctx, "shared-sandbox")
	require.NoError(t, err)
	require.EqualValues(t, 1, observed.Count)
	ch, err := r.WatchActiveChanges(ctx, observed.Revision+1)
	require.NoError(t, err)
	_, _, _, err = r.BeginDestroy(ctx, "shared-sandbox")
	require.NoError(t, err)
	batch := nextActiveChange(t, ch, func(b state.ActiveChangeBatch) bool { return len(b.Changes) > 0 })
	require.Greater(t, batch.Revision, observed.Revision)
	require.Equal(t, "shared-sandbox", batch.Changes[0].SandboxID)
	require.True(t, batch.Changes[0].RecordChanged)
	require.NoError(t, r.EndOperation(ctx, view.Lease.Operation))
	batch = nextActiveChange(t, ch, func(b state.ActiveChangeBatch) bool { return len(b.Changes) > 0 && b.Changes[0].OperationChanged })
	require.True(t, batch.Changes[0].OperationChanged)
}

func TestActiveChangesResumeAfterDisconnect(t *testing.T) {
	_, r, _ := sharedDataFixture(t)
	ctx := context.Background()
	observed, err := r.ObserveOperations(ctx, "shared-sandbox")
	require.NoError(t, err)
	// The consumer was disconnected when this record changed; revision replay
	// must observe the already committed transition when it reconnects.
	_, err = r.Update(ctx, "shared-sandbox", 2, json.RawMessage(`{"ready":true,"new":true}`))
	require.NoError(t, err)
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := r.WatchActiveChanges(watchCtx, observed.Revision+1)
	require.NoError(t, err)
	batch := nextActiveChange(t, ch, func(b state.ActiveChangeBatch) bool { return len(b.Changes) > 0 })
	require.True(t, batch.Changes[0].RecordChanged)
}

func TestActiveChangesCompactionRequestsReset(t *testing.T) {
	s, r, _ := sharedDataFixture(t)
	observed, err := r.ObserveOperations(context.Background(), "shared-sandbox")
	require.NoError(t, err)
	_, err = s.client.Compact(context.Background(), observed.Revision)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := r.WatchActiveChanges(ctx, 1)
	require.NoError(t, err)
	batch := nextActiveChange(t, ch, func(b state.ActiveChangeBatch) bool { return b.Reset })
	require.True(t, batch.Reset)
}
