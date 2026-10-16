package etcd

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
)

func workerRelistTestClaims(t *testing.T, b *Backend, partition uint8, slot uint16) (*WorkerSession, *WorkerPartitionClaim, *WorkerBudgetShare) {
	t.Helper()
	s, err := b.OpenWorkerSession(context.Background(), "worker")
	require.NoError(t, err)
	p, err := b.ClaimWorkerPartition(context.Background(), s, partition)
	require.NoError(t, err)
	share, err := b.AcquireWorkerBudget(context.Background(), s, WorkerBudgetRelist, slot)
	require.NoError(t, err)
	return s, p, share
}

type workerRelistTestBlock struct {
	entered chan context.Context
	release chan struct{}
	done    chan error
	once    sync.Once
}

func workerRelistTestStart(t *testing.T, b *Backend, ctx context.Context, p *WorkerPartitionClaim, share *WorkerBudgetShare) *workerRelistTestBlock {
	t.Helper()
	block := &workerRelistTestBlock{entered: make(chan context.Context, 1), release: make(chan struct{}), done: make(chan error, 1)}
	t.Cleanup(block.unblock)
	go func() {
		block.done <- b.RunWorkerRelist(ctx, p, share, func(owned context.Context) error {
			block.entered <- owned
			<-block.release // Cancellation never substitutes for actual return.
			return nil
		})
	}()
	return block
}

func (block *workerRelistTestBlock) unblock() { block.once.Do(func() { close(block.release) }) }

func TestWorkerRelistOwner(t *testing.T) {
	t.Run("same-original-session-and-kind-only", func(t *testing.T) {
		b, _ := workerTestBackend(t)
		s, p, share := workerRelistTestClaims(t, b, 0, 0)
		other, err := b.OpenWorkerSession(context.Background(), "other")
		require.NoError(t, err)
		foreignShare, err := b.AcquireWorkerBudget(context.Background(), other, WorkerBudgetRelist, 1)
		require.NoError(t, err)
		rangeShare, err := b.AcquireWorkerBudget(context.Background(), s, WorkerBudgetRange, 0)
		require.NoError(t, err)
		copy := new(WorkerBudgetShare)
		reflect.ValueOf(copy).Elem().Set(reflect.ValueOf(share).Elem())
		called := false
		work := func(context.Context) error { called = true; return nil }
		for _, bad := range []*WorkerBudgetShare{nil, {}, copy, foreignShare, rangeShare} {
			require.ErrorIs(t, b.RunWorkerRelist(context.Background(), p, bad, work), ErrInvalidRecord)
		}
		require.ErrorIs(t, b.RunWorkerRelist(context.Background(), p, share, nil), ErrInvalidRecord)
		require.ErrorIs(t, b.RunWorkerRelist(nil, p, share, work), ErrInvalidRecord)
		require.False(t, called)
		require.NoError(t, b.RunWorkerRelist(context.Background(), p, share, work))
		require.True(t, called)
	})
	t.Run("one-physical-owner-and-original-callback-deadline", func(t *testing.T) {
		b, _ := workerTestBackend(t)
		s, p, share := workerRelistTestClaims(t, b, 0, 0)
		block := workerRelistTestStart(t, b, context.Background(), p, share)
		owned := workerTestReceive(t, block.entered)
		before, ok := owned.Deadline()
		require.True(t, ok)
		require.NoError(t, b.RenewWorkerSession(context.Background(), s), "the native permit must not span a local callback")
		after, ok := owned.Deadline()
		require.True(t, ok)
		require.Equal(t, before, after, "renewal cannot extend already admitted callback work")
		called := false
		require.ErrorIs(t, b.RunWorkerRelist(context.Background(), p, share, func(context.Context) error { called = true; return nil }), ErrConflict)
		require.False(t, called)
		block.unblock()
		require.NoError(t, workerTestReceive(t, block.done))
		require.NoError(t, b.RunWorkerRelist(context.Background(), p, share, func(context.Context) error { return nil }))
	})
	t.Run("local-error-does-not-lose-native-session", func(t *testing.T) {
		b, _ := workerTestBackend(t)
		s, p, share := workerRelistTestClaims(t, b, 0, 0)
		workErr := errors.New("bounded local rebuild failed")
		require.ErrorIs(t, b.RunWorkerRelist(context.Background(), p, share, func(context.Context) error { return workErr }), workErr)
		require.NoError(t, b.RenewWorkerSession(context.Background(), s))
		require.NoError(t, b.RunWorkerRelist(context.Background(), p, share, func(context.Context) error { return nil }))
	})
	for _, releaseKind := range []string{"share", "partition", "session", "backend"} {
		t.Run("release-joins/"+releaseKind, func(t *testing.T) {
			b, _ := workerTestBackend(t)
			s, p, share := workerRelistTestClaims(t, b, 0, 0)
			block := workerRelistTestStart(t, b, context.Background(), p, share)
			owned := workerTestReceive(t, block.entered)
			released := make(chan error, 1)
			go func() {
				switch releaseKind {
				case "share":
					released <- b.ReleaseWorkerBudget(context.Background(), share)
				case "partition":
					released <- b.ReleaseWorkerPartition(context.Background(), p)
				case "session":
					released <- b.ReleaseWorkerSession(context.Background(), s)
				case "backend":
					released <- b.Close()
				}
			}()
			workerTestReceive(t, owned.Done())
			select {
			case err := <-released:
				t.Fatalf("release returned before its actual callback: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			if releaseKind == "share" || releaseKind == "partition" {
				require.NoError(t, b.RenewWorkerSession(context.Background(), s), "release cannot hold a native permit while joining local work")
			}
			block.unblock()
			require.Error(t, workerTestReceive(t, block.done), "intentionally canceled old work must not publish success")
			require.NoError(t, workerTestReceive(t, released))
		})
	}
	t.Run("canceled-work-still-occupies-four-local-owners", func(t *testing.T) {
		b, server := workerTestBackend(t)
		var blocks []*workerRelistTestBlock
		var first *WorkerSession
		for i := 0; i < 4; i++ {
			s, p, share := workerRelistTestClaims(t, b, uint8(i), uint16(i))
			if i == 0 {
				first = s
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			block := workerRelistTestStart(t, b, ctx, p, share)
			owned := workerTestReceive(t, block.entered)
			cancel()
			workerTestReceive(t, owned.Done())
			blocks = append(blocks, block)
		}
		// Model the soft leased handoff without pretending old physical work
		// disappeared. The separate native test supplies actual Lease evidence.
		_, err := server.LeaseRevoke(context.Background(), &pb.LeaseRevokeRequest{ID: first.Reference().LeaseID})
		require.NoError(t, err)
		successor, p, share := workerRelistTestClaims(t, b, 7, 0)
		called := false
		require.Error(t, b.RunWorkerRelist(context.Background(), p, share, func(context.Context) error { called = true; return nil }))
		require.False(t, called, "a new session/soft Lease cannot bypass four original local callbacks")
		require.NoError(t, b.RenewWorkerSession(context.Background(), successor), "ordinary local capacity refusal does not poison the new session")
		for _, block := range blocks {
			block.unblock()
			require.Error(t, workerTestReceive(t, block.done))
		}
		require.NoError(t, b.RunWorkerRelist(context.Background(), p, share, func(context.Context) error { return nil }))
	})
	for _, changed := range []string{"guard", "partition", "share"} {
		t.Run("original-postguard/"+changed, func(t *testing.T) {
			b, server := workerTestBackend(t)
			s, p, share := workerRelistTestClaims(t, b, 0, 0)
			key, _ := b.namespace.Key("workers", "sessions", s.Reference().SessionID)
			if changed == "partition" {
				key, _ = b.namespace.Key("p", "00", "worker")
			}
			if changed == "share" {
				key, _ = b.namespace.Key("workers", "budget", "relist", "00")
			}
			old := server.point(key)
			err := b.RunWorkerRelist(context.Background(), p, share, func(context.Context) error { server.put(key, string(old.Value), old.Lease); return nil })
			require.Error(t, err, "a completed callback cannot publish through changed original fences")
			server.put(key, string(old.Value), old.Lease)
			require.Error(t, b.RenewWorkerSession(context.Background(), s), "restoring bytes cannot revive the lost original capability")
		})
	}
}
