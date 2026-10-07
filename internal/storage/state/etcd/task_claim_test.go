package etcd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func taskClaimFixture(t *testing.T) (*Backend, *clientv3.Client, TaskReference, context.Context) {
	t.Helper()
	b, raw, in, _, ctx := taskDestroyFixture(t)
	s, r := taskDestroyPrepare(t, b, ctx, in)
	out, e := b.CommitStage(ctx, s)
	require.NoError(t, e)
	require.Equal(t, OutcomeCommitted, out)
	return b, raw, r, ctx
}

func TestTaskClaimCopies(t *testing.T) {
	t.Run("strict-record", func(t *testing.T) {
		task, _, _, _ := taskTestRecords()
		record := taskClaimRecord{Version: 1, Task: task.Reference, ClaimID: taskClaimTestID, WorkerID: "worker", LeaseID: 7}
		wire, e := encodeTaskClaimRecord(record)
		require.NoError(t, e)
		kv := &mvccpb.KeyValue{Value: []byte(wire), Lease: 7, CreateRevision: 11, ModRevision: 11}
		var got taskClaimRecord
		require.NoError(t, decodeTaskClaimRecord(kv, &got))
		require.Equal(t, record, got)
		reject := func(wire []byte) {
			bad := *kv
			bad.Value = wire
			dst := record
			require.ErrorIs(t, decodeTaskClaimRecord(&bad, &dst), ErrCorruptRecord)
			require.Equal(t, record, dst)
		}
		taskBadFields(t, kv.Value, reject, "")
		reject(append(bytes.Clone(kv.Value), 0xff))
		reject(append(bytes.Clone(kv.Value), []byte(" {}")...))
		for _, modify := range []func(*mvccpb.KeyValue){func(k *mvccpb.KeyValue) { k.Lease = 0 }, func(k *mvccpb.KeyValue) { k.Lease = 8 }, func(k *mvccpb.KeyValue) { k.ModRevision++ }, func(k *mvccpb.KeyValue) { k.CreateRevision = 0 }} {
			bad := *kv
			modify(&bad)
			dst := record
			require.ErrorIs(t, decodeTaskClaimRecord(&bad, &dst), ErrCorruptRecord)
			require.Equal(t, record, dst)
		}
		padded := *kv
		padded.Value = append(bytes.Clone(kv.Value), bytes.Repeat([]byte(" "), 4096-len(kv.Value))...)
		require.NoError(t, decodeTaskClaimRecord(&padded, &got))
		reject(append(padded.Value, ' '))
		clear(kv.Value)
		require.Equal(t, record, got)
	})
}

func TestTaskClaimLifecycle(t *testing.T) {
	t.Run("coherent-bundle", func(t *testing.T) {
		b, raw, r, ctx := taskClaimFixture(t)
		bundle, e := b.loadTaskClaimBundle(ctx, r)
		require.NoError(t, e)
		require.Len(t, bundle.fences, 8)
		require.Equal(t, r, bundle.task.Reference)
		require.Equal(t, bundle.birth, bundle.checkpoint.CreateRevision)
		require.Equal(t, bundle.birth, bundle.fences[4].mod)
		require.Greater(t, bundle.birth, bundle.task.ControlRevision)
		// New claim setup must reject an existing fixed claim without adopting it.
		key, e := b.namespace.taskClaimKey(r)
		require.NoError(t, e)
		_, e = raw.Put(ctx, key, "existing")
		require.NoError(t, e)
		denied, e := b.loadTaskClaimBundle(ctx, r)
		require.ErrorIs(t, e, ErrConflict)
		require.Nil(t, denied)
	})
}
