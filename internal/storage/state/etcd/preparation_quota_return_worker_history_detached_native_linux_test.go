//go:build linux && (amd64 || arm64)

package etcd

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// Readiness observes a copy under the original lifecycle lock. Only the actual
// paid cursor step may advance the real bucket or subtract its Range charge;
// elapsed monotonic time, rather than a test credit assignment, earns the turn.
func detachedPhysicalQuotaHistoryRangeReady(t *testing.T, ctx context.Context, b *Backend, ranges *WorkerBudgetShare) {
	t.Helper()
	for {
		b.archiveLife.mu.Lock()
		bucket := ranges.bucket
		b.archiveLife.mu.Unlock()
		require.Positive(t, bucket.rate)
		require.GreaterOrEqual(t, bucket.capacity, uint64(maxMutationBytes))
		now := time.Now()
		ready := dueWorkerCreditAt(bucket, now, uint32(maxMutationBytes))
		if !ready.After(now) {
			return
		}
		timer := time.NewTimer(time.Until(ready))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			t.Fatalf("waiting for the original earned Range credit: %v", ctx.Err())
		}
		timer.Stop()
	}
}

// These independent raw point reads establish before/after metadata identity.
// They do not participate in the paid cursor's seven logical turns and never
// turn a located record, reference or source stamp into a permission.
func detachedPhysicalQuotaHistoryPoints(t *testing.T, f *cleanupDetachedNoEffectNativeFixture, keys []string) []*mvccpb.KeyValue {
	t.Helper()
	points := make([]*mvccpb.KeyValue, len(keys))
	for i, key := range keys {
		reply, err := f.raw.Get(f.ctx, key)
		require.NoError(t, err)
		require.LessOrEqual(t, len(reply.Kvs), 1)
		if len(reply.Kvs) == 1 {
			points[i] = reply.Kvs[0]
		}
	}
	return points
}

// Root installs the optional physical setup and this hook only after genuine
// inventory/reservation/settlement, observer terminal proof, seven provider
// slots and native owner release. This is an independent paid-reader component,
// not managed actor admission or a completed quota refund. The old test-scheduled
// job must really finish and its worker must join before a new session starts.
func detachedPhysicalPaidQuotaHistory(t *testing.T, f *cleanupDetachedNoEffectNativeFixture, finish func(), workspace WorkspaceIdentity) {
	t.Helper()
	require.Equal(t, "1", os.Getenv("SANDBOX_DETACHED_QUOTA_HISTORY_PHYSICAL_NATIVE"))
	require.Equal(t, "1", os.Getenv("SANDBOX_DETACHED_OWNER_RELEASE_PHYSICAL_NATIVE"))
	require.NotNil(t, finish)
	require.NotNil(t, f.job)
	require.NotNil(t, f.worker)
	job, previousWorker := f.job, f.worker
	finish()
	require.Nil(t, job.claim)
	require.True(t, job.effectsJoined)
	require.NotNil(t, job.state)
	require.NotNil(t, job.state.record.Run)
	require.True(t, job.state.record.Run.Joined)
	select {
	case <-job.done:
	default:
		t.Fatal("the original physical job has not completed its real finish tail")
	}
	require.NoError(t, previousWorker.Stop())
	select {
	case <-previousWorker.Done():
	default:
		t.Fatal("the original worker has not joined")
	}
	require.NoError(t, f.ctx.Err(), "the new session inherits the original fixture context, not canceled job.ctx")

	b := f.b
	intentID := f.publication.Record.IntentID
	terminationKey, providerKey, releaseKey, err := b.namespace.localRuntimeReleaseKeys(f.ref)
	require.NoError(t, err)
	taskKey, err := b.namespace.taskKey(f.ref)
	require.NoError(t, err)
	head := detachedPhysicalQuotaHistoryPoints(t, f, []string{taskKey, terminationKey})
	require.NotNil(t, head[0])
	require.NotNil(t, head[1])
	termination, err := decodeLocalRuntimeTermination(head[1])
	require.NoError(t, err)
	historyKeys, _, err := b.taskOwnerReleaseHistoryKeys(termination, head[0])
	require.NoError(t, err)
	require.Len(t, historyKeys, 13)
	reservationKey, workKey, err := b.namespace.coldPreparationKeys(workspace.Partition(), intentID)
	require.NoError(t, err)
	reserved := detachedPhysicalQuotaHistoryPoints(t, f, []string{reservationKey})
	var reservation ColdPreparationRecord
	require.NoError(t, coldDecode(reserved[0], &reservation))
	require.True(t, b.validColdReservation(reservation, workspace, intentID))
	require.Equal(t, "settled", reservation.Phase)
	slotKey, freeKey, err := b.namespace.coldSlotKeys(reservation.Quota)
	require.NoError(t, err)
	readyKey, err := b.namespace.Key("preparation", "inventory", "ready")
	require.NoError(t, err)
	returnKey, err := b.namespace.coldRuntimeQuotaReturnKey(workspace.Partition(), intentID)
	require.NoError(t, err)
	journalKey, err := b.namespace.coldRuntimeQuotaAttemptKey(workspace.Partition(), intentID)
	require.NoError(t, err)
	sourceKeys := append([]string{releaseKey, terminationKey, providerKey, readyKey, reservationKey, workKey, slotKey, freeKey, returnKey, journalKey}, historyKeys...)
	publicationReceipt, err := b.runtimeDispatchReceiptKey(f.publication.Record.Attempt)
	require.NoError(t, err)
	settlementReceipt, err := b.runtimeDispatchReceiptKey(reservation.Settlement)
	require.NoError(t, err)
	sourceKeys = append(sourceKeys, publicationReceipt, settlementReceipt)
	executionIndex := len(sourceKeys)
	require.NotNil(t, job.state.slot)
	sourceKeys = append(sourceKeys, job.state.execution.key, job.state.slot.key)
	before := detachedPhysicalQuotaHistoryPoints(t, f, sourceKeys)
	require.NotNil(t, before[0])
	require.NoError(t, b.decodeColdReady(before[3]))
	var slot coldSlotRecord
	require.NoError(t, coldDecode(before[6], &slot))
	require.True(t, b.validColdSlot(slot))
	require.True(t, slot.Occupied)
	require.Equal(t, reservation.Owner, slot.Owner)
	require.Nil(t, before[7])
	require.Nil(t, before[8])
	require.Nil(t, before[9])
	require.NotNil(t, before[executionIndex], "owner release retains the real Joined execution")
	require.NotNil(t, before[executionIndex+1], "owner release retains the original occupation")

	// Every acquired resource immediately registers its original release API.
	// LIFO cleanup finishes the reader, then releases Range and partition before
	// revoking/joining the new session. No old setup member is borrowed.
	session, err := b.OpenWorkerSession(f.ctx, "detached-physical-paid-quota-history")
	require.NoError(t, err)
	defer func() { require.NoError(t, b.ReleaseWorkerSession(context.Background(), session)) }()
	require.NotSame(t, f.session, session)
	require.NotSame(t, previousWorker.session, session)
	partition, err := b.ClaimWorkerPartition(f.ctx, session, f.ref.Partition)
	require.NoError(t, err)
	defer func() { require.NoError(t, b.ReleaseWorkerPartition(context.Background(), partition)) }()
	ranges, err := b.AcquireWorkerBudget(f.ctx, session, WorkerBudgetRange, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, b.ReleaseWorkerBudget(context.Background(), ranges)) }()
	owner, err := b.registerEnrolledPublication(f.ctx)
	require.NoError(t, err)
	defer owner.finish()
	cursor, err := b.newColdQuotaWorkerHistory(owner, workspace, intentID, f.ref, session, partition, ranges)
	require.NoError(t, err)
	if os.Getenv("SANDBOX_DETACHED_QUOTA_REFUND_PHYSICAL_NATIVE") == "1" {
		detachedPhysicalPaidQuotaRefund(t, f, cursor, sourceKeys, before)
		return
	}
	originalOrigin := cursor.origin
	for phase := 0; phase < 7; phase++ {
		detachedPhysicalQuotaHistoryRangeReady(t, f.ctx, b, ranges)
		history, err := b.stepColdQuotaWorkerHistory(owner, cursor, session, partition, ranges)
		require.NoError(t, err)
		if phase < 6 {
			require.Nil(t, history)
		} else {
			require.Same(t, cursor.history, history)
		}
		b.archiveLife.mu.Lock()
		tokens, operation := ranges.bucket.tokens, session.operation
		b.archiveLife.mu.Unlock()
		require.Zero(t, tokens, "only actual admission spends each naturally earned Range")
		require.Nil(t, operation, "each original native call must return before its turn yields")
	}
	require.Equal(t, coldQuotaWorkerHistoryDone, cursor.phase)
	require.NotNil(t, cursor.releaseProof)
	require.Nil(t, cursor.releaseProof.local)
	require.NotNil(t, cursor.releaseProof.detached)
	require.NotNil(t, cursor.releaseProof.detached.history)
	require.Same(t, cursor.releaseProof, cursor.history.profileRelease)
	require.Nil(t, cursor.history.release)
	require.Nil(t, cursor.releaseKV)
	require.Nil(t, cursor.detachedHead)
	require.Nil(t, cursor.actualTask)
	require.Same(t, cursor, cursor.self)
	require.Same(t, b, cursor.origin.backend)
	require.Same(t, owner, cursor.origin.owner)
	require.Same(t, owner.context, cursor.origin.context)
	require.Equal(t, workspace, cursor.origin.workspace)
	require.Equal(t, intentID, cursor.origin.intent)
	require.Equal(t, f.ref, cursor.origin.task)
	require.Same(t, session, cursor.origin.session)
	require.Same(t, partition, cursor.origin.partition)
	require.Same(t, ranges, cursor.origin.ranges)
	require.Equal(t, originalOrigin, cursor.origin)
	require.True(t, coldQuotaDetachedHistoryOriginMatches(cursor.history, cursor.releaseProof))

	// Count the whole retained cursor, including its complete private proof.
	// This uses the original walker and its original future-group reservation;
	// no new shared-root exemption hides source bytes, keys or decoded tuples.
	counter := &cleanupColdQuotaCounter{pointers: make(map[cleanupColdQuotaPointer]bool), keys: make(map[string]bool)}
	require.NoError(t, counter.value(reflect.ValueOf(cursor), false))
	require.LessOrEqual(t, counter.bytes, uint64(cleanupColdQuotaBytes))
	require.LessOrEqual(t, len(counter.keys)+cleanupColdQuotaGroupKeys, cleanupColdQuotaKeys)
	b.archiveLife.mu.Lock()
	bucket, operation := ranges.bucket, session.operation
	_, registered := b.archiveLife.enrolled[owner]
	b.archiveLife.mu.Unlock()
	require.True(t, registered)
	require.Nil(t, operation)
	proof, originalContext := cursor.releaseProof, owner.context
	for attempt := 0; attempt < 3; attempt++ {
		history, err := b.stepColdQuotaWorkerHistory(owner, cursor, session, partition, ranges)
		require.NoError(t, err)
		require.Same(t, cursor.history, history)
		require.Same(t, proof, cursor.releaseProof)
		require.Same(t, originalContext, owner.context)
		require.Equal(t, originalOrigin, cursor.origin)
		b.archiveLife.mu.Lock()
		afterBucket, afterOperation := ranges.bucket, session.operation
		_, stillRegistered := b.archiveLife.enrolled[owner]
		b.archiveLife.mu.Unlock()
		require.Equal(t, bucket, afterBucket, "cached done cannot even move the bucket's earning origin")
		require.Nil(t, afterOperation)
		require.True(t, stillRegistered)
	}
	require.Equal(t, before, detachedPhysicalQuotaHistoryPoints(t, f, sourceKeys))
	t.Logf("actual detached paid-reader component: seven logical paid turns, retained=%d bytes keys+reservation=%d; cached done returned the same history three times; wire dispatch/retry counts unmeasured", counter.bytes, len(counter.keys)+cleanupColdQuotaGroupKeys)
}
