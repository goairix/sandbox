package etcd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

const taskTestID = "8a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"
const taskClaimTestID = "9a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"

func taskTestRecords() (TaskRecord, CleanupIntentRecord, TaskLinkRecord, TaskCheckpointRecord) {
	ref := TaskReference{Namespace: "/codex-test/tasks/cell/", RestoreEpoch: "epoch", TaskID: taskTestID, SandboxID: "sandbox", Partition: 0xab}
	task := TaskRecord{Version: 1, Reference: ref, Kind: "cleanup", WorkspaceHash: "ab" + strings.Repeat("1", 62), CreationIntentID: "creation", Generation: 2, DataGateEpoch: 3, ControlRevision: 4, Runtime: RuntimeReference{ID: "runtime", UID: "uid", BootID: "boot"}, Snapshot: SnapshotReference{Version: "v1", Digest: strings.Repeat("a", 64)}, ExpiresAt: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)}
	attempt := StageAttemptLocator{Namespace: ref.Namespace, RestoreEpoch: ref.RestoreEpoch, Partition: ref.Partition, RequestID: "request", StageID: "cleanup", AttemptID: taskClaimTestID}
	return task, CleanupIntentRecord{Version: 1, Task: task, Attempt: attempt}, TaskLinkRecord{Version: 1, Reference: ref}, TaskCheckpointRecord{Version: 1, Reference: ref, State: TaskCheckpointPending, Attempt: attempt}
}

// For every required field, including empty checkpoint scalars and every nested
// object, preserve the real codec's atomic destination contract on malformed JSON.
func taskBadFields(t *testing.T, wire []byte, check func([]byte), path string) {
	t.Helper()
	var object map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wire, &object))
	check(append([]byte(`{"unknown":1,`), wire[1:]...))
	for field, value := range object {
		for _, kind := range []string{"missing", "null", "duplicate"} {
			changed := make(map[string]json.RawMessage, len(object))
			for k, v := range object {
				changed[k] = v
			}
			switch kind {
			case "missing":
				delete(changed, field)
			case "null":
				changed[field] = json.RawMessage("null")
			}
			bad, err := json.Marshal(changed)
			require.NoError(t, err)
			if kind == "duplicate" {
				name, _ := json.Marshal(field)
				bad = append(append(append([]byte("{"), name...), append([]byte(":"), value...)...), append([]byte(","), bad[1:]...)...)
			}
			check(bad)
		}
		if len(value) > 0 && value[0] == '{' {
			taskBadFields(t, value, func(nested []byte) {
				changed := make(map[string]json.RawMessage, len(object))
				for k, v := range object {
					changed[k] = v
				}
				changed[field] = nested
				bad, err := json.Marshal(changed)
				require.NoError(t, err)
				check(bad)
			}, path+"/"+field)
		}
	}
}
func taskCodecChecks[T interface{ Validate() error }](t *testing.T, value T, encode func(T) (string, error), decode func(*mvccpb.KeyValue, *T) error, limit int, mutable bool) {
	t.Helper()
	wire, err := encode(value)
	require.NoError(t, err)
	kv := &mvccpb.KeyValue{Value: []byte(wire), CreateRevision: 11, ModRevision: 11}
	var got T
	require.NoError(t, decode(kv, &got))
	require.Equal(t, value, got)
	badWire := func(wire []byte) {
		t.Helper()
		dst := value
		bad := *kv
		bad.Value = wire
		require.ErrorIs(t, decode(&bad, &dst), ErrCorruptRecord, "wire %s", wire)
		require.Equal(t, value, dst)
	}
	taskBadFields(t, kv.Value, badWire, "")
	badWire(append([]byte(`{"unknown":1,`), kv.Value[1:]...))
	badWire(append(bytes.Clone(kv.Value), []byte(` {}`)...))
	badWire(append(bytes.Clone(kv.Value), 0xff))
	badWire(kv.Value[:len(kv.Value)-1])
	badWire([]byte("null"))
	exact := append(bytes.Clone(kv.Value), bytes.Repeat([]byte(" "), limit-len(kv.Value))...)
	exactKV := *kv
	exactKV.Value = exact
	require.NoError(t, decode(&exactKV, &got))
	badWire(append(exact, ' '))
	for _, metadata := range []struct{ lease, create, mod int64 }{{1, 11, 11}, {-1, 11, 11}, {0, 0, 0}, {0, -1, -1}, {0, 11, 10}} {
		bad := *kv
		bad.Lease, bad.CreateRevision, bad.ModRevision = metadata.lease, metadata.create, metadata.mod
		dst := value
		require.ErrorIs(t, decode(&bad, &dst), ErrCorruptRecord)
		require.Equal(t, value, dst)
	}
	later := *kv
	later.ModRevision++
	if mutable {
		require.NoError(t, decode(&later, &got))
	} else {
		dst := value
		require.ErrorIs(t, decode(&later, &dst), ErrCorruptRecord)
		require.Equal(t, value, dst)
	}
	require.ErrorIs(t, decode(nil, &got), ErrCorruptRecord)
	require.ErrorIs(t, decode(kv, nil), ErrCorruptRecord)
	clear(kv.Value)
	require.Equal(t, value, got, "decoded value retained input backing storage")
}
func TestTaskRecordsStrict(t *testing.T) {
	task, intent, link, checkpoint := taskTestRecords()
	invalidUTF8 := task
	invalidUTF8.Runtime.BootID = string([]byte{0xff})
	_, err := encodeTaskRecord(invalidUTF8)
	require.ErrorIs(t, err, ErrInvalidRecord)
	t.Run("task", func(t *testing.T) { taskCodecChecks(t, task, encodeTaskRecord, decodeTaskRecord, 4096, false) })
	t.Run("intent", func(t *testing.T) {
		taskCodecChecks(t, intent, encodeCleanupIntentRecord, decodeCleanupIntentRecord, 4096, false)
	})
	t.Run("link", func(t *testing.T) { taskCodecChecks(t, link, encodeTaskLinkRecord, decodeTaskLinkRecord, 2048, false) })
	t.Run("checkpoint", func(t *testing.T) {
		taskCodecChecks(t, checkpoint, encodeTaskCheckpointRecord, decodeTaskCheckpointRecord, 4096, true)
	})
	t.Run("binding", func(t *testing.T) {
		for _, mutate := range []func(*TaskRecord){
			func(r *TaskRecord) { r.Version = 2 }, func(r *TaskRecord) { r.Kind = "destroy" }, func(r *TaskRecord) { r.Reference.TaskID = strings.ToUpper(taskTestID) }, func(r *TaskRecord) { r.Reference.TaskID = "00000000-0000-0000-0000-000000000000" }, func(r *TaskRecord) { r.Reference.TaskID = "bad" }, func(r *TaskRecord) { r.Reference.Namespace = "/x/../cell/" }, func(r *TaskRecord) { r.Reference.Namespace = strings.TrimSuffix(r.Reference.Namespace, "/") }, func(r *TaskRecord) { r.Reference.RestoreEpoch = "../epoch" }, func(r *TaskRecord) { r.Reference.SandboxID = "" }, func(r *TaskRecord) { r.Reference.Partition++ }, func(r *TaskRecord) { r.WorkspaceHash = "bad" }, func(r *TaskRecord) { r.CreationIntentID = "" }, func(r *TaskRecord) { r.Generation = 0 }, func(r *TaskRecord) { r.DataGateEpoch = 0 }, func(r *TaskRecord) { r.ControlRevision = 0 }, func(r *TaskRecord) { r.Runtime.UID = "" }, func(r *TaskRecord) { r.Runtime.BootID = "\n" }, func(r *TaskRecord) { r.Snapshot.Digest = "bad" }, func(r *TaskRecord) { r.ExpiresAt = time.Time{} },
		} {
			bad := task
			mutate(&bad)
			require.ErrorIs(t, bad.Validate(), ErrInvalidRecord)
			wire, err := json.Marshal(bad)
			require.NoError(t, err)
			dst := task
			require.ErrorIs(t, decodeTaskRecord(&mvccpb.KeyValue{Value: wire, CreateRevision: 1, ModRevision: 1}, &dst), ErrCorruptRecord)
			require.Equal(t, task, dst)
			_, err = encodeTaskRecord(bad)
			require.ErrorIs(t, err, ErrInvalidRecord)
		}
		for _, mutate := range []func(*StageAttemptLocator){func(a *StageAttemptLocator) { a.Namespace = "/codex-test/other/cell/" }, func(a *StageAttemptLocator) { a.RestoreEpoch = "other" }, func(a *StageAttemptLocator) { a.Partition++ }, func(a *StageAttemptLocator) { a.AttemptID = "bad" }} {
			i := intent
			c := checkpoint
			mutate(&i.Attempt)
			mutate(&c.Attempt)
			require.ErrorIs(t, i.Validate(), ErrInvalidRecord)
			require.ErrorIs(t, c.Validate(), ErrInvalidRecord)
		}
		for _, state := range []TaskCheckpointState{TaskCheckpointPending, TaskCheckpointNeedsReconciliation} {
			c := checkpoint
			c.State = state
			c.ClaimID = taskClaimTestID
			c.DetailDigest = strings.Repeat("b", 64)
			require.NoError(t, c.Validate())
			c.ClaimID = ""
			require.ErrorIs(t, c.Validate(), ErrInvalidRecord)
			c.ClaimID = taskClaimTestID
			c.DetailDigest = ""
			require.ErrorIs(t, c.Validate(), ErrInvalidRecord)
		}
		checkpoint.State = TaskCheckpointNeedsReconciliation
		require.ErrorIs(t, checkpoint.Validate(), ErrInvalidRecord)
		checkpoint.State = "completed"
		require.ErrorIs(t, checkpoint.Validate(), ErrInvalidRecord)
	})
}
func TestTaskKeys(t *testing.T) {
	task, _, _, _ := taskTestRecords()
	n, err := NewNamespace("/codex-test", "tasks", "cell")
	require.NoError(t, err)
	for _, test := range []struct {
		key    func(TaskReference) (string, error)
		suffix string
	}{{n.taskKey, "tasks/" + taskTestID}, {n.cleanupIntentKey, "cleanup-intents/" + taskTestID}, {n.taskLinkKey, "sandboxes/sandbox/cleanup-task"}, {n.taskClaimKey, "tasks/" + taskTestID + "/claim"}, {n.taskCheckpointKey, "tasks/" + taskTestID + "/checkpoint"}, {func(r TaskReference) (string, error) { return n.taskGuardKey(r, taskClaimTestID) }, "tasks/" + taskTestID + "/guards/" + taskClaimTestID}} {
		key, err := test.key(task.Reference)
		require.NoError(t, err)
		require.Equal(t, n.Root()+"p/ab/"+test.suffix, key)
		bad := task.Reference
		bad.Namespace = "/codex-test/other/cell/"
		_, err = test.key(bad)
		require.ErrorIs(t, err, ErrIdentityMismatch)
		bad = task.Reference
		bad.TaskID = "../task"
		_, err = test.key(bad)
		require.ErrorIs(t, err, ErrInvalidRecord)
	}
	_, err = n.taskGuardKey(task.Reference, "00000000-0000-0000-0000-000000000000")
	require.ErrorIs(t, err, ErrInvalidRecord)
}
