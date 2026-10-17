package etcd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Both legacy Tasks have authentic immutable T/I/link/committed-receipt births.
// Only the first Task's optional E is semantically corrupt. Index coverage must
// reach the later valid Task without converting that refusal into execution or
// terminal authority. This host fixture performs metadata work only.
func TestDueWorkerAutomaticBackfillOptionalRefusal(t *testing.T) {
	f, w := newDueAutomaticMetadataFixture(t)
	first := seedCleanupBirth(t, f.backfillBootstrapFixture)
	second := cleanupBackfillAdditionalBirth(t, f, first, "ffffffff-1111-4111-8111-111111111111", false)
	execution := cleanupBirthTestExecution(first)
	execution.TaskDigest = strings.Repeat("e", 64)
	// Keep E internally canonical: the only bad attribution is its digest
	// relative to the authentic Task, not a malformed execution record.
	execution.Slot = cleanupCandidate(execution.TaskDigest, execution.Selection)
	execution.Incarnation = cleanupIncarnation(cleanupSlotRecordFromExecution(execution))
	require.NotEqual(t, workerIdentityDigest(first.wires[0]), execution.TaskDigest)
	wire, err := encodeCleanupExecution(execution)
	require.NoError(t, err)
	f.server.put(first.keys[4], wire, 0)
	originalExecution := ownTaskFence(f.server.point(first.keys[4]))
	require.Nil(t, f.server.point(first.keys[5]))
	require.Nil(t, f.server.point(second.keys[5]))

	// Establish the actual strict typed refusal before the actor is driven.
	// The failure must not be mistaken for missing mandatory birth metadata.
	backfillBootstrapCredit(f.b, f.ranges, 262144)
	birth, err := f.b.readCleanupBirth(w.ctx, f.partition, f.ranges, first.task.Reference.TaskID)
	require.ErrorIs(t, err, ErrCorruptRecord)
	require.Nil(t, birth)
	for i := 0; i < 4; i++ {
		require.Equal(t, first.wires[i], string(f.server.point(first.keys[i]).Value))
		require.Equal(t, first.birth, f.server.point(first.keys[i]).CreateRevision)
		require.Equal(t, first.birth, f.server.point(first.keys[i]).ModRevision)
	}

	for turn := 0; turn < 40; turn++ {
		dueAutomaticMetadataTurn(t, f, w)
		state := w.automatic.partitions[0].backfill
		if state != nil && state.record.Phase == "complete" {
			break
		}
	}
	require.NotNil(t, f.server.point(second.keys[5]), "optional refusal of the first legacy Task must not hide a later valid Task")
	state := w.automatic.partitions[0].backfill
	require.NotNil(t, state)
	require.Equal(t, "complete", state.record.Phase, "completion means discovery indexing, not physical cleanup")
	require.Equal(t, second.task.Reference.TaskID, state.record.Frontier)
	for _, seeded := range []cleanupBirthTestSeed{first, second} {
		pendingKV := f.server.point(seeded.keys[5])
		require.NotNil(t, pendingKV)
		pending, err := decodeCleanupPending(f.b.namespace, f.b.restoreEpoch, pendingKV, pendingKV.ModRevision)
		require.NoError(t, err)
		require.Equal(t, seeded.task.Reference, pending.Reference)
		require.Equal(t, workerIdentityDigest(seeded.wires[0]), pending.TaskDigest)
	}

	// Conservative indexing leaves E byte-for-byte and revision-for-revision
	// intact. The strict pending/execution path must still refuse this Task.
	require.Equal(t, originalExecution, ownTaskFence(f.server.point(first.keys[4])))
	backfillBootstrapCredit(f.b, f.ranges, 262144)
	birth, err = f.b.readCleanupBirth(w.ctx, f.partition, f.ranges, first.task.Reference.TaskID)
	require.ErrorIs(t, err, ErrCorruptRecord)
	require.Nil(t, birth)
	require.Nil(t, f.server.point(second.keys[4]), "indexing cannot fabricate physical execution")
	for _, job := range w.cleanupJobs {
		require.Nil(t, job)
	}
}
