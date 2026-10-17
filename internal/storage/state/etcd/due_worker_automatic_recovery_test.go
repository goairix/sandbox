package etcd

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Commit the first of two authentic legacy births. The resulting incomplete B
// is real durable coverage, not a constructed diagnostic checkpoint. The actor
// must retain this prefix through compaction or a replacement original owner.
func dueAutomaticCommittedPrefix(t *testing.T, f *legacyPageFixture, w *DueWorker) (*cleanupBackfillState, cleanupBirthTestSeed, cleanupBirthTestSeed) {
	t.Helper()
	first := seedCleanupBirth(t, f.backfillBootstrapFixture)
	second := cleanupBackfillAdditionalBirth(t, f, first, "ffffffff-1111-4111-8111-111111111111", false)
	state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
	page, birth := cleanupBackfillAdvanceRead(t, f, state, first)
	backfillBootstrapCredit(f.b, f.claims, 1)
	state, err := f.b.advanceCleanupBackfill(w.ctx, f.partition, f.claims, page, birth, nil)
	require.NoError(t, err)
	require.Equal(t, "list", state.record.Phase)
	require.Equal(t, first.task.Reference.TaskID, state.record.Frontier)
	require.NotNil(t, f.server.point(first.keys[5]))
	require.Nil(t, f.server.point(second.keys[5]))
	w.automatic.partitions[0].backfill = state
	return state, first, second
}

// Acquire genuine replacement capabilities from the same backend. This serial
// harness is never registered as a started physical worker and owns no Kernel.
// It shares only durable data/model transport with its predecessor, not its
// private session, partition, budgets, cache, cursor or birth evidence.
func dueAutomaticReplacement(t *testing.T, prior *legacyPageFixture) (*legacyPageFixture, *DueWorker) {
	t.Helper()
	b := prior.b
	session, err := b.OpenWorkerSession(context.Background(), "automatic-replacement")
	require.NoError(t, err)
	partition, err := b.ClaimWorkerPartition(context.Background(), session, 0xaa)
	require.NoError(t, err)
	ranges, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetRange, 0)
	require.NoError(t, err)
	claims, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetClaim, 0)
	require.NoError(t, err)
	relist, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetRelist, 0)
	require.NoError(t, err)
	cache, err := newDueWorkerCache(b, session)
	require.NoError(t, err)
	scan, err := newDueWorkerScan(b, partition, ranges, claims, 30*time.Second)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(session.ctx)
	t.Cleanup(cancel)
	options := dueWorkerActorOptions()
	options.AutomaticCleanup = true
	w := &DueWorker{
		backend: b, session: session, ctx: ctx, cancel: cancel, options: options,
		ranges: []*WorkerBudgetShare{ranges}, claims: []*WorkerBudgetShare{claims},
		relist: relist, scans: []*dueWorkerScan{scan}, cache: cache,
		cleanupWake: make(chan struct{}, 1),
	}
	w.self = w
	require.NoError(t, w.initializeAutomatic())
	f := &legacyPageFixture{
		backfillBootstrapFixture: &backfillBootstrapFixture{b: b, server: prior.server, session: session, partition: partition, ranges: ranges, claims: claims},
		pages:                    prior.pages,
	}
	return f, w
}

func TestDueWorkerAutomaticRecovery(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending-compaction-guard-lost-%t", lost), func(t *testing.T) {
			f, w := newDueAutomaticMetadataFixture(t)
			rows := pendingPageSeed(t, &duePageFixture{b: f.b, server: f.server}, 0, 81)
			for _, row := range rows[:80] {
				key, err := f.b.namespace.cleanupPendingKey(row.Reference)
				require.NoError(t, err)
				f.server.put(key, `{}`, 0)
			}
			dueAutomaticMetadataTurn(t, f, w)
			a := w.automatic
			oldCursor := a.partitions[0].cursor
			require.NotNil(t, oldCursor)
			require.False(t, a.pageReady)
			selected, _ := w.automaticSelectPending(time.Now())
			require.True(t, selected)
			originalKV := f.b.client.KV
			f.b.client.KV = &faultKV{KV: originalKV, match: duePageRangeOps, after: func(*clientv3.TxnResponse, error) (*clientv3.TxnResponse, error) {
				if lost {
					f.server.put(f.partition.fence.key, f.partition.fence.value, f.partition.fence.lease)
				}
				return nil, rpctypes.ErrCompacted
			}}
			backfillBootstrapCredit(f.b, f.ranges, 262144)
			_, err := w.automaticTurn(w.ctx)
			f.b.client.KV = originalKV
			if lost {
				require.Error(t, err, "compaction must not conceal a lost original guard")
				f.b.archiveLife.mu.Lock()
				liveErr := f.session.live()
				f.b.archiveLife.mu.Unlock()
				require.Error(t, liveErr)
				return
			}
			require.NoError(t, err)
			require.Nil(t, a.partitions[0].cursor)
			require.Zero(t, a.partitions[0].pendingRevision)
			require.Greater(t, time.Until(a.partitions[0].nextSweep), 29*time.Second)
			require.False(t, a.pageReady)
			require.Empty(t, a.page.Candidates)
			require.Nil(t, a.birth)
			require.Nil(t, a.release)

			// A consumed continuation is not transferable back into the actor.
			// Refusal precedes any RPC even though fresh Range credit exists.
			backfillBootstrapCredit(f.b, f.ranges, 262144)
			before := len(backfillBootstrapRequests(f.backfillBootstrapFixture))
			_, err = f.b.readCleanupPendingPage(w.ctx, f.partition, f.ranges, oldCursor)
			require.Error(t, err)
			require.Len(t, backfillBootstrapRequests(f.backfillBootstrapFixture), before)
			dueAutomaticMetadataTurn(t, f, w)
			require.True(t, a.backfill, "pending cooldown must leave unrelated backfill eligible")
			require.NotNil(t, a.observation)
		})
	}

	t.Run("legacy-compaction-retains-committed-prefix", func(t *testing.T) {
		f, w := newDueAutomaticMetadataFixture(t)
		state, _, second := dueAutomaticCommittedPrefix(t, f, w)
		selected, _ := w.automaticSelectBackfill(time.Now())
		require.True(t, selected)
		originalKV := f.b.client.KV
		f.b.client.KV = &faultKV{KV: originalKV, match: duePageRangeOps, after: func(*clientv3.TxnResponse, error) (*clientv3.TxnResponse, error) {
			return nil, rpctypes.ErrCompacted
		}}
		dueAutomaticMetadataTurn(t, f, w)
		f.b.client.KV = originalKV
		a := w.automatic
		require.Equal(t, "backfill-refresh-read", a.phase)
		require.Same(t, state, a.partitions[0].backfill)
		require.Nil(t, a.legacy)
		require.Nil(t, a.birth)
		require.Nil(t, a.release)

		dueAutomaticMetadataTurn(t, f, w) // actual B observation
		dueAutomaticMetadataTurn(t, f, w) // actual view-only CAS
		refreshed := a.partitions[0].backfill
		require.NotSame(t, state, refreshed)
		require.Equal(t, state.record.LegacyCutoffRevision, refreshed.record.LegacyCutoffRevision)
		require.Equal(t, state.record.Frontier, refreshed.record.Frontier)
		require.Greater(t, refreshed.record.ReadViewRevision, state.record.ReadViewRevision)
		require.Equal(t, refreshed.fence, ownTaskFence(f.server.point(refreshed.fence.key)))
		dueAutomaticMetadataTurn(t, f, w) // fresh page starts after durable frontier
		require.Len(t, a.legacy.rows, 1)
		require.Equal(t, second.task.Reference.TaskID, a.legacy.rows[0].taskID)
		require.Same(t, refreshed, a.legacy.state)
		require.Nil(t, f.server.point(second.keys[5]), "page discovery alone cannot claim coverage")
	})

	t.Run("replacement-reloads-durable-prefix-not-old-capabilities", func(t *testing.T) {
		f, old := newDueAutomaticMetadataFixture(t)
		state, first, second := dueAutomaticCommittedPrefix(t, f, old)
		pendingPageSeed(t, &duePageFixture{b: f.b, server: f.server}, 0, 81)
		dueAutomaticMetadataTurn(t, f, old)
		oldCursor := old.automatic.page.Next
		require.NotNil(t, oldCursor)
		require.NoError(t, f.b.ReleaseWorkerSession(context.Background(), f.session))
		before := len(backfillBootstrapRequests(f.backfillBootstrapFixture))
		_, err := old.automaticTurn(old.ctx)
		require.Error(t, err)
		require.Len(t, backfillBootstrapRequests(f.backfillBootstrapFixture), before)

		fresh, w := dueAutomaticReplacement(t, f)
		require.Nil(t, w.automatic.partitions[0].backfill)
		require.Nil(t, w.automatic.partitions[0].cursor)
		backfillBootstrapCredit(fresh.b, fresh.ranges, 262144)
		before = len(backfillBootstrapRequests(fresh.backfillBootstrapFixture))
		_, err = fresh.b.readCleanupPendingPage(w.ctx, fresh.partition, fresh.ranges, oldCursor)
		require.Error(t, err)
		require.Len(t, backfillBootstrapRequests(fresh.backfillBootstrapFixture), before)
		selected, _ := w.automaticSelectBackfill(time.Now())
		require.True(t, selected)
		dueAutomaticMetadataTurn(t, fresh, w)
		adopted := w.automatic.partitions[0].backfill
		require.NotSame(t, state, adopted)
		require.NotSame(t, state.partition, adopted.partition)
		require.Equal(t, state.fence, adopted.fence)
		require.Equal(t, state.record.Frontier, adopted.record.Frontier)
		require.Equal(t, state.record.LegacyCutoffRevision, adopted.record.LegacyCutoffRevision)

		// The remaining authentic legacy row can complete through this fresh
		// actor. No old phase pointer or prior invocation provenance is adopted.
		for i := 0; i < 8 && w.automatic.partitions[0].backfill.record.Phase != "complete"; i++ {
			dueAutomaticMetadataTurn(t, fresh, w)
		}
		require.Equal(t, "complete", w.automatic.partitions[0].backfill.record.Phase)
		require.Equal(t, second.task.Reference.TaskID, w.automatic.partitions[0].backfill.record.Frontier)
		require.NotNil(t, fresh.server.point(first.keys[5]))
		require.NotNil(t, fresh.server.point(second.keys[5]))
		require.Nil(t, fresh.server.point(second.keys[4]), "metadata recovery is not physical cleanup acceptance")
	})
}
