package etcd

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// Pending is an immutable routing record, so accepting altered bytes, an old
// epoch or a replacement envelope would let discovery misattribute a Task.
func TestCleanupPendingRecords(t *testing.T) {
	namespace, err := NewNamespace("/codex-test", "tasks", "cell")
	require.NoError(t, err)
	task, _, _, _ := taskTestRecords()
	taskWire, err := encodeTaskRecord(task)
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(taskWire))
	record := cleanupPendingRecord{Version: 1, Reference: task.Reference, TaskDigest: fmt.Sprintf("%x", digest)}
	wire, err := encodeCleanupPending(record)
	require.NoError(t, err)
	reference, err := json.Marshal(task.Reference)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf(`{"version":1,"reference":%s,"task_digest":"%x"}`, reference, digest), wire)
	key, err := namespace.cleanupPendingKey(task.Reference)
	require.NoError(t, err)
	require.Equal(t, "/codex-test/tasks/cell/p/ab/cleanup-pending/8a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", key)
	kv := &mvccpb.KeyValue{Key: []byte(key), Value: []byte(wire), Version: 1, CreateRevision: 11, ModRevision: 11}
	got, err := decodeCleanupPending(namespace, "epoch", kv, 11)
	require.NoError(t, err)
	require.Equal(t, record, got)

	t.Run("strict-json", func(t *testing.T) {
		refuse := func(value []byte) {
			t.Helper()
			bad := workerTestKV(kv)
			bad.Value = value
			_, err := decodeCleanupPending(namespace, "epoch", bad, 11)
			require.ErrorIs(t, err, ErrCorruptRecord)
		}
		taskBadFields(t, []byte(wire), refuse, "pending")
		for _, value := range [][]byte{
			append([]byte(wire), ' '),
			append([]byte(wire), []byte(` {}`)...),
			append([]byte(wire), 0xff),
			[]byte(wire[:len(wire)-1]),
			[]byte("null"),
			bytes.Repeat([]byte("x"), 2049),
		} {
			refuse(value)
		}
	})
	for name, change := range map[string]func(*mvccpb.KeyValue){
		"leased":         func(row *mvccpb.KeyValue) { row.Lease = 7 },
		"negative-lease": func(row *mvccpb.KeyValue) { row.Lease = -1 },
		"zero-birth":     func(row *mvccpb.KeyValue) { row.CreateRevision = 0 },
		"changed-row":    func(row *mvccpb.KeyValue) { row.ModRevision = 12 },
		"wrong-version":  func(row *mvccpb.KeyValue) { row.Version = 2 },
		"zero-version":   func(row *mvccpb.KeyValue) { row.Version = 0 },
		"foreign-key":    func(row *mvccpb.KeyValue) { row.Key = []byte(strings.Replace(key, "/ab/", "/ac/", 1)) },
		"descendant":     func(row *mvccpb.KeyValue) { row.Key = append(row.Key, []byte("/claim")...) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := workerTestKV(kv)
			change(bad)
			_, err := decodeCleanupPending(namespace, "epoch", bad, 20)
			require.ErrorIs(t, err, ErrCorruptRecord)
		})
	}
	t.Run("view-and-identity", func(t *testing.T) {
		_, err := decodeCleanupPending(namespace, "epoch", nil, 11)
		require.ErrorIs(t, err, ErrCorruptRecord)
		_, err = decodeCleanupPending(namespace, "epoch", kv, 10)
		require.ErrorIs(t, err, ErrCorruptRecord)
		_, err = decodeCleanupPending(namespace, "replacement-epoch", kv, 11)
		require.ErrorIs(t, err, ErrCorruptRecord)
		foreign, err := NewNamespace("/codex-test", "tasks", "foreign")
		require.NoError(t, err)
		_, err = decodeCleanupPending(foreign, "epoch", kv, 11)
		require.ErrorIs(t, err, ErrCorruptRecord)
		_, err = foreign.cleanupPendingKey(record.Reference)
		require.Error(t, err)
	})
	t.Run("maximum-reference-and-invalid-encoding", func(t *testing.T) {
		maximum, err := NewNamespace("/"+strings.Repeat("p", 252), strings.Repeat("s", 128), strings.Repeat("c", 128))
		require.NoError(t, err)
		large := record
		large.Reference.Namespace = maximum.Root()
		large.Reference.RestoreEpoch = strings.Repeat("e", 128)
		large.Reference.SandboxID = strings.Repeat("s", 128)
		wire, err := encodeCleanupPending(large)
		require.NoError(t, err)
		require.LessOrEqual(t, len(wire), 2048)
		key, err := maximum.cleanupPendingKey(large.Reference)
		require.NoError(t, err)
		row := &mvccpb.KeyValue{Key: []byte(key), Value: []byte(wire), Version: 1, CreateRevision: 11, ModRevision: 11}
		got, err := decodeCleanupPending(maximum, large.Reference.RestoreEpoch, row, 11)
		require.NoError(t, err)
		require.Equal(t, large, got)
		for _, bad := range []cleanupPendingRecord{
			{Version: 2, Reference: record.Reference, TaskDigest: record.TaskDigest},
			{Version: 1, Reference: record.Reference, TaskDigest: strings.Repeat("A", 64)},
			{Version: 1, Reference: record.Reference, TaskDigest: "short"},
			{Version: 1, TaskDigest: record.TaskDigest},
		} {
			_, err := encodeCleanupPending(bad)
			require.Error(t, err)
		}
	})
}
