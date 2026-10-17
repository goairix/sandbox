package etcd

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Actual original Range admissions, rather than readiness booleans, spend the
// one available credit. Reusing automatic lane 1 for cleanup would starve one
// of these continuously waiting consumers under the actor's fixed call order.
func TestDueWorkerColdQuotaThreeRangeLanes(t *testing.T) {
	// Due discovery requires the real constructor's publication trust and
	// trusted clock. The metadata-only backfill fixture deliberately lacks both.
	f := newCleanupColdQuotaFixture(t)
	w := f.w
	owner, err := f.b.registerEnrolledPublication(w.ctx)
	require.NoError(t, err)
	defer owner.finish()
	key := f.b.namespace.Root() + "cold-range-fairness"
	f.server.put(key, "bounded metadata", 0)
	grants := make([]int, 0, 15)
	for round := 0; round < 15; round++ {
		backfillBootstrapCredit(f.b, f.ranges, 262144)
		before := len(grants)
		for _, lane := range []int{2, 1, 0} {
			claimLane := []int{0, 2, 1}[lane]
			w.beginClaimLane(claimLane)
			at := w.readyRangeAt(f.ranges, lane)
			if time.Now().Before(at) {
				w.endClaimLane()
				continue
			}
			switch lane {
			case 0:
				_, err = f.b.ReadDuePage(w.ctx, f.part, f.ranges, nil)
			case 1:
				_, err = f.b.readCleanupPendingPage(w.ctx, f.part, f.ranges, nil)
			case 2:
				_, err = f.b.readColdQuotaWorkerPoints(owner, f.session, f.part, f.ranges, []string{key}, []int{4096})
			}
			require.NoError(t, err)
			w.endClaimLane()
			grants = append(grants, lane)
			f.b.archiveLife.mu.Lock()
			tokens, operation := f.ranges.bucket.tokens, f.session.operation
			f.b.archiveLife.mu.Unlock()
			require.Zero(t, tokens)
			require.Nil(t, operation)
		}
		require.Len(t, grants, before+1)
	}
	for i := 3; i+3 <= len(grants); i++ {
		seen := [3]bool{}
		for _, lane := range grants[i : i+3] {
			seen[lane] = true
		}
		require.Equal(t, [3]bool{true, true, true}, seen, "all consumers progress within three actual grants: %v", grants)
	}

	// A cleanup turn without Range work withdraws quota lane 2. It cannot
	// retain the preference after its workspace finishes or waits on physical IO.
	backfillBootstrapCredit(f.b, f.ranges, 0)
	w.beginClaimLane(1)
	_ = w.readyRangeAt(f.ranges, 2)
	w.endClaimLane()
	w.beginClaimLane(1)
	w.endClaimLane()
	require.False(t, w.rangeWaiting[f.ranges.slot][2])
	backfillBootstrapCredit(f.b, f.ranges, 262144)
	admitted := false
	for _, lane := range []int{1, 0} {
		w.beginClaimLane([]int{0, 2}[lane])
		at := w.readyRangeAt(f.ranges, lane)
		w.endClaimLane()
		admitted = admitted || !time.Now().Before(at)
	}
	require.True(t, admitted, "idle quota lane cannot block an earned opportunity")
}

// The cold reservation is additional to the existing automatic reservation:
// 32MiB cache + 24MiB fixed/transient + 4MiB discovery + 4MiB quota = 64MiB.
func TestDueWorkerColdQuotaCacheReservation(t *testing.T) {
	for _, cold := range []bool{false, true} {
		t.Run(fmt.Sprintf("cold-%t", cold), func(t *testing.T) {
			f, w := newDueAutomaticMetadataFixture(t)
			f.b.localCleanup = &LocalCleanupOptions{MaxActive: 32, ColdRuntimeQuotaReturn: cold}
			w.automatic, w.cache.automatic = nil, false
			require.NoError(t, w.initializeAutomatic())
			limit := 9631
			if cold {
				limit = 9567 // 64 distinct cold source keys are additionally reserved.
			}
			for i := 0; i < limit; i++ {
				require.NoError(t, w.cache.put(fmt.Sprintf("%scold-count/%05d", f.b.namespace.Root(), i), "v", DueCandidateRecord{}))
			}
			require.ErrorIs(t, w.cache.put(f.b.namespace.Root()+"cold-count/overflow", "v", DueCandidateRecord{}), ErrConflict)
			require.Equal(t, limit, w.cache.count)
		})
	}
	t.Run("bytes-charge-before-copy", func(t *testing.T) {
		f, w := newDueAutomaticMetadataFixture(t)
		f.b.localCleanup = &LocalCleanupOptions{MaxActive: 32, ColdRuntimeQuotaReturn: true}
		w.automatic, w.cache.automatic = nil, false
		require.NoError(t, w.initializeAutomatic())
		// The original cache charge is 3*value bytes plus key/fixed charge.
		// Eleven MiB fits the old 36MiB policy but exceeds the cold 32MiB cap.
		require.ErrorIs(t, w.cache.put(f.b.namespace.Root()+"cold-bytes/overflow", strings.Repeat("x", 11<<20), DueCandidateRecord{}), ErrConflict)
		require.Zero(t, w.cache.count)
		require.Zero(t, w.cache.bytes)
		w.publishStatus("cold-budget", nil)
		require.Equal(t, uint64(32<<20), w.Status().OwnedBytes, "fixed24+automatic4+cold4 are counted once each")
	})
}

// Legacy callers retain their existing two-lane arbitration and cache space.
// Cold mode is deployment configuration, never inferred from a v2 record.
func TestDueWorkerColdQuotaDisabledKeepsTwoRangeLanes(t *testing.T) {
	f, w := newDueAutomaticMetadataFixture(t)
	f.b.localCleanup = &LocalCleanupOptions{MaxActive: 32}
	grants := make([]int, 0, 8)
	for round := 0; round < 8; round++ {
		backfillBootstrapCredit(f.b, f.ranges, 262144)
		for _, lane := range []int{1, 0} {
			w.beginClaimLane(2 * lane)
			at := w.readyRangeAt(f.ranges, lane)
			if !time.Now().Before(at) {
				_, err := f.b.readCleanupPendingPage(context.Background(), f.partition, f.ranges, nil)
				require.NoError(t, err)
				grants = append(grants, lane)
			}
			w.endClaimLane()
		}
	}
	require.Len(t, grants, 8)
	for i := 2; i+2 <= len(grants); i++ {
		require.NotEqual(t, grants[i], grants[i+1], "original two contenders alternate actual grants")
	}
}
