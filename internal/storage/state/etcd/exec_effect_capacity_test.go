package etcd

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type execEffectCounts struct{ Txns, Points, Puts, Grants, Keeps, Revokes int64 }
type execEffectCountKV struct {
	clientv3.KV
	t                         *testing.T
	b                         *Backend
	cap                       *OperationCapability
	txns, points, puts        int64
	commitBudget, fenceBudget int
}
type execEffectCountTxn struct {
	clientv3.Txn
	owner           *execEffectCountKV
	comparisons     []clientv3.Cmp
	then, otherwise []clientv3.Op
}

func (kv *execEffectCountKV) Txn(ctx context.Context) clientv3.Txn {
	kv.txns++
	deadline, ok := ctx.Deadline()
	require.True(kv.t, ok)
	require.False(kv.t, deadline.After(kv.cap.execDraft.deadline))
	return &execEffectCountTxn{Txn: kv.KV.Txn(ctx), owner: kv}
}
func (tx *execEffectCountTxn) If(c ...clientv3.Cmp) clientv3.Txn {
	tx.comparisons = c
	tx.Txn = tx.Txn.If(c...)
	return tx
}
func (tx *execEffectCountTxn) Then(o ...clientv3.Op) clientv3.Txn {
	tx.then = o
	tx.Txn = tx.Txn.Then(o...)
	return tx
}
func (tx *execEffectCountTxn) Else(o ...clientv3.Op) clientv3.Txn {
	tx.otherwise = o
	tx.Txn = tx.Txn.Else(o...)
	return tx
}
func (tx *execEffectCountTxn) Commit() (*clientv3.TxnResponse, error) {
	r, err := tx.Txn.Commit()
	if err != nil || r == nil {
		return r, err
	}
	ops := tx.then
	if !r.Succeeded {
		ops = tx.otherwise
	}
	for _, op := range ops {
		if op.IsGet() {
			tx.owner.points++
			require.Empty(tx.owner.t, op.RangeBytes())
			require.False(tx.owner.t, op.IsSerializable())
		} else if op.IsPut() {
			tx.owner.puts++
		} else {
			tx.owner.t.Fatal("unexpected exec effect operation")
		}
	}
	if len(tx.comparisons) == 26 {
		tx.owner.fenceBudget = 26
		for _, branch := range [][]clientv3.Op{tx.then, tx.otherwise} {
			require.Len(tx.owner.t, branch, 1)
			require.True(tx.owner.t, branch[0].IsGet())
			require.Equal(tx.owner.t, tx.owner.b.identityKey, string(branch[0].KeyBytes()))
			require.False(tx.owner.t, branch[0].IsSerializable())
		}
	}
	if d := tx.owner.cap.execDraft; d != nil && d.stage != nil {
		effectKey, e := tx.owner.b.namespace.execEffectKey(tx.owner.cap.Reference())
		require.NoError(tx.owner.t, e)
		if putsKey(effectKey)(tx.then) {
			tx.owner.commitBudget = len(d.stage.mutation.Comparisons) + len(d.stage.mutation.Writes) + stageProtocolOperations
			require.Equal(tx.owner.t, 38, tx.owner.commitBudget)
			require.Len(tx.owner.t, tx.comparisons, 31)
		}
	}
	return r, err
}
func (kv *execEffectCountKV) reset() { kv.txns = 0; kv.points = 0; kv.puts = 0 }
func (kv *execEffectCountKV) counts(l *creationFaultLease) execEffectCounts {
	return execEffectCounts{kv.txns, kv.points, kv.puts, l.grants.Load(), l.keeps.Load(), l.revokes.Load()}
}

// Both runs register the exact issuer before counting Prepare. Synthetic idle
// keys use the real namespace but create no workloads, Leases, watches or timers.
func TestExecEffectFixedCost(t *testing.T) {
	var baseline execEffectCounts
	for _, idle := range []int{0, 1000} {
		t.Run(fmt.Sprint(idle), func(t *testing.T) {
			f := newExecEffectFixture(t)
			b := f.b
			ctx := context.Background()
			_, err := b.RegisterExecIssuer(ctx)
			require.NoError(t, err)
			for start := 0; start < idle; start += 50 {
				ops := []clientv3.Op{}
				for i := start; i < start+50 && i < idle; i++ {
					ops = append(ops, clientv3.OpPut(fmt.Sprintf("%ssynthetic-idle/%04d", b.namespace.Root(), i), `{"idle":true}`))
				}
				_, err = f.raw.Txn(ctx).Then(ops...).Commit()
				require.NoError(t, err)
			}
			counter := &execEffectCountKV{KV: b.client.KV, t: t, b: b, cap: f.cap}
			b.client.KV = counter
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			result, err := b.PrepareExecEffect(ctx, f.cap, execEffectRequest())
			require.NoError(t, err)
			require.NotNil(t, result.Prepared)
			got := counter.counts(lease)
			require.Equal(t, execEffectCounts{Txns: 7, Points: 15, Puts: 3, Grants: 1, Revokes: 1}, got)
			if idle == 0 {
				baseline = got
			} else {
				require.Equal(t, baseline, got)
			}
			require.Equal(t, 38, counter.commitBudget)
			require.Equal(t, 26, counter.fenceBudget)
			d := f.cap.execDraft
			operation, err := encodeOperationRecord(f.cap.record)
			require.NoError(t, err)
			require.LessOrEqual(t, len(d.recordValue), 16384)
			require.LessOrEqual(t, len(d.ticket.Wire()), 4096)
			require.LessOrEqual(t, len(operation), 4096)
			require.NotContains(t, d.recordValue, "private-argument")
			require.NotContains(t, d.recordValue, "private-env")
			require.NotContains(t, d.recordValue, "private-input")
			t.Logf("idle=%d warm Prepare counts=%+v budgets Stage=%d postFence=%d bytes record=%d ticket=%d operation=%d descriptor=%d window=%s", idle, got, counter.commitBudget, counter.fenceBudget, len(d.recordValue), len(d.ticket.Wire()), len(operation), len(d.descriptor.Canonical()), d.claims.NotAfter.Sub(d.claims.NotBefore))
			counter.reset()
			lease.grants.Store(0)
			lease.keeps.Store(0)
			lease.revokes.Store(0)
			retry, err := b.PrepareExecEffect(ctx, f.cap, execEffectRequest())
			require.NoError(t, err)
			require.NotNil(t, retry.Prepared)
			require.Equal(t, execEffectCounts{Txns: 3, Points: 9}, counter.counts(lease))
			t.Logf("idle=%d committed retry counts=%+v", idle, counter.counts(lease))
		})
	}
}

type execEffectTrackedParent struct {
	done                chan struct{}
	registered, stopped int
}

func (p *execEffectTrackedParent) Deadline() (time.Time, bool) { return time.Time{}, false }
func (p *execEffectTrackedParent) Done() <-chan struct{}       { return p.done }
func (p *execEffectTrackedParent) Err() error                  { return nil }
func (p *execEffectTrackedParent) Value(any) any               { return nil }
func (p *execEffectTrackedParent) AfterFunc(func()) func() bool {
	p.registered++
	stopped := false
	return func() bool {
		if stopped {
			return false
		}
		stopped = true
		p.stopped++
		return true
	}
}
func TestPrepareExecEffectStopsParentCallbacks(t *testing.T) {
	f := newExecEffectFixture(t)
	parent := &execEffectTrackedParent{done: make(chan struct{})}
	f.cap.parentCtx = parent
	for _, bad := range []bool{false, true} {
		if bad {
			f.b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
				return ClockObservation{UTC: f.now, Uncertainty: 2 * time.Second}, nil
			})
		}
		result, err := f.b.PrepareExecEffect(context.Background(), f.cap, execEffectRequest())
		if bad {
			require.Error(t, err)
			require.Nil(t, result.Prepared)
		} else {
			require.NoError(t, err)
			require.NotNil(t, result.Prepared)
		}
		require.Positive(t, parent.registered)
		require.Equal(t, parent.registered, parent.stopped, "no parent callback may remain attached after return")
	}
}
