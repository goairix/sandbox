package sandbox

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSessionReadOnlyChecksBothFormatsWithoutWriting(t *testing.T) {
	for _, prefix := range []string{sandboxSessionKeyPrefix, legacySandboxSessionKeyPrefix} {
		t.Run(prefix, func(t *testing.T) {
			_, store, _ := distributedSyncManagers(t)
			sessions := NewSessionStore(store, time.Hour)
			sb := Sandbox{ID: "audit-session", RuntimeID: "pod-a", CreatedAt: time.Now()}
			raw, err := json.Marshal(sb)
			require.NoError(t, err)
			require.NoError(t, store.Set(context.Background(), prefix+sb.ID, raw, 0))
			got, err := sessions.LoadReadOnly(context.Background(), sb.ID)
			require.NoError(t, err)
			require.Equal(t, sb.RuntimeID, got.RuntimeID)
			keys, err := store.Keys(context.Background(), "sandbox:*")
			require.NoError(t, err)
			require.Equal(t, []string{prefix + sb.ID}, keys)
			unchanged, err := store.Get(context.Background(), prefix+sb.ID)
			require.NoError(t, err)
			require.Equal(t, raw, unchanged)
		})
	}
}

func TestSessionReadOnlyDoesNotTreatUnconfiguredStoreAsAbsent(t *testing.T) {
	sessions := NewSessionStore(nil, time.Hour)
	_, err := sessions.LoadReadOnly(context.Background(), "audit-session")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrSandboxNotFound)
}
