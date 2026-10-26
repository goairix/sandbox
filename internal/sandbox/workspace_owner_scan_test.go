package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
)

type ownerScanEntry struct {
	key   string
	value []byte
}

type ownerScanOnlyStore struct {
	state.AtomicStore
	entries []ownerScanEntry
	visited int
}

func (*ownerScanOnlyStore) Keys(context.Context, string) ([]string, error) {
	return nil, errors.New("whole owner key list is forbidden")
}

func (*ownerScanOnlyStore) Get(context.Context, string) ([]byte, error) {
	return nil, errors.New("one owner RPC per record is forbidden")
}

func (s *ownerScanOnlyStore) VisitPrefixValues(_ context.Context, prefix string, visit func(string, []byte) (bool, error)) error {
	for _, entry := range s.entries {
		if len(entry.key) < len(prefix) || entry.key[:len(prefix)] != prefix {
			continue
		}
		s.visited++
		stop, err := visit(entry.key, entry.value)
		if err != nil || stop {
			return err
		}
	}
	return nil
}

func TestWorkspaceRuntimeOwnerUsesBoundedValueTraversal(t *testing.T) {
	store := &ownerScanOnlyStore{}
	for index := range 300 {
		owner := WorkspaceOwner{
			MountType: WorkspaceMountFUSE, Provider: "minio",
			StorageIdentityHash: storageIdentityHash("primary"), Bucket: "sandbox",
			Prefix:    fmt.Sprintf("workspaces/%03d/", index),
			SandboxID: fmt.Sprintf("sandbox-%03d", index), Runtime: "docker",
			RuntimeID: fmt.Sprintf("runtime-%03d", index), RuntimeUID: fmt.Sprintf("uid-%03d", index),
			Generation: 1, MountAttempt: 1, UpdatedAt: time.Now().UTC(),
		}
		keys, err := workspaceStateKeysFromOwner(owner)
		require.NoError(t, err)
		owner.WorkspaceHash = keys.workspaceHash
		raw, err := json.Marshal(owner)
		require.NoError(t, err)
		store.entries = append(store.entries, ownerScanEntry{key: keys.owner, value: raw})
	}
	coordinator := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	protected, err := coordinator.HasRuntimeOwner(context.Background(), "runtime-299", "uid-299")
	require.NoError(t, err)
	require.True(t, protected)
	require.Equal(t, 300, store.visited)
}

func TestWorkspaceRuntimeOwnerScanRejectsMalformedEntry(t *testing.T) {
	store := &ownerScanOnlyStore{entries: []ownerScanEntry{{
		key: workspaceOwnerKeyPrefix + "invalid", value: []byte(`{"runtime_id":"runtime"}`),
	}}}
	coordinator := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	protected, err := coordinator.HasRuntimeOwner(context.Background(), "runtime", "uid")
	require.False(t, protected)
	require.ErrorIs(t, err, ErrWorkspaceOwnerLost)
	require.Equal(t, 1, store.visited)
}
