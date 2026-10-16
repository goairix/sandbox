package etcd

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

func TestWorkerBudget(t *testing.T) {
	t.Run("strict-slot-key-record-envelope", func(t *testing.T) {
		b, server := workerTestBackend(t)
		session, err := b.OpenWorkerSession(context.Background(), "worker")
		require.NoError(t, err)
		_, err = b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetClaim, 99)
		require.NoError(t, err)
		key, _ := b.namespace.Key("workers", "budget", "claim", "63")
		kv := server.point(key)
		var original workerBudgetRecord
		require.NoError(t, decodeWorkerBudgetRecord(b.namespace, kv, &original))
		for name, mutate := range map[string]func(*mvccpb.KeyValue){
			"unknown-kind": func(k *mvccpb.KeyValue) {
				k.Value = bytes.Replace(k.Value, []byte(`"kind":"claim"`), []byte(`"kind":"other"`), 1)
			},
			"out-of-range-slot": func(k *mvccpb.KeyValue) {
				k.Value = bytes.Replace(k.Value, []byte(`"slot":99`), []byte(`"slot":100`), 1)
			},
			"slot-key": func(k *mvccpb.KeyValue) { k.Key = []byte(b.namespace.Root() + "workers/budget/claim/62") },
			"kind-key": func(k *mvccpb.KeyValue) { k.Key = []byte(b.namespace.Root() + "workers/budget/range/63") },
			"lease":    func(k *mvccpb.KeyValue) { k.Lease++ },
			"rewrite":  func(k *mvccpb.KeyValue) { k.ModRevision++ },
			"version":  func(k *mvccpb.KeyValue) { k.Version++ },
			"unknown": func(k *mvccpb.KeyValue) {
				k.Value = append(bytes.TrimSuffix(k.Value, []byte("}")), []byte(`,"extra":1}`)...)
			},
			"duplicate": func(k *mvccpb.KeyValue) {
				k.Value = append(bytes.TrimSuffix(k.Value, []byte("}")), []byte(`,"slot":99}`)...)
			},
			"noncanonical": func(k *mvccpb.KeyValue) { k.Value = append(k.Value, ' ') },
		} {
			t.Run(name, func(t *testing.T) {
				bad := workerTestKV(kv)
				mutate(bad)
				dst := original
				require.ErrorIs(t, decodeWorkerBudgetRecord(b.namespace, bad, &dst), ErrCorruptRecord)
				require.Equal(t, original, dst)
			})
		}
	})
	t.Run("duplicate-acquire-preserves-earned-fraction", func(t *testing.T) {
		b, _ := workerTestBackend(t)
		session, err := b.OpenWorkerSession(context.Background(), "worker")
		require.NoError(t, err)
		share, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetClaim, 0)
		require.NoError(t, err)
		// Private state installation avoids sleeping or exposing a production
		// test-clock option. The pure bucket tests verify earning separately.
		start := time.Unix(100, 0)
		b.archiveLife.mu.Lock()
		share.bucket = newWorkerBucket(start, 1, 1)
		spent := share.bucket.take(start.Add(500*time.Millisecond), 1)
		before := share.bucket
		b.archiveLife.mu.Unlock()
		require.False(t, spent)
		again, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetClaim, 0)
		require.NoError(t, err)
		require.Same(t, share, again)
		b.archiveLife.mu.Lock()
		after := share.bucket
		b.archiveLife.mu.Unlock()
		require.Equal(t, before, after, "duplicate acquisition must neither replace the bucket nor reset earned remainder")
	})
	t.Run("all-fixed-shares-one-original-Lease", func(t *testing.T) {
		b, server := workerTestBackend(t)
		session, err := b.OpenWorkerSession(context.Background(), "worker")
		require.NoError(t, err)
		for _, group := range []struct {
			kind  WorkerBudgetKind
			name  string
			count int
		}{
			{WorkerBudgetRelist, "relist", 4}, {WorkerBudgetRange, "range", 20}, {WorkerBudgetClaim, "claim", 100},
		} {
			for i := 0; i < group.count; i++ {
				share, err := b.AcquireWorkerBudget(context.Background(), session, group.kind, uint16(i))
				require.NoError(t, err)
				ref := share.Reference()
				require.Equal(t, session.Reference(), ref.Session)
				require.Equal(t, group.kind, ref.Kind)
				require.Equal(t, uint16(i), ref.Slot)
				key, _ := b.namespace.Key("workers", "budget", group.name, fmt.Sprintf("%02x", i))
				kv := server.point(key)
				require.NotNil(t, kv)
				require.Equal(t, session.Reference().LeaseID, kv.Lease)
				require.Equal(t, kv.CreateRevision, ref.CreateRevision)
				require.Equal(t, kv.CreateRevision, kv.ModRevision)
				require.LessOrEqual(t, len(kv.Value), 2048)
				again, err := b.AcquireWorkerBudget(context.Background(), session, group.kind, uint16(i))
				require.NoError(t, err)
				require.Same(t, share, again, "duplicate kind+slot retains the original object and bucket")
			}
			_, err := b.AcquireWorkerBudget(context.Background(), session, group.kind, uint16(group.count))
			require.ErrorIs(t, err, ErrInvalidRecord)
		}
		grants, _, _ := server.counts()
		require.Equal(t, 1, grants)
		require.NoError(t, b.ReleaseWorkerSession(context.Background(), session))
		for _, group := range []struct {
			name  string
			count int
		}{{"relist", 4}, {"range", 20}, {"claim", 100}} {
			for i := 0; i < group.count; i++ {
				key, _ := b.namespace.Key("workers", "budget", group.name, fmt.Sprintf("%02x", i))
				require.Nil(t, server.point(key))
			}
		}
	})
	t.Run("explicit-slot-one-CAS-and-terminal-release", func(t *testing.T) {
		b, server := workerTestBackend(t)
		first, err := b.OpenWorkerSession(context.Background(), "first")
		require.NoError(t, err)
		second, err := b.OpenWorkerSession(context.Background(), "second")
		require.NoError(t, err)
		share, err := b.AcquireWorkerBudget(context.Background(), first, WorkerBudgetRange, 7)
		require.NoError(t, err)
		server.mu.Lock()
		before := len(server.requests)
		server.mu.Unlock()
		_, err = b.AcquireWorkerBudget(context.Background(), second, WorkerBudgetRange, 7)
		require.ErrorIs(t, err, ErrConflict)
		server.mu.Lock()
		attempts := len(server.requests) - before
		server.mu.Unlock()
		require.Equal(t, 1, attempts, "an explicit occupied slot performs one CAS with no allocator loop")
		require.NoError(t, b.RenewWorkerSession(context.Background(), second))
		require.NoError(t, b.ReleaseWorkerBudget(context.Background(), share))
		_, err = b.AcquireWorkerBudget(context.Background(), first, WorkerBudgetRange, 7)
		require.Error(t, err, "released original kind+slot is a permanent local tombstone")
		successor, err := b.AcquireWorkerBudget(context.Background(), second, WorkerBudgetRange, 7)
		require.NoError(t, err)
		_ = b.ReleaseWorkerBudget(context.Background(), share)
		key, _ := b.namespace.Key("workers", "budget", "range", "07")
		require.Equal(t, successor.Reference().Session.LeaseID, server.point(key).Lease)
	})
	t.Run("invalid-copies-empty-buckets-never-authorize", func(t *testing.T) {
		b, server := workerTestBackend(t)
		session, err := b.OpenWorkerSession(context.Background(), "worker")
		require.NoError(t, err)
		share, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetClaim, 0)
		require.NoError(t, err)
		require.Error(t, b.ConsumeWorkerBudget(context.Background(), share, 1))
		require.ErrorIs(t, b.ConsumeWorkerBudget(context.Background(), share, 0), ErrInvalidRecord)
		require.ErrorIs(t, b.ConsumeWorkerBudget(context.Background(), share, 2), ErrInvalidRecord)
		copy := new(WorkerBudgetShare)
		reflect.ValueOf(copy).Elem().Set(reflect.ValueOf(share).Elem())
		require.ErrorIs(t, b.ConsumeWorkerBudget(context.Background(), copy, 1), ErrInvalidRecord)
		require.ErrorIs(t, b.ReleaseWorkerBudget(context.Background(), copy), ErrInvalidRecord)
		ref := share.Reference()
		diagnostic := ref
		diagnostic.Slot, diagnostic.Session.LeaseID = 99, 999
		require.Equal(t, ref, share.Reference())
		_, err = b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetKind(255), 0)
		require.ErrorIs(t, err, ErrInvalidRecord)
		_, err = b.AcquireWorkerBudget(nil, session, WorkerBudgetClaim, 1)
		require.ErrorIs(t, err, ErrInvalidRecord)
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		require.ErrorIs(t, b.ConsumeWorkerBudget(canceled, share, 1), context.Canceled)
		require.NoError(t, b.RenewWorkerSession(context.Background(), session), "token empty and pure invalid calls preserve the live session")
		grants, _, _ := server.counts()
		require.Equal(t, 1, grants)
	})
	t.Run("unknown-ACK-does-not-find-another-slot", func(t *testing.T) {
		b, server := workerTestBackend(t)
		session, err := b.OpenWorkerSession(context.Background(), "worker")
		require.NoError(t, err)
		key, _ := b.namespace.Key("workers", "budget", "claim", "00")
		server.txnReply = func(request *pb.TxnRequest, reply *pb.TxnResponse) {
			for _, op := range request.Success {
				if p := op.GetRequestPut(); p != nil && string(p.Key) == key {
					reply.Header = nil
				}
			}
		}
		share, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetClaim, 0)
		require.Nil(t, share)
		require.ErrorIs(t, err, ErrOutcomeUnknown)
		require.NotNil(t, server.point(key))
		server.txnReply = nil
		_, err = b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetClaim, 1)
		require.Error(t, err)
		other, _ := b.namespace.Key("workers", "budget", "claim", "01")
		require.Nil(t, server.point(other))
	})
}
