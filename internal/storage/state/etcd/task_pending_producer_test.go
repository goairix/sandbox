package etcd

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// A missing or separately written pending row loses committed tasks when the
// creator disappears. These tests exercise the real producer against the
// bounded host gRPC model; they do not claim native-etcd or physical evidence.
func TestCleanupPendingProducerAtomic(t *testing.T) {
	assertPayload := func(t *testing.T, ref TaskReference, task, pending []byte) {
		t.Helper()
		reference, err := json.Marshal(ref)
		require.NoError(t, err)
		digest := sha256.Sum256(task)
		want := fmt.Sprintf(`{"version":1,"reference":%s,"task_digest":"%x"}`, reference, digest)
		require.Equal(t, want, string(pending), "pending discovery binds canonical immutable Task bytes")
		require.LessOrEqual(t, len(pending), 2048)
	}

	for _, scenario := range []string{"due-committed", "due-committed-ack-lost", "due-rejected-cas"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDueExpiryFixture(t)
			f.earnClaim()
			before := make([]*mvccpb.KeyValue, len(f.keys))
			for i, key := range f.keys {
				before[i] = f.server.point(key)
			}
			dueKey, err := f.b.namespace.dueCandidateKey(f.candidate)
			require.NoError(t, err)
			dueBefore := f.server.point(dueKey)

			switch scenario {
			case "due-committed-ack-lost":
				// Drop only the acknowledgement after the real producer's Txn
				// commits in the model. No replacement Task is submitted.
				f.b.client.KV = &faultKV{KV: f.b.client.KV, match: putsKey(f.keys[1]), after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
					require.NoError(t, err)
					require.True(t, response.Succeeded)
					return nil, context.DeadlineExceeded
				}}
			case "due-rejected-cas":
				f.b.client.KV = &faultKV{KV: f.b.client.KV, match: putsKey(f.keys[1]), before: func() {
					f.server.put(f.keys[1], string(before[1].Value), 0)
				}}
			}

			result, err := f.admit()
			require.NotEmpty(t, result.Task.TaskID)
			pendingKey := fmt.Sprintf("%sp/%02x/cleanup-pending/%s", f.b.namespace.Root(), result.Task.Partition, result.Task.TaskID)
			for _, i := range []int{0, 2, 3, 4} {
				require.Equal(t, before[i], f.server.point(f.keys[i]), "pending creation must preserve original authority %d", i)
			}

			if scenario == "due-rejected-cas" {
				require.ErrorIs(t, err, ErrConflict)
				require.Equal(t, OutcomeUnknown, result.Outcome)
				require.Equal(t, before[1].Value, f.server.point(f.keys[1]).Value)
				require.Equal(t, dueBefore, f.server.point(dueKey))
				for _, key := range append(taskDestroyKeys(t, f.b, result.Task), pendingKey) {
					require.Nil(t, f.server.point(key), "rejected CAS cannot publish any Task or pending row")
				}
				return
			}
			if scenario == "due-committed-ack-lost" {
				require.ErrorIs(t, err, ErrOutcomeUnknown)
				require.Equal(t, OutcomeUnknown, result.Outcome)
				outcome, resolveErr := f.b.ResolveStage(context.Background(), result.Reference)
				require.NoError(t, resolveErr)
				require.Equal(t, OutcomeCommitted, outcome)
			} else {
				require.NoError(t, err)
				require.Equal(t, OutcomeCommitted, result.Outcome)
			}

			control := f.server.point(f.keys[1])
			require.Greater(t, control.ModRevision, before[1].ModRevision)
			require.Nil(t, f.server.point(dueKey))
			_, receiptKey, keyErr := f.b.stageKeys(result.Reference)
			require.NoError(t, keyErr)
			keys := taskDestroyKeys(t, f.b, result.Task)
			for _, key := range append(keys, receiptKey) {
				row := f.server.point(key)
				require.NotNil(t, row)
				require.Equal(t, control.ModRevision, row.CreateRevision)
				require.Equal(t, row.CreateRevision, row.ModRevision)
				require.Zero(t, row.Lease)
			}
			pending := f.server.point(pendingKey)
			require.NotNil(t, pending, "committed Task is missing its atomic permanent pending row")
			require.Equal(t, control.ModRevision, pending.CreateRevision)
			require.Equal(t, pending.CreateRevision, pending.ModRevision)
			require.EqualValues(t, 1, pending.Version)
			require.Zero(t, pending.Lease)
			assertPayload(t, result.Task, f.server.point(keys[0]).Value, pending.Value)
		})
	}

	t.Run("manual-destroy-builder", func(t *testing.T) {
		task, _, _, checkpoint := taskTestRecords()
		namespace, err := NewNamespace("/codex-test", "tasks", "cell")
		require.NoError(t, err)
		control := SandboxControlRecord{Version: 1, SandboxID: task.Reference.SandboxID, WorkspaceHash: task.WorkspaceHash, IntentID: task.CreationIntentID, RestoreEpoch: task.Reference.RestoreEpoch, Generation: task.Generation, DataGateEpoch: task.DataGateEpoch, Phase: PhaseActive, Snapshot: task.Snapshot, Runtime: &task.Runtime, ExpiresAt: task.ExpiresAt}
		bundle := &operationControlBundle{Control: control, Partition: task.Reference.Partition}
		for i := 0; i < 5; i++ {
			key := fmt.Sprintf("%sp/ab/original/point-%d", namespace.Root(), i)
			bundle.Keys = append(bundle.Keys, key)
			bundle.KVs = append(bundle.KVs, &mvccpb.KeyValue{Key: []byte(key), Value: []byte(fmt.Sprintf("original-%d", i)), CreateRevision: int64(i + 1), ModRevision: int64(i + 10)})
		}

		mutation, err := buildTaskDestroyMutation(namespace, bundle, task.Reference, checkpoint.Attempt)
		require.NoError(t, err)
		require.Equal(t, control, bundle.Control, "building pending must not mutate the observed active control")
		for i, row := range bundle.KVs {
			for _, want := range []clientv3.Cmp{
				clientv3.Compare(clientv3.Value(bundle.Keys[i]), "=", string(row.Value)),
				clientv3.Compare(clientv3.ModRevision(bundle.Keys[i]), "=", row.ModRevision),
				clientv3.Compare(clientv3.CreateRevision(bundle.Keys[i]), "=", row.CreateRevision),
				clientv3.Compare(clientv3.LeaseValue(bundle.Keys[i]), "=", 0),
			} {
				require.Contains(t, mutation.Comparisons, want, "pending cannot weaken an original authority fence")
			}
		}

		pendingKey := "/codex-test/tasks/cell/p/ab/cleanup-pending/8a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"
		taskKey := "/codex-test/tasks/cell/p/ab/tasks/8a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"
		var pending, taskWire []byte
		for _, write := range mutation.Writes {
			switch write.Key {
			case pendingKey:
				require.False(t, write.Delete)
				pending = write.Value
			case taskKey:
				taskWire = write.Value
			}
		}
		require.NotEmpty(t, pending, "manual destroy mutation is missing its atomic permanent pending row")
		require.NotEmpty(t, taskWire)
		require.Contains(t, mutation.Comparisons, clientv3.Compare(clientv3.CreateRevision(pendingKey), "=", 0))
		assertPayload(t, task.Reference, taskWire, pending)
		_, _, err = prepareMutation(namespace, mutation)
		require.NoError(t, err, "atomic pending must fit the existing Stage budget")
	})
}

// The due row's exact observed ModRevision must reject byte-identical ABA and
// lease attachment without weakening any of the five authoritative records.
func TestCleanupPendingDueABA(t *testing.T) {
	for _, scenario := range []string{"same-value-update", "delete-recreate", "lease-attach"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDueExpiryFixture(t)
			f.earnClaim()
			before := make([]*mvccpb.KeyValue, len(f.keys))
			for i, key := range f.keys {
				before[i] = f.server.point(key)
			}
			dueKey, err := f.b.namespace.dueCandidateKey(f.candidate)
			require.NoError(t, err)
			due := f.server.point(dueKey)
			original := f.b.client.KV
			f.b.client.KV = &faultKV{KV: original, match: putsKey(f.keys[1]), before: func() {
				lease := int64(0)
				if scenario == "delete-recreate" {
					response, err := original.Txn(context.Background()).Then(clientv3.OpDelete(dueKey)).Commit()
					require.NoError(t, err)
					require.EqualValues(t, 1, response.Responses[0].GetResponseDeleteRange().Deleted)
				}
				if scenario == "lease-attach" {
					lease = f.session.guard.lease
					require.Positive(t, lease)
				}
				f.server.put(dueKey, string(due.Value), lease)
			}}

			result, err := f.admit()
			require.ErrorIs(t, err, ErrConflict)
			require.Equal(t, OutcomeUnknown, result.Outcome)
			for i, key := range f.keys {
				require.Equal(t, before[i], f.server.point(key), "failed due CAS must preserve authority %d", i)
			}
			changed := f.server.point(dueKey)
			require.NotNil(t, changed)
			require.Equal(t, due.Value, changed.Value)
			require.Greater(t, changed.ModRevision, due.ModRevision)
			if scenario == "delete-recreate" {
				require.Greater(t, changed.CreateRevision, due.CreateRevision)
			}
			pendingKey := fmt.Sprintf("%sp/%02x/cleanup-pending/%s", f.b.namespace.Root(), result.Task.Partition, result.Task.TaskID)
			for _, key := range append(taskDestroyKeys(t, f.b, result.Task), pendingKey) {
				require.Nil(t, f.server.point(key), "stale due snapshot must not publish Task or pending")
			}
		})
	}
}
