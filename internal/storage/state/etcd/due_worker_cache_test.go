package etcd

import (
	"context"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
)

func TestDueWorkerCacheOriginalOwnerAndOwnedCopies(t *testing.T) {
	f := newDueWatchTestFixture(t)
	_, err := newDueWorkerCache(f.b, f.session)
	require.Error(t, err, "one bounded original cache per session")
	clone := *f.cache
	_, err = f.b.runDueWatchBatch(context.Background(), &clone)
	require.Error(t, err, "a copied cache cannot reconstruct the original session owner")
	before := f.cache.snapshot()
	require.Equal(t, 1, before.Keys)
	require.LessOrEqual(t, unsafe.Sizeof(*f.cache), uintptr(8*1024*1024), "fixed representation stays within its reserved bound")
	require.GreaterOrEqual(t, before.OwnedBytes, uint64(24*1024*1024), "fixed and transient reservations count in the global worker ledger")
	require.LessOrEqual(t, before.OwnedBytes, uint64(64*1024*1024))
	require.NoError(t, f.cache.addPage(f.scan))
	after := f.cache.snapshot()
	require.Equal(t, before.Keys, after.Keys)
	require.Equal(t, before.OwnedBytes, after.OwnedBytes, "exact-key merge does not grow an extra queue node")
	before.Partitions[0].Revision++
	require.Equal(t, after.Partitions, f.cache.snapshot().Partitions, "status returns owned rows")
	f.scan.page[0].SandboxID = "changed-outside-cache"
	f.progress(1)
	f.run(t)
	got, ok := f.cache.peek()
	require.True(t, ok)
	require.Equal(t, f.candidate, got, "candidate ownership survives input record mutation")
	wrong := got
	wrong.Generation++
	f.cache.settled(wrong)
	require.Equal(t, 1, f.cache.snapshot().Keys)
	f.cache.settled(got)
	require.Zero(t, f.cache.snapshot().Keys)
}

func TestDueWorkerCacheCountBoundAndGlobalPageOwnership(t *testing.T) {
	t.Run("actual-fixed-view-pages-over-key-limit", func(t *testing.T) {
		f := newDuePageFixture(t)
		f.seed(t, 10001, f.now.Add(30*time.Second))
		claims, err := f.b.AcquireWorkerBudget(context.Background(), f.session, WorkerBudgetClaim, 0)
		require.NoError(t, err)
		cache, err := newDueWorkerCache(f.b, f.session)
		require.NoError(t, err)
		scan, err := newDueWorkerScan(f.b, f.partition, f.share, claims, 30*time.Second)
		require.NoError(t, err)
		rejected := false
		for pages := 0; pages < 86; pages++ {
			if pages%16 == 0 {
				require.NoError(t, f.b.RenewWorkerSession(context.Background(), f.session))
			}
			f.earn()
			require.NoError(t, scan.refill(context.Background()), "actual original Range, not a constructed candidate callback")
			err = cache.addPage(scan)
			state := cache.snapshot()
			require.LessOrEqual(t, state.Keys, 10000)
			require.LessOrEqual(t, state.OwnedBytes, uint64(64*1024*1024))
			if err != nil {
				rejected = true
				break
			}
			scan.page = nil // release the globally charged source page before next refill
			if scan.cursor == nil {
				break
			}
		}
		require.True(t, rejected, "10001 exact keys cannot be retained or silently dropped")
		require.Zero(t, cache.snapshot().Keys, "invalid view is discarded rather than scheduled partially")
		_, ok := cache.peek()
		require.False(t, ok)
	})
	t.Run("another-unmerged-page-is-not-free", func(t *testing.T) {
		f := newDueWatchTestFixture(t)
		p, err := f.b.ClaimWorkerPartition(context.Background(), f.session, 0xbb)
		require.NoError(t, err)
		second := f.candidate
		second.Partition, second.WorkspaceHash, second.SandboxID = 0xbb, strings.Repeat("b", 64), "second-partition"
		key, err := f.b.namespace.dueCandidateKey(second)
		require.NoError(t, err)
		wire, err := encodeDueCandidate(second)
		require.NoError(t, err)
		f.server.put(key, wire, 0)
		scan, err := newDueWorkerScan(f.b, p, f.share, f.claimShare, 30*time.Second)
		require.NoError(t, err)
		f.earn()
		require.NoError(t, scan.step(context.Background()))
		err = f.cache.addPage(scan)
		require.Error(t, err, "initial consumer permits only one globally retained source page")
		require.LessOrEqual(t, f.cache.snapshot().OwnedBytes, uint64(64*1024*1024))
	})
}

func TestDueWorkerCacheFutureSurvivesWatchAndInvalidationPreservesEvidence(t *testing.T) {
	f := newDueWatchTestFixture(t)
	future := f.candidate
	future.SandboxID, future.IntentID = "future-sandbox", "future-intent"
	future.ExpiresAt = f.now.Add(30 * time.Second)
	key, err := f.b.namespace.dueCandidateKey(future)
	require.NoError(t, err)
	wire, err := encodeDueCandidate(future)
	require.NoError(t, err)
	f.server.put(key, wire, 0)
	// A fresh original fixed view includes a future candidate. No admission
	// result or durable watch watermark is fabricated just to evict it.
	f.earn()
	page, err := f.b.ReadDuePage(context.Background(), f.partition, f.share, nil)
	require.NoError(t, err)
	next := f.scan.recordForPage(page)
	require.NoError(t, f.scan.save(context.Background(), next))
	f.scan.page, f.scan.index, f.scan.cursor = page.Candidates, 0, page.Next
	require.NoError(t, f.cache.addPage(f.scan))
	f.progress(1)
	f.run(t)
	f.cache.settled(f.candidate)
	got, ok := f.cache.peek()
	require.True(t, ok)
	require.Equal(t, future, got)
	checkpoint := dueCheckpointTestRaw(t, f.duePageFixture)
	require.Equal(t, checkpoint.Revision+1, checkpoint.ReplayFrom)
	f.cache.invalidate("test-gap")
	require.Zero(t, f.cache.snapshot().Keys)
	_, ok = f.cache.peek()
	require.False(t, ok)
	require.NotNil(t, f.server.point(key))
	require.NotNil(t, f.server.point(dueCheckpointTestKey(f.duePageFixture)))
	require.Equal(t, checkpoint, dueCheckpointTestRaw(t, f.duePageFixture))
}

func TestDueWorkerCacheCrossPartitionExpiryPriority(t *testing.T) {
	f := newDueWatchTestFixture(t)
	f.scan.page = nil
	p, err := f.b.ClaimWorkerPartition(context.Background(), f.session, 0)
	require.NoError(t, err)
	future := f.candidate
	future.Partition, future.WorkspaceHash = 0, strings.Repeat("0", 64)
	future.SandboxID, future.IntentID = "future-00", "future-00-intent"
	future.ExpiresAt = f.now.Add(30 * time.Second)
	key, err := f.b.namespace.dueCandidateKey(future)
	require.NoError(t, err)
	wire, err := encodeDueCandidate(future)
	require.NoError(t, err)
	f.server.put(key, wire, 0)
	scan, err := newDueWorkerScan(f.b, p, f.share, f.claimShare, 30*time.Second)
	require.NoError(t, err)
	f.earn()
	require.NoError(t, scan.step(context.Background()))
	require.NoError(t, f.cache.addPage(scan))
	f.progress(2)
	f.run(t)
	first, ok := f.cache.peek()
	require.True(t, ok)
	require.Equal(t, f.candidate, first, "partition00 future key cannot block partitionaa expired work")
	f.cache.settled(first)
	second, ok := f.cache.peek()
	require.True(t, ok)
	require.Equal(t, future, second)
}
