package etcd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Only E's attribution to T is wrong. All mandatory birth records are written
// by the existing real metadata fixture; this helper creates no evidence seal.
func cleanupIndexSeed(t *testing.T, f *legacyPageFixture, pending bool) cleanupBirthTestSeed {
	t.Helper()
	s := seedCleanupBirth(t, f.backfillBootstrapFixture)
	e := cleanupBirthTestExecution(s)
	e.TaskDigest = strings.Repeat("e", 64)
	e.Slot = cleanupCandidate(e.TaskDigest, e.Selection)
	e.Incarnation = cleanupIncarnation(cleanupSlotRecordFromExecution(e))
	wire, err := encodeCleanupExecution(e)
	require.NoError(t, err)
	f.server.put(s.keys[4], wire, 0)
	cleanupBackfillSeedMetadata(t, f, s, false, false, pending, false)
	return s
}

// The strict reader still refuses the Task. Only the guarded error may carry
// separate index-only evidence; no normal birth or physical proof is returned.
func cleanupIndexRead(t *testing.T, f *legacyPageFixture, s cleanupBirthTestSeed) *cleanupBirthIndexEvidence {
	t.Helper()
	backfillBootstrapCredit(f.b, f.ranges, 262144)
	birth, err := f.b.readCleanupBirth(context.Background(), f.partition, f.ranges, s.task.Reference.TaskID)
	require.ErrorIs(t, err, ErrCorruptRecord)
	require.Nil(t, birth)
	var refusal *cleanupBirthOptionalRefusal
	require.ErrorAs(t, err, &refusal)
	require.NotNil(t, refusal.index)
	require.NoError(t, f.b.validateCleanupBirthIndexEvidence(f.partition, refusal.index))
	return refusal.index
}

func TestCleanupBirthIndexEvidence(t *testing.T) {
	for _, present := range []bool{false, true} {
		name := "absent-P"
		if present {
			name = "present-P"
		}
		t.Run(name, func(t *testing.T) {
			f := newLegacyPageFixture(t)
			s := cleanupIndexSeed(t, f, present)
			index := cleanupIndexRead(t, f, s)
			require.Same(t, index, index.self)
			require.Same(t, f.b, index.origin)
			require.Same(t, f.partition, index.partition)
			require.Equal(t, s.task, index.record)
			require.Equal(t, s.intent.Attempt, index.initial)
			require.Equal(t, s.birth, index.birth)
			require.GreaterOrEqual(t, index.revision, f.server.point(s.keys[4]).ModRevision)
			for i, fence := range []taskFence{index.task, index.intent, index.link, index.receipt} {
				require.Equal(t, ownTaskFence(f.server.point(s.keys[i])), fence)
			}
			if present {
				require.NotNil(t, index.pending)
				require.Equal(t, ownTaskFence(f.server.point(s.keys[5])), *index.pending)
			} else {
				require.Nil(t, index.pending)
			}

			copied := *index
			require.Error(t, f.b.validateCleanupBirthIndexEvidence(f.partition, &copied))
			other := newLegacyPageFixture(t)
			require.Error(t, other.b.validateCleanupBirthIndexEvidence(other.partition, index))
			require.Error(t, f.b.validateCleanupBirthIndexEvidence(other.partition, index))
			require.Error(t, f.b.validateCleanupBirthIndexEvidence(f.partition, nil))

			// Pure decoding cannot certify that the original W postguard ran.
			birth, err := f.b.cleanupBirthFromPoints(f.partition, f.server.point(s.keys[1]), cleanupBirthTestPoints(f.backfillBootstrapFixture, s), index.revision)
			require.Nil(t, birth)
			var draft *cleanupBirthOptionalRefusal
			require.ErrorAs(t, err, &draft)
			require.Error(t, f.b.validateCleanupBirthIndexEvidence(f.partition, draft.index))
		})
	}
	for _, bad := range []string{"missing-link", "rewritten-T", "bad-receipt", "malformed-P", "rewritten-P", "leased-P"} {
		t.Run(bad, func(t *testing.T) {
			f := newLegacyPageFixture(t)
			s := cleanupIndexSeed(t, f, strings.HasSuffix(bad, "P"))
			switch bad {
			case "missing-link":
				_, err := f.b.client.Txn(context.Background()).Then(clientv3.OpDelete(s.keys[2])).Commit()
				require.NoError(t, err)
			case "rewritten-T":
				f.server.put(s.keys[0], s.wires[0], 0)
			case "bad-receipt":
				f.server.put(s.keys[3], "{}", 0)
			case "malformed-P":
				f.server.put(s.keys[5], "{}", 0)
			case "rewritten-P":
				f.server.put(s.keys[5], string(f.server.point(s.keys[5]).Value), 0)
			case "leased-P":
				value := string(f.server.point(s.keys[5]).Value)
				_, err := f.b.client.Txn(context.Background()).Then(clientv3.OpDelete(s.keys[5])).Commit()
				require.NoError(t, err)
				f.server.put(s.keys[5], value, f.partition.fence.lease)
			}
			backfillBootstrapCredit(f.b, f.ranges, 262144)
			birth, err := f.b.readCleanupBirth(context.Background(), f.partition, f.ranges, s.task.Reference.TaskID)
			require.Error(t, err)
			require.Nil(t, birth)
			var refusal *cleanupBirthOptionalRefusal
			if errors.As(err, &refusal) {
				require.Error(t, f.b.validateCleanupBirthIndexEvidence(f.partition, refusal.index), "a malformed mandatory cohort or P cannot become covered")
			}
		})
	}
}

func TestCleanupBirthIndexGuardAndBounds(t *testing.T) {
	for _, failure := range []string{"postguard", "transport"} {
		t.Run(failure, func(t *testing.T) {
			f := newLegacyPageFixture(t)
			s := cleanupIndexSeed(t, f, false)
			f.b.client.KV = &faultKV{KV: f.b.client.KV, match: cleanupBirthCohortOps, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				require.NoError(t, err)
				if failure == "transport" {
					return nil, context.DeadlineExceeded
				}
				f.server.put(f.partition.fence.key, f.partition.fence.value, f.partition.fence.lease)
				return r, nil
			}}
			backfillBootstrapCredit(f.b, f.ranges, 262144)
			birth, err := f.b.readCleanupBirth(context.Background(), f.partition, f.ranges, s.task.Reference.TaskID)
			require.Error(t, err)
			require.Nil(t, birth)
			var refusal *cleanupBirthOptionalRefusal
			if errors.As(err, &refusal) && refusal.index != nil {
				require.Nil(t, refusal.index.self, "joined guard failure must not expose a sealed fallback")
			}
			f.b.archiveLife.mu.Lock()
			liveErr, tokens := f.session.live(), f.ranges.bucket.tokens
			f.b.archiveLife.mu.Unlock()
			require.Error(t, liveErr)
			require.Zero(t, tokens, "a failed original read never refunds its earned Range")
		})
	}
	t.Run("full-page-and-index-accounted-without-optional-bodies", func(t *testing.T) {
		f, w := newDueAutomaticMetadataFixture(t)
		pendingPageSeed(t, &duePageFixture{b: f.b, server: f.server}, 0, 80)
		s := cleanupIndexSeed(t, f, true)
		index := cleanupIndexRead(t, f, s)
		backfillBootstrapCredit(f.b, f.ranges, 262144)
		page, err := f.b.readCleanupPendingPage(w.ctx, f.partition, f.ranges, nil)
		require.NoError(t, err)
		require.Len(t, page.Candidates, 80)
		w.automatic.page, w.automatic.indexBirth = page, index
		retained, err := w.automaticRetainedBytes()
		require.NoError(t, err)
		require.LessOrEqual(t, retained, uint64(4<<20))
		charge, err := automaticEvidenceBytes(w.automatic)
		require.NoError(t, err)
		require.Greater(t, charge, uint64(64<<10), "index bodies must not be omitted from accounting")
		require.LessOrEqual(t, charge, uint64(188416))

		// Oversize bodies are only accounting inputs, never proof or dispatch.
		original := index.task.value
		index.task.value = strings.Repeat("x", 4097)
		_, err = w.automaticRetainedBytes()
		require.Error(t, err)
		index.task.value = original
		w.automaticFinishPhase()
		require.Nil(t, w.automatic.indexBirth, "the phase must release retained index evidence")
	})
}
