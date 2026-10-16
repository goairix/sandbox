package etcd

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestCleanupManagedClaimBudget(t *testing.T) {
	f := newCleanupSlotTestFixture(t)
	state := f.occupy(t)
	lease := &creationFaultLease{Lease: f.b.client.Lease}
	f.b.client.Lease = lease
	jobCtx, cancelJob := context.WithCancel(f.ctx)
	defer cancelJob()

	f.turn(t, func(owner *workerDueOwner) {
		f.b.archiveLife.mu.Lock()
		currentOwner, currentCall, tokens := f.share.dueOwner, f.session.operation, f.share.bucket.tokens
		f.b.archiveLife.mu.Unlock()
		require.Same(t, owner, currentOwner)
		require.Same(t, owner.call, currentCall)
		require.Zero(t, tokens)

		// No positive host Grant is authorized by this fixture. In particular,
		// a slot and original Claim share cannot stand in for the complete old
		// physical-chain wire inventory or an original Linux worker boundary.
		claim, err := f.b.claimCleanupTask(owner, jobCtx, f.evidence.bundle, state, nil, "cleanup-job", 30*time.Second)
		require.ErrorIs(t, err, ErrInvalidConfiguration)
		require.Nil(t, claim)
		require.Zero(t, lease.grants.Load())
		require.NoError(t, jobCtx.Err())

		second, stop, err := f.b.beginExpiredTaskOwner(f.ctx, f.part, f.share)
		require.ErrorIs(t, err, ErrConflict)
		require.Nil(t, second)
		require.Nil(t, stop)
	})

	// Use the existing bucket's immutable instant to test nonrefund without
	// sleeps or an assumption that a loaded CI process returns within one sec.
	f.b.archiveLife.mu.Lock()
	tokens := f.share.bucket.tokens
	refunded := f.share.bucket.take(f.share.bucket.last, 1)
	currentCall, currentOwner := f.session.operation, f.share.dueOwner
	f.b.archiveLife.mu.Unlock()
	require.Zero(t, tokens)
	require.False(t, refunded)
	require.Nil(t, currentCall)
	require.Nil(t, currentOwner)
	require.NoError(t, jobCtx.Err(), "finishing the charged turn must not cancel the distinct job lifetime")
	require.Equal(t, state, f.observe(t), "profile refusal retains the original occupation")

	t.Run("finished-owner-cannot-be-reused", func(t *testing.T) {
		var finished *workerDueOwner
		f.turn(t, func(owner *workerDueOwner) { finished = owner })
		bundle, err := f.b.loadCleanupClaimBundle(finished, f.ref)
		require.Error(t, err)
		require.Nil(t, bundle)
		claim, err := f.b.claimCleanupTask(finished, jobCtx, f.evidence.bundle, state, nil, "cleanup-job", 30*time.Second)
		require.Error(t, err)
		require.Nil(t, claim)
		require.Zero(t, lease.grants.Load())
	})
}

func TestCleanupManagedClaimLifetime(t *testing.T) {
	t.Run("bundle-is-two-native-reads-on-original-call", func(t *testing.T) {
		f := newCleanupSlotTestFixture(t)
		var reads atomic.Int64
		native := f.b.client.KV
		f.b.client.KV = &faultKV{KV: native, match: func(ops []clientv3.Op) bool {
			for _, op := range ops {
				require.True(t, op.IsGet())
				require.Empty(t, op.RangeBytes())
			}
			reads.Add(1)
			return false
		}}
		f.turn(t, func(owner *workerDueOwner) {
			bundle, err := f.b.loadCleanupClaimBundle(owner, f.ref)
			require.NoError(t, err)
			require.Equal(t, f.ref, bundle.task.Reference)
			require.Len(t, bundle.fences, 8, "all original permanent records remain fenced")
			require.NoError(t, owner.postcheck())
		})
		require.Equal(t, int64(2), reads.Load(), "bundle preparation must not hide Grant or run a discovery scan")
	})

	t.Run("session-release-joins-actual-native-return", func(t *testing.T) {
		f := newCleanupSlotTestFixture(t)
		arrived, unblock := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(unblock) }) })
		native := f.b.client.KV
		var reads atomic.Int64
		f.b.client.KV = &faultKV{KV: native, match: func(ops []clientv3.Op) bool {
			return len(ops) > 0 && ops[0].IsGet() && reads.Add(1) == 1
		}, after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
			close(arrived)
			<-unblock
			return response, err
		}}
		f.b.archiveLife.mu.Lock()
		f.share.bucket.last = time.Now().Add(-2 * time.Second)
		f.b.archiveLife.mu.Unlock()
		owner, stop, err := f.b.beginExpiredTaskOwner(f.ctx, f.part, f.share)
		require.NoError(t, err)
		defer stop()
		readDone := make(chan error, 1)
		go func() {
			_, err := f.b.loadCleanupClaimBundle(owner, f.ref)
			owner.finish()
			readDone <- err
		}()
		select {
		case <-arrived:
		case <-f.ctx.Done():
			t.Fatal("original native read did not arrive")
		}
		releaseDone := make(chan error, 1)
		go func() { releaseDone <- f.b.ReleaseWorkerSession(f.ctx, f.session) }()
		select {
		case <-owner.call.ctx.Done():
		case <-f.ctx.Done():
			t.Fatal("session release did not cancel the original call")
		}
		select {
		case err := <-releaseDone:
			t.Fatalf("release returned before native join: %v", err)
		default:
		}
		select {
		case <-owner.done:
			t.Fatal("cancellation was mistaken for call completion")
		default:
		}

		releaseOnce.Do(func() { close(unblock) })
		select {
		case err := <-readDone:
			require.Error(t, err, "native success after owner cancellation cannot hand off a prepared claim")
		case <-f.ctx.Done():
			t.Fatal("native read did not join")
		}
		select {
		case err := <-releaseDone:
			require.NoError(t, err)
		case <-f.ctx.Done():
			t.Fatal("session release did not join")
		}
		require.Equal(t, int64(1), reads.Load(), "lost owner must stop before the second bundle RPC")
	})
}
