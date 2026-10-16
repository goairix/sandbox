package etcd

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func workerTestReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("original owner did not return")
	}
	var zero T
	return zero
}

func TestWorkerCloseJoins(t *testing.T) {
	t.Run("release-canceled-during-callback-join", func(t *testing.T) {
		b, server := workerTestBackend(t)
		s, p, share := workerRelistTestClaims(t, b, 0, 0)
		block := workerRelistTestStart(t, b, context.Background(), p, share)
		owned := workerTestReceive(t, block.entered)
		type revokeEntry struct {
			lease        clientv3.LeaseID
			err          error
			at, deadline time.Time
			bounded      bool
		}
		entries := make(chan revokeEntry, 2)
		original := b.client.Lease
		b.client.Lease = &workerTestLease{Lease: original, revoke: func(ctx context.Context, lease clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
			// Observe the actual Revoke entry, before workerRevoke's deferred
			// cancellation can alter the context being inspected.
			deadline, bounded := ctx.Deadline()
			entries <- revokeEntry{lease: lease, err: ctx.Err(), at: time.Now(), deadline: deadline, bounded: bounded}
			return original.Revoke(ctx, lease)
		}}
		releaseCtx, cancelRelease := context.WithCancel(context.Background())
		t.Cleanup(cancelRelease)
		done := make(chan error, 1)
		go func() { done <- b.ReleaseWorkerSession(releaseCtx, s) }()
		workerTestReceive(t, owned.Done())
		select {
		case err := <-done:
			t.Fatalf("Release returned before its actual callback: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		cancelRelease()
		select {
		case err := <-done:
			t.Fatalf("caller cancellation replaced the actual callback join: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		block.unblock()
		require.Error(t, workerTestReceive(t, block.done))
		entry := workerTestReceive(t, entries)
		require.Equal(t, clientv3.LeaseID(s.Reference().LeaseID), entry.lease)
		require.NoError(t, entry.err, "an admitted original cleanup must not inherit canceled Release context")
		require.True(t, entry.bounded)
		require.True(t, entry.deadline.After(entry.at))
		require.LessOrEqual(t, entry.deadline.Sub(entry.at), b.requestTimeout)
		require.NoError(t, workerTestReceive(t, done))
		guard, _ := b.namespace.workerSessionKey(s.Reference().SessionID)
		partition, _ := b.namespace.workerPartitionKey(0)
		budget, _ := b.namespace.workerBudgetKey(WorkerBudgetRelist, 0)
		for _, key := range []string{guard, partition, budget} {
			require.Nil(t, server.point(key))
		}
		require.NoError(t, b.Close())
		_, _, revokes := server.counts()
		require.Equal(t, 1, revokes)
		require.Empty(t, entries, "Close cannot retry the already joined original cleanup")
	})
	for _, phase := range []string{"initial-Grant", "original-Renew"} {
		t.Run(phase, func(t *testing.T) {
			b, server := workerTestBackend(t)
			var s *WorkerSession
			if phase == "original-Renew" {
				var err error
				s, err = b.OpenWorkerSession(context.Background(), "worker")
				require.NoError(t, err)
			}
			entered := make(chan context.Context, 1)
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock) // Always release before the backend cleanup can join.
			original := b.client.Lease
			wrapper := &workerTestLease{Lease: original}
			if phase == "initial-Grant" {
				wrapper.grant = func(ctx context.Context, ttl int64) (*clientv3.LeaseGrantResponse, error) {
					r, err := original.Grant(ctx, ttl)
					entered <- ctx
					<-release
					return r, err
				}
			} else {
				wrapper.keep = func(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseKeepAliveResponse, error) {
					r, err := original.KeepAliveOnce(ctx, id)
					entered <- ctx
					<-release
					return r, err
				}
			}
			b.client.Lease = wrapper
			operationDone := make(chan error, 1)
			go func() {
				if phase == "initial-Grant" {
					_, err := b.OpenWorkerSession(context.Background(), "worker")
					operationDone <- err
				} else {
					operationDone <- b.RenewWorkerSession(context.Background(), s)
				}
			}()
			rpcContext := workerTestReceive(t, entered)
			closeDone := make(chan error, 1)
			go func() { closeDone <- b.Close() }()
			workerTestReceive(t, rpcContext.Done())
			select {
			case err := <-closeDone:
				t.Fatalf("Close returned before the actual original RPC returned: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			_, _, revokes := server.counts()
			require.Zero(t, revokes, "the client/original Lease cannot be cleaned up while its RPC owner still runs")
			unblock()
			require.Error(t, workerTestReceive(t, operationDone))
			require.NoError(t, workerTestReceive(t, closeDone))
			require.NoError(t, b.Close(), "idempotent Close returns the original cached result")
			grants, _, revokes := server.counts()
			require.Equal(t, 1, grants)
			require.Equal(t, 1, revokes)
		})
	}
	t.Run("once-only-cached-cleanup-error", func(t *testing.T) {
		server, options := workerTestEndpoint(t)
		b, err := New(context.Background(), options)
		require.NoError(t, err)
		t.Cleanup(func() { _ = b.Close() })
		_, err = b.OpenWorkerSession(context.Background(), "worker")
		require.NoError(t, err)
		original := b.client.Lease
		b.client.Lease = &workerTestLease{Lease: original, revoke: func(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
			r, err := original.Revoke(ctx, id)
			return r, errors.Join(err, errors.New("revoke ACK lost"))
		}}
		first := b.Close()
		require.ErrorIs(t, first, ErrOutcomeUnknown)
		require.Equal(t, first, b.Close())
		_, _, revokes := server.counts()
		require.Equal(t, 1, revokes)
	})
}

func TestWorkerSessionSingleOperation(t *testing.T) {
	b, server := workerTestBackend(t)
	s, err := b.OpenWorkerSession(context.Background(), "worker")
	require.NoError(t, err)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	original := b.client.Lease
	b.client.Lease = &workerTestLease{Lease: original, keep: func(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseKeepAliveResponse, error) {
		r, err := original.KeepAliveOnce(ctx, id)
		entered <- struct{}{}
		<-release
		return r, err
	}}
	done := make(chan error, 1)
	go func() { done <- b.RenewWorkerSession(context.Background(), s) }()
	workerTestReceive(t, entered)
	_, err = b.ClaimWorkerPartition(context.Background(), s, 1)
	require.ErrorIs(t, err, ErrConflict)
	require.ErrorIs(t, b.RenewWorkerSession(context.Background(), s), ErrConflict)
	unblock()
	require.NoError(t, workerTestReceive(t, done))
	p, err := b.ClaimWorkerPartition(context.Background(), s, 1)
	require.NoError(t, err)
	require.NotNil(t, p)
	grants, keeps, _ := server.counts()
	require.Equal(t, 1, grants)
	require.Equal(t, 1, keeps)
}
