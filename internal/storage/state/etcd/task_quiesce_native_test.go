package etcd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestTaskQuiesceNativeMetadata(t *testing.T) {
	t.Run("success", quiescenceNativeSuccess)
	t.Run("unknown", quiescenceNativeUnknown)
	t.Run("postcommit", quiescenceNativePostcommit)
	t.Run("budget", quiescenceNativeBudget)
}
func TestTaskQuiesceNativeReceiptFence(t *testing.T) {
	for _, fault := range []string{"delete", "same-bytes", "alternate", "lease", "recreate", "restore"} {
		t.Run(fault, func(t *testing.T) {
			f := newQuiescenceNativeFixture(t)
			key, err := f.b.namespace.taskQuiescenceKey(f.c.reference.Task)
			require.NoError(t, err)
			_, receipt, err := f.b.stageKeys(f.old.Stage)
			require.NoError(t, err)
			original := f.b.client.KV
			defer func() { f.b.client.KV = original }()
			fired := false
			f.b.client.KV = &faultKV{KV: original, match: putsKey(key), before: func() {
				fired = true
				d := f.c.quiescenceDraft
				require.NotNil(t, d.prior)
				require.Positive(t, d.prior.receiptRevision)
				g, e := f.raw.Get(f.ctx, receipt)
				require.NoError(t, e)
				require.Len(t, g.Kvs, 1)
				value := string(g.Kvs[0].Value)
				switch fault {
				case "delete":
					_, e = f.raw.Delete(f.ctx, receipt)
				case "same-bytes":
					_, e = f.raw.Put(f.ctx, receipt, value)
				case "alternate":
					value, e = encodeReceipt(f.old.Stage, OutcomeAborted)
					require.NoError(t, e)
					_, e = f.raw.Put(f.ctx, receipt, value)
				case "lease":
					grant, x := f.raw.Grant(f.ctx, 20)
					require.NoError(t, x)
					t.Cleanup(func() { _, _ = f.raw.Revoke(context.Background(), grant.ID) })
					_, e = f.raw.Put(f.ctx, receipt, value, clientv3.WithLease(grant.ID))
				case "recreate":
					_, e = f.raw.Delete(f.ctx, receipt)
					require.NoError(t, e)
					_, e = f.raw.Put(f.ctx, receipt, value)
				case "restore":
					_, e = f.raw.Put(f.ctx, f.b.restoreKey, "different")
					deferRestore(t, f)
				}
				require.NoError(t, e)
			}}
			r, e := f.b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
			require.True(t, fired)
			require.Nil(t, r.Prepared)
			require.NotEqual(t, OutcomeCommitted, r.Outcome)
			if e == nil {
				require.Equal(t, OutcomeAborted, r.Outcome)
			}
			g, e := f.raw.Get(f.ctx, key)
			require.NoError(t, e)
			require.Empty(t, g.Kvs)
		})
	}
}
func deferRestore(t *testing.T, f *quiescenceNativeFixture) {
	t.Cleanup(func() {
		_, e := f.raw.Put(context.Background(), f.b.restoreKey, f.b.restoreEpoch)
		require.NoError(t, e)
	})
}

type quiescenceCaptureKV struct {
	clientv3.KV
	t       *testing.T
	b       *Backend
	c       *TaskClaim
	commits int
}
type quiescenceCaptureTxn struct {
	clientv3.Txn
	k       *quiescenceCaptureKV
	cmps    []clientv3.Cmp
	yes, no []clientv3.Op
}

func (k *quiescenceCaptureKV) Txn(ctx context.Context) clientv3.Txn {
	deadline, ok := ctx.Deadline()
	require.True(k.t, ok)
	require.False(k.t, deadline.After(k.c.quiescenceDraft.deadline))
	return &quiescenceCaptureTxn{Txn: k.KV.Txn(ctx), k: k}
}
func (t *quiescenceCaptureTxn) If(c ...clientv3.Cmp) clientv3.Txn {
	t.cmps = c
	t.Txn = t.Txn.If(c...)
	return t
}
func (t *quiescenceCaptureTxn) Then(o ...clientv3.Op) clientv3.Txn {
	t.yes = o
	t.Txn = t.Txn.Then(o...)
	return t
}
func (t *quiescenceCaptureTxn) Else(o ...clientv3.Op) clientv3.Txn {
	t.no = o
	t.Txn = t.Txn.Else(o...)
	return t
}
func (t *quiescenceCaptureTxn) Commit() (*clientv3.TxnResponse, error) {
	key, _ := t.k.b.namespace.taskQuiescenceKey(t.k.c.reference.Task)
	if putsKey(key)(t.yes) {
		t.k.commits++
		require.Len(t.k.t, t.cmps, 57)
		require.Len(t.k.t, t.yes, 2)
		require.Len(t.k.t, t.no, 4)
		require.Equal(t.k.t, 63, len(t.cmps)+len(t.yes)+len(t.no))
		require.Equal(t.k.t, t.k.c.comparisons(), t.cmps[8:48])
		require.Equal(t.k.t, 64, len(t.cmps[8:])+1+stageProtocolOperations)
	}
	return t.Txn.Commit()
}
func quiescenceNativeBudget(t *testing.T) {
	f := newQuiescenceNativeFixture(t)
	original := f.b.client.KV
	capture := &quiescenceCaptureKV{KV: original, t: t, b: f.b, c: f.c}
	f.b.client.KV = capture
	defer func() { f.b.client.KV = original }()
	r, e := f.b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
	require.NoError(t, e)
	require.NotNil(t, r.Prepared)
	require.Equal(t, 1, capture.commits)
	f.b.client.KV = original
	for _, kind := range []string{"65", "bytes"} {
		t.Run(kind, func(t *testing.T) {
			lease := &creationFaultLease{Lease: f.b.client.Lease}
			f.b.client.Lease = lease
			defer func() { f.b.client.Lease = lease.Lease }()
			d := *f.c.quiescenceDraft
			stage, e := f.b.beginStageWithBuilder(f.ctx, f.c.reference.Task.Partition, f.c.reference.ClaimID, "task_quiesce_users", time.Second, func(l StageAttemptLocator) (Mutation, error) {
				m, e := f.b.buildTaskQuiescence(f.c, &d, l)
				require.NoError(t, e)
				if kind == "65" {
					m.Comparisons = append(m.Comparisons, clientv3.Compare(clientv3.ModRevision(f.c.claimKey), "=", 0))
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
			require.Zero(t, lease.revokes.Load())
			require.Zero(t, lease.keeps.Load())
		})
	}
}
