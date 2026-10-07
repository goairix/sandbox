package etcd

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func taskDestroyFixture(t *testing.T) (*Backend, *clientv3.Client, BeginDestroyInput, []string, context.Context) {
	t.Helper()
	ownedFixtureContainers(t)
	b, raw, in, keys := operationControlFixture(t, "plain")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	got, err := raw.Get(ctx, keys[1])
	require.NoError(t, err)
	require.Len(t, got.Kvs, 1)
	return b, raw, BeginDestroyInput{SandboxID: in.SandboxID, RequestID: "destroy-request", ExpectedControlRevision: got.Kvs[0].ModRevision}, keys, ctx
}
func taskDestroyPrepare(t *testing.T, b *Backend, ctx context.Context, in BeginDestroyInput) (*Stage, TaskReference) {
	t.Helper()
	s, r, err := b.PrepareDestroy(ctx, in, 30*time.Second)
	require.NoError(t, err)
	require.NotNil(t, s)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, b.ReleaseStage(ctx, s))
	})
	return s, r
}
func taskDestroyKeys(t *testing.T, b *Backend, r TaskReference) []string {
	t.Helper()
	keys := make([]string, 0, 4)
	for _, f := range []func(TaskReference) (string, error){b.namespace.taskKey, b.namespace.cleanupIntentKey, b.namespace.taskLinkKey, b.namespace.taskCheckpointKey} {
		k, e := f(r)
		require.NoError(t, e)
		keys = append(keys, k)
	}
	return keys
}
func taskDestroyAssertAbsent(t *testing.T, ctx context.Context, raw *clientv3.Client, keys []string) {
	t.Helper()
	for _, key := range keys {
		got, e := raw.Get(ctx, key)
		require.NoError(t, e)
		require.Empty(t, got.Kvs, key)
	}
}
func taskDestroyAssertBirth(t *testing.T, b *Backend, ctx context.Context, s *Stage, r TaskReference, prior *operationControlBundle) {
	t.Helper()
	keys := append([]string{prior.Keys[1]}, taskDestroyKeys(t, b, r)...)
	keys = append(keys, s.receiptKey)
	got, e := b.readDomain(ctx, keys...)
	require.NoError(t, e)
	for _, kv := range got {
		require.NotNil(t, kv)
		require.Zero(t, kv.Lease)
		require.Equal(t, got[0].ModRevision, kv.ModRevision)
	}
	require.Equal(t, prior.KVs[1].CreateRevision, got[0].CreateRevision)
	for _, kv := range got[1:] {
		require.Equal(t, got[0].ModRevision, kv.CreateRevision)
	}
	var control SandboxControlRecord
	require.NoError(t, decodeDomainRecord(got[0], &control))
	original := prior.Control
	original.Phase = PhaseDestroying
	require.Equal(t, original, control)
	var task TaskRecord
	require.NoError(t, decodeTaskRecord(got[1], &task))
	require.Equal(t, r, task.Reference)
	require.Equal(t, prior.KVs[1].ModRevision, task.ControlRevision)
	require.Equal(t, *prior.Control.Runtime, task.Runtime)
	require.Equal(t, prior.Control.Snapshot, task.Snapshot)
	require.Equal(t, prior.Control.ExpiresAt, task.ExpiresAt)
	require.Equal(t, prior.Control.DataGateEpoch, task.DataGateEpoch)
	var intent CleanupIntentRecord
	require.NoError(t, decodeCleanupIntentRecord(got[2], &intent))
	require.Equal(t, task, intent.Task)
	var link TaskLinkRecord
	require.NoError(t, decodeTaskLinkRecord(got[3], &link))
	require.Equal(t, r, link.Reference)
	var cp TaskCheckpointRecord
	require.NoError(t, decodeTaskCheckpointRecord(got[4], &cp))
	require.Equal(t, r, cp.Reference)
	require.Equal(t, TaskCheckpointPending, cp.State)
	require.Empty(t, cp.ClaimID)
	require.Empty(t, cp.DetailDigest)
	require.Equal(t, intent.Attempt, cp.Attempt)
	require.Equal(t, s.Reference(), cp.Attempt.reference(s.Reference().Digest))
	outcome, e := decodeReceipt(got[5], s.Reference())
	require.NoError(t, e)
	require.Equal(t, OutcomeCommitted, outcome)
	after, e := b.readDomain(ctx, prior.Keys...)
	require.NoError(t, e)
	for _, i := range []int{0, 2, 3, 4} {
		require.Equal(t, prior.KVs[i], after[i], "permanent authority %d changed", i)
	}
}

func TestTaskDestroyAtomic(t *testing.T) {
	t.Run("mutation-shape", func(t *testing.T) {
		task, _, _, cp := taskTestRecords()
		n, e := NewNamespace("/codex-test", "tasks", "cell")
		require.NoError(t, e)
		control := SandboxControlRecord{Version: 1, SandboxID: task.Reference.SandboxID, WorkspaceHash: task.WorkspaceHash, IntentID: task.CreationIntentID, RestoreEpoch: task.Reference.RestoreEpoch, Generation: task.Generation, DataGateEpoch: task.DataGateEpoch, Phase: PhaseActive, Snapshot: task.Snapshot, Runtime: &task.Runtime, ExpiresAt: task.ExpiresAt}
		bundle := &operationControlBundle{Control: control, Partition: task.Reference.Partition}
		for i := 0; i < 5; i++ {
			key := n.Root() + fmt.Sprintf("p/ab/original/point-%d", i)
			bundle.Keys = append(bundle.Keys, key)
			bundle.KVs = append(bundle.KVs, &mvccpb.KeyValue{Key: []byte(key), Value: []byte(fmt.Sprintf("original-%d", i)), CreateRevision: int64(i + 1), ModRevision: int64(i + 10)})
		}
		mutation, e := buildTaskDestroyMutation(n, bundle, task.Reference, cp.Attempt)
		require.NoError(t, e)
		require.Len(t, mutation.Writes, 5)
		require.Len(t, mutation.Comparisons, 24)
		for i, kv := range bundle.KVs {
			for _, want := range []clientv3.Cmp{clientv3.Compare(clientv3.Value(bundle.Keys[i]), "=", string(kv.Value)), clientv3.Compare(clientv3.ModRevision(bundle.Keys[i]), "=", kv.ModRevision), clientv3.Compare(clientv3.CreateRevision(bundle.Keys[i]), "=", kv.CreateRevision), clientv3.Compare(clientv3.LeaseValue(bundle.Keys[i]), "=", 0)} {
				require.Contains(t, mutation.Comparisons, want)
			}
		}
		expectedKeys := []string{bundle.Keys[1]}
		for _, f := range []func(TaskReference) (string, error){n.taskKey, n.cleanupIntentKey, n.taskLinkKey, n.taskCheckpointKey} {
			k, e := f(task.Reference)
			require.NoError(t, e)
			expectedKeys = append(expectedKeys, k)
			require.Contains(t, mutation.Comparisons, clientv3.Compare(clientv3.CreateRevision(k), "=", 0))
		}
		for i, w := range mutation.Writes {
			require.Equal(t, expectedKeys[i], w.Key)
			require.False(t, w.Delete)
			require.NotEmpty(t, w.Value)
		}
		_, _, e = prepareMutation(n, mutation)
		require.NoError(t, e, "native Stage budgets remain unchanged")
		require.Equal(t, PhaseActive, bundle.Control.Phase)
		require.Equal(t, "uid", bundle.Control.Runtime.UID)
	})
	t.Run("permanent-atomic", func(t *testing.T) {
		b, raw, in, keys, ctx := taskDestroyFixture(t)
		prior, e := b.loadOperationControl(ctx, in.SandboxID)
		require.NoError(t, e)
		s, r := taskDestroyPrepare(t, b, ctx, in)
		taskDestroyAssertAbsent(t, ctx, raw, taskDestroyKeys(t, b, r))
		before, e := b.readDomain(ctx, keys...)
		require.NoError(t, e)
		require.Equal(t, prior.KVs, before, "preparation must not close admission")
		out, e := b.CommitStage(ctx, s)
		require.NoError(t, e)
		require.Equal(t, OutcomeCommitted, out)
		taskDestroyAssertBirth(t, b, ctx, s, r, prior)
		require.NoError(t, b.ReleaseStage(ctx, s))
		taskDestroyAssertBirth(t, b, ctx, s, r, prior)
	})
	t.Run("expired-manual", func(t *testing.T) {
		b, raw, in, keys, ctx := taskDestroyFixture(t)
		operationChangePoint(t, raw, keys[1], "expires_at", time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
		prior, e := b.loadOperationControl(ctx, in.SandboxID)
		require.NoError(t, e)
		in.ExpectedControlRevision = prior.KVs[1].ModRevision
		b.authorityClock = nil
		s, r := taskDestroyPrepare(t, b, ctx, in)
		out, e := b.CommitStage(ctx, s)
		require.NoError(t, e)
		require.Equal(t, OutcomeCommitted, out)
		taskDestroyAssertBirth(t, b, ctx, s, r, prior)
	})
	t.Run("preflight", func(t *testing.T) {
		b, _, in, _, ctx := taskDestroyFixture(t)
		lease := &creationFaultLease{Lease: b.client.Lease}
		b.client.Lease = lease
		for _, change := range []func(*BeginDestroyInput){func(i *BeginDestroyInput) { i.ExpectedControlRevision = 0 }, func(i *BeginDestroyInput) { i.ExpectedControlRevision++ }, func(i *BeginDestroyInput) { i.SandboxID = "../bad" }, func(i *BeginDestroyInput) { i.RequestID = "" }} {
			bad := in
			change(&bad)
			s, _, e := b.PrepareDestroy(ctx, bad, time.Second)
			require.Error(t, e)
			require.Nil(t, s)
		}
		for _, ttl := range []time.Duration{0, -time.Second, 24*time.Hour + 1} {
			s, _, e := b.PrepareDestroy(ctx, in, ttl)
			require.Error(t, e)
			require.Nil(t, s)
		}
		s, _, e := b.PrepareDestroy(nil, in, time.Second)
		require.Error(t, e)
		require.Nil(t, s)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		s, _, e = b.PrepareDestroy(canceled, in, time.Second)
		require.ErrorIs(t, e, context.Canceled)
		require.Nil(t, s)
		require.Zero(t, lease.grants.Load())
	})
	t.Run("competing", func(t *testing.T) {
		b, raw, in, _, ctx := taskDestroyFixture(t)
		a, ar := taskDestroyPrepare(t, b, ctx, in)
		c, cr := taskDestroyPrepare(t, b, ctx, in)
		require.NotEqual(t, ar.TaskID, cr.TaskID)
		out, e := b.CommitStage(ctx, a)
		require.NoError(t, e)
		require.Equal(t, OutcomeCommitted, out)
		out, e = b.CommitStage(ctx, c)
		require.ErrorIs(t, e, ErrConflict)
		require.Equal(t, OutcomeUnknown, out)
		other := taskDestroyKeys(t, b, cr)
		taskDestroyAssertAbsent(t, ctx, raw, []string{other[0], other[1], other[3]})
		value, e := b.readDomain(ctx, other[2])
		require.NoError(t, e)
		var link TaskLinkRecord
		require.NoError(t, decodeTaskLinkRecord(value[0], &link))
		require.Equal(t, ar, link.Reference)
		out, e = b.ResolveStage(ctx, c.Reference())
		require.NoError(t, e)
		require.Equal(t, OutcomeAborted, out)
	})
}

func TestTaskDestroyAdmissionOrder(t *testing.T) {
	for _, order := range []string{"begin-first", "destroy-first", "late-admit"} {
		t.Run(order, func(t *testing.T) {
			b, raw, in, _, ctx := taskDestroyFixture(t)
			s, _ := taskDestroyPrepare(t, b, ctx, in)
			commit := func() {
				out, e := b.CommitStage(ctx, s)
				require.NoError(t, e)
				require.Equal(t, OutcomeCommitted, out)
			}
			if order == "destroy-first" {
				commit()
			}
			if order == "late-admit" {
				b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
					for _, op := range ops {
						if op.IsPut() && strings.Contains(string(op.KeyBytes()), "/operations/") {
							return true
						}
					}
					return false
				}, before: commit}
			}
			op, e := b.BeginOperation(ctx, BeginOperationInput{SandboxID: in.SandboxID, RequestID: "original-operation", Kind: OperationData})
			if order == "begin-first" {
				require.NoError(t, e)
				require.NotNil(t, op.Capability)
				t.Cleanup(func() {
					c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_, e := raw.Revoke(c, clientv3.LeaseID(op.Reference.LeaseID))
					require.NoError(t, e)
				})
				token, guard, receipt, _, e := b.namespace.operationKeys(op.Reference)
				require.NoError(t, e)
				before, e := b.readDomain(ctx, token, guard, receipt)
				require.NoError(t, e)
				commit()
				after, e := b.readDomain(ctx, token, guard, receipt)
				require.NoError(t, e)
				require.Equal(t, before, after, "destroy cannot remove or mutate existing operations")
			} else {
				require.Error(t, e)
				require.Nil(t, op.Capability)
				if order == "late-admit" {
					require.Equal(t, OperationAborted, op.Outcome)
				}
			}
			later, e := b.BeginOperation(ctx, BeginOperationInput{SandboxID: in.SandboxID, RequestID: "after-destroy", Kind: OperationData})
			require.ErrorIs(t, e, ErrOperationAdmissionClosed)
			require.Nil(t, later.Capability)
		})
	}
}
