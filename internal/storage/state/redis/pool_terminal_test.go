package redis

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

func TestPreparedReturnTerminalRequiresReplicaAck(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	ctx := context.Background()
	id := poolTestID("terminal-ack")
	key := fusePoolRecordPrefix + fusePoolDigest(id)
	t.Cleanup(func() { _ = s.client.Del(ctx, key).Err() })
	strong := NewFUSEPoolRepository(&Store{client: s.client, durability: DurabilityReplicaAck, ackReplicas: 99, ackTimeout: time.Millisecond})
	best := NewFUSEPoolRepository(s)
	for _, absent := range []bool{false, true} {
		var expected *state.FUSEPoolRecord
		if absent {
			require.NoError(t, s.client.Del(ctx, key).Err())
		} else {
			expected = &state.FUSEPoolRecord{PreparationID: id, RuntimeID: "runtime-a", RuntimeUID: "uid-a", PoolKey: "pool-key", State: state.FUSEPoolPrepared, Revision: 4}
			raw, err := json.Marshal(expected)
			require.NoError(t, err)
			require.NoError(t, s.client.Set(ctx, key, raw, 0).Err())
		}
		require.ErrorIs(t, strong.ConfirmReturnPreparedTerminal(ctx, id, expected), state.ErrDurabilityUnconfirmed)
		require.NoError(t, best.ConfirmReturnPreparedTerminal(ctx, id, expected))
	}
	require.NoError(t, s.client.Set(ctx, key, `{"preparation_id":"replacement"}`, 0).Err())
	require.ErrorIs(t, strong.ConfirmReturnPreparedTerminal(ctx, id, nil), state.ErrFUSEPoolConflict)
}
