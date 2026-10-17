package etcd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These 32 slots are CONTROL-ONLY registry occupants with valid immutable Task
// references. They have no Kernel, context, proof or running physical chain.
// Tests call only the serial discovery/registry controls, never cleanupTurn,
// enqueue, finish or the physical executor for these occupants.
func dueAutomaticFullRegistry(t *testing.T) (*legacyPageFixture, *DueWorker) {
	t.Helper()
	f, w := newDueAutomaticMetadataFixture(t)
	pendingPageSeed(t, &duePageFixture{b: f.b, server: f.server}, 0, 81)
	dueAutomaticMetadataTurn(t, f, w)
	a := w.automatic
	require.Len(t, a.page.Candidates, 80)
	require.NotNil(t, a.page.Next)
	require.Equal(t, "pending-select", a.phase)

	f.b.archiveLife.mu.Lock()
	for i := range w.cleanupJobs {
		w.cleanupJobs[i] = &cleanupLocalJob{task: a.page.Candidates[i+1].Reference}
	}
	f.b.archiveLife.mu.Unlock()
	t.Cleanup(func() {
		f.b.archiveLife.mu.Lock()
		w.cleanupJobs = [32]*cleanupLocalJob{}
		f.b.archiveLife.mu.Unlock()
	})
	for _, job := range w.cleanupJobs {
		require.NoError(t, job.task.Validate())
	}
	duplicate, full := w.automaticRegistry(a.page.Candidates[0].Reference)
	require.False(t, duplicate)
	require.True(t, full)
	return f, w
}

func TestDueWorkerAutomaticRegistry(t *testing.T) {
	t.Run("full-preserves-page-and-yields-backfill", func(t *testing.T) {
		f, w := dueAutomaticFullRegistry(t)
		a := w.automatic
		original := append([]cleanupPendingRecord(nil), a.page.Candidates...)
		next := a.page.Next
		before := len(backfillBootstrapRequests(f.backfillBootstrapFixture))
		at, err := w.automaticTurn(w.ctx)
		require.NoError(t, err)
		require.False(t, at.IsZero(), "full capacity schedules a finite retry")
		require.Len(t, backfillBootstrapRequests(f.backfillBootstrapFixture), before, "capacity control performs no metadata RPC")
		require.Equal(t, original, a.page.Candidates)
		require.Same(t, next, a.page.Next)
		require.Zero(t, a.index)
		require.True(t, a.pageReady)
		require.Nil(t, a.partitions[0].cursor, "continuation cannot advance before the retained page is consumed")
		require.Empty(t, a.phase)
		require.True(t, a.preferBackfill)

		// The following actual paid B read proves this is a yield to other
		// discovery work, rather than a busy retry of the full registry.
		dueAutomaticMetadataTurn(t, f, w)
		require.True(t, a.backfill)
		require.Equal(t, "backfill-refresh", a.phase)
		require.NotNil(t, a.observation)
		require.Equal(t, original, a.page.Candidates)
		require.Same(t, next, a.page.Next)
		require.Zero(t, a.index)
		retained, err := w.automaticRetainedBytes()
		require.NoError(t, err)
		require.LessOrEqual(t, retained, uint64(4<<20))
	})

	t.Run("duplicate-in-last-slot-advances-while-full", func(t *testing.T) {
		f, w := dueAutomaticFullRegistry(t)
		a := w.automatic
		tail := append([]cleanupPendingRecord(nil), a.page.Candidates[1:]...)
		next := a.page.Next
		f.b.archiveLife.mu.Lock()
		w.cleanupJobs[31] = &cleanupLocalJob{task: a.page.Candidates[0].Reference}
		f.b.archiveLife.mu.Unlock()
		duplicate, full := w.automaticRegistry(a.page.Candidates[0].Reference)
		require.True(t, duplicate)
		require.True(t, full)
		before := len(backfillBootstrapRequests(f.backfillBootstrapFixture))
		_, err := w.automaticTurn(w.ctx)
		require.NoError(t, err)
		require.Len(t, backfillBootstrapRequests(f.backfillBootstrapFixture), before)
		require.Equal(t, 1, a.index)
		require.Equal(t, cleanupPendingRecord{}, a.page.Candidates[0], "release the consumed hint body immediately")
		require.Equal(t, tail, a.page.Candidates[1:])
		require.Same(t, next, a.page.Next)
		require.True(t, a.pageReady)
		require.Nil(t, a.partitions[0].cursor)
	})

	t.Run("one-free-slot-reselects-the-same-candidate", func(t *testing.T) {
		f, w := dueAutomaticFullRegistry(t)
		a := w.automatic
		first, next := a.page.Candidates[0], a.page.Next
		_, err := w.automaticTurn(w.ctx)
		require.NoError(t, err)
		f.b.archiveLife.mu.Lock()
		w.cleanupJobs[31] = nil
		f.b.archiveLife.mu.Unlock()
		duplicate, full := w.automaticRegistry(first.Reference)
		require.False(t, duplicate)
		require.False(t, full)

		// Select the retained pending opportunity directly: no real physical
		// job completed here. Capacity permits birth authentication, not an
		// enqueue success in this intentionally missing-profile host model.
		selected, _ := w.automaticSelectPending(time.Now())
		require.True(t, selected)
		before := len(backfillBootstrapRequests(f.backfillBootstrapFixture))
		_, err = w.automaticTurn(w.ctx)
		require.NoError(t, err)
		require.Len(t, backfillBootstrapRequests(f.backfillBootstrapFixture), before)
		require.Equal(t, "pending-birth", a.phase)
		require.Equal(t, first, a.candidate)
		require.Equal(t, first, a.page.Candidates[0])
		require.Zero(t, a.index)
		require.Same(t, next, a.page.Next)
		require.Nil(t, a.birth)
		require.Nil(t, a.release)
		require.Nil(t, w.cleanupJobs[31], "selection must not fabricate a registered physical job")
		retained, err := w.automaticRetainedBytes()
		require.NoError(t, err)
		require.LessOrEqual(t, retained, uint64(4<<20))
	})
}
