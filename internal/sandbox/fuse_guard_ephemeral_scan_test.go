package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
)

func TestFUSEGuardFindsEphemeralReferenceWithoutPointReads(t *testing.T) {
	store := &ownerScanOnlyStore{}
	for index := range 300 {
		record := validEphemeralRecord()
		record.SandboxID = fmt.Sprintf("sandbox-%03d", index)
		record.RuntimeID = fmt.Sprintf("runtime-%03d", index)
		record.RuntimeUID = fmt.Sprintf("uid-%03d", index)
		record.Owner.SandboxID = record.SandboxID
		record.Owner.RuntimeID = record.RuntimeID
		record.Owner.RuntimeUID = record.RuntimeUID
		raw, err := json.Marshal(record)
		require.NoError(t, err)
		store.entries = append(store.entries, ownerScanEntry{
			key: ephemeralLifecycleKeyPrefix + record.SandboxID, value: raw,
		})
	}
	m := &Manager{ephemeral: NewEphemeralLifecycleStore(store)}
	disposition, err := m.guardFUSEPoolRecord(context.Background(), state.FUSEPoolRecord{
		PreparationID: "preparation", RuntimeID: "runtime-299", RuntimeUID: "uid-299",
		State: state.FUSEPoolConsumed,
	})
	require.NoError(t, err)
	require.Equal(t, FUSEPoolProtected, disposition)
	require.Equal(t, 300, store.visited)
}

func TestFUSEGuardKeepsRuntimeWhenEphemeralScanIsCorrupt(t *testing.T) {
	store := &ownerScanOnlyStore{entries: []ownerScanEntry{{
		key: ephemeralLifecycleKeyPrefix + "broken", value: []byte(`{"runtime_uid":"uid"}`),
	}}}
	m := &Manager{ephemeral: NewEphemeralLifecycleStore(store)}
	disposition, err := m.guardFUSEPoolRecord(context.Background(), state.FUSEPoolRecord{
		PreparationID: "preparation", RuntimeID: "runtime", RuntimeUID: "uid",
		State: state.FUSEPoolConsumed,
	})
	require.Equal(t, FUSEPoolDispositionUnknown, disposition)
	require.ErrorIs(t, err, ErrEphemeralLifecycleInvalid)
}

func TestFUSEGuardKeepsOriginalEphemeralStorePath(t *testing.T) {
	store := newAtomicMemoryStore()
	ephemeral := NewEphemeralLifecycleStore(store)
	record := validEphemeralRecord()
	require.NoError(t, ephemeral.Create(context.Background(), record))
	m := &Manager{ephemeral: ephemeral}
	disposition, err := m.guardFUSEPoolRecord(context.Background(), state.FUSEPoolRecord{
		PreparationID: "preparation", RuntimeID: record.RuntimeID, RuntimeUID: record.RuntimeUID,
		State: state.FUSEPoolConsumed,
	})
	require.NoError(t, err)
	require.Equal(t, FUSEPoolProtected, disposition)
}
