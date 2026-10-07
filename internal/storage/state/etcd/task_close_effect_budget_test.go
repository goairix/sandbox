package etcd

import (
	"context"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"strings"
	"testing"
	"time"
)

type taskCloseCaptureKV struct {
	clientv3.KV
	t                 *testing.T
	b                 *Backend
	c                 *TaskClaim
	commits, barriers int
}
type taskCloseCaptureTxn struct {
	clientv3.Txn
	k       *taskCloseCaptureKV
	cmps    []clientv3.Cmp
	yes, no []clientv3.Op
}

func (k *taskCloseCaptureKV) Txn(ctx context.Context) clientv3.Txn {
	deadline, ok := ctx.Deadline()
	require.True(k.t, ok)
	require.False(k.t, deadline.After(k.c.closeDraft.deadline))
	return &taskCloseCaptureTxn{Txn: k.KV.Txn(ctx), k: k}
}
func (t *taskCloseCaptureTxn) If(c ...clientv3.Cmp) clientv3.Txn {
	t.cmps = c
	t.Txn = t.Txn.If(c...)
	return t
}
func (t *taskCloseCaptureTxn) Then(o ...clientv3.Op) clientv3.Txn {
	t.yes = o
	t.Txn = t.Txn.Then(o...)
	return t
}
func (t *taskCloseCaptureTxn) Else(o ...clientv3.Op) clientv3.Txn {
	t.no = o
	t.Txn = t.Txn.Else(o...)
	return t
}
func (t *taskCloseCaptureTxn) Commit() (*clientv3.TxnResponse, error) {
	key, _ := t.k.b.namespace.taskCloseDataKey(t.k.c.reference.Task)
	if putsKey(key)(t.yes) {
		t.k.commits++
		require.Len(t.k.t, t.cmps, 52)
		require.Len(t.k.t, t.yes, 2)
		require.Len(t.k.t, t.no, 4)
		require.Equal(t.k.t, 58, len(t.cmps)+len(t.yes)+len(t.no))
		require.Equal(t.k.t, t.k.c.comparisons(), t.cmps[8:48])
		require.Equal(t.k.t, 59, len(t.cmps[8:])+1+stageProtocolOperations)
	}
	if len(t.cmps) == 44 || len(t.cmps) == 47 || len(t.cmps) == 51 {
		t.k.barriers++
		for _, ops := range [][]clientv3.Op{t.yes, t.no} {
			require.Len(t.k.t, ops, 1)
			require.True(t.k.t, ops[0].IsGet())
			require.Equal(t.k.t, t.k.b.identityKey, string(ops[0].KeyBytes()))
			require.False(t.k.t, ops[0].IsSerializable())
			require.Empty(t.k.t, ops[0].RangeBytes())
		}
	}
	return t.Txn.Commit()
}
func TestTaskCloseNativeBudget(t *testing.T) {
	f := newTaskCloseFixture(t, false)
	raw := f.b.client.KV
	count := &taskCloseCaptureKV{KV: raw, t: t, b: f.b, c: f.c}
	f.b.client.KV = count
	defer func() { f.b.client.KV = raw }()
	r, e := f.b.PrepareTaskCloseData(f.ctx, f.c)
	require.NoError(t, e)
	require.NotNil(t, r.Prepared)
	require.Equal(t, 1, count.commits)
	require.Greater(t, count.barriers, 0)
	f.b.client.KV = raw
	for _, fault := range []string{"operations", "bytes"} {
		t.Run(fault, func(t *testing.T) {
			lease := &creationFaultLease{Lease: f.b.client.Lease}
			f.b.client.Lease = lease
			defer func() { f.b.client.Lease = lease.Lease }()
			d := *f.c.closeDraft
			stage, e := f.b.beginStageWithBuilder(f.ctx, f.c.reference.Task.Partition, f.c.reference.ClaimID, "task_close_data", time.Second, func(l StageAttemptLocator) (Mutation, error) {
				m, e := f.b.buildTaskClose(f.c, &d, l)
				require.NoError(t, e)
				if fault == "operations" {
					for i := 0; i < 6; i++ {
						m.Comparisons = append(m.Comparisons, clientv3.Compare(clientv3.CreateRevision(f.c.claimKey), "=", 0))
					}
				} else {
					for i := 0; i < 8; i++ {
						m.Comparisons[i*4] = clientv3.Compare(clientv3.Value(f.c.fences[i].key), "=", strings.Repeat("x", maxRecordBytes))
					}
				}
				return m, nil
			})
			require.ErrorIs(t, e, ErrInvalidMutation)
			require.Nil(t, stage)
			require.Zero(t, lease.grants.Load())
			require.Zero(t, lease.keeps.Load())
			require.Zero(t, lease.revokes.Load())
		})
	}
}
