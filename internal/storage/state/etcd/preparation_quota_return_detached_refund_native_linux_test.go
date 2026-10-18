//go:build linux && (amd64 || arm64)

package etcd

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// Every protocol object retained across a paid yield is included here. Aliased
// pointers are counted once by the original walker, but no phase drops its
// earlier roots from measurement merely to fit the shared workspace budget.
type detachedPhysicalQuotaRefundRoots struct {
	cursor  *coldQuotaWorkerHistory
	history *coldQuotaReturnHistory

	recovery  *coldQuotaWorkerRecovery
	recovered *coldQuotaWorkerRecoveryResult
	prepare   *coldQuotaWorkerPrepare
	prepared  *coldQuotaReturnPrepared
	journal   *coldQuotaWorkerJournalPermit
	permit    *coldQuotaWorkerCommitPermit
	postread  *coldQuotaWorkerPostread
	entry     *ColdRuntimeQuotaReturnEntry

	replayRecovery *coldQuotaWorkerRecovery
	replayResult   *coldQuotaWorkerRecoveryResult
}

func detachedPhysicalQuotaRefundMeter(t *testing.T, roots *detachedPhysicalQuotaRefundRoots, phase string) {
	t.Helper()
	origin := roots.cursor.origin
	origin.backend.archiveLife.mu.Lock()
	operation := origin.session.operation
	_, registered := origin.backend.archiveLife.enrolled[origin.owner]
	origin.backend.archiveLife.mu.Unlock()
	require.Nil(t, operation, "%s must join the actual original worker call before yielding", phase)
	require.True(t, registered, phase)
	counter := &cleanupColdQuotaCounter{pointers: make(map[cleanupColdQuotaPointer]bool), keys: make(map[string]bool)}
	require.NoError(t, counter.value(reflect.ValueOf(roots), false), phase)
	require.LessOrEqual(t, counter.bytes, uint64(cleanupColdQuotaBytes), phase)
	require.LessOrEqual(t, len(counter.keys)+cleanupColdQuotaGroupKeys, cleanupColdQuotaKeys, phase)
	t.Logf("actual detached refund retained graph phase=%s bytes=%d keys+reservation=%d", phase, counter.bytes, len(counter.keys)+cleanupColdQuotaGroupKeys)
}

// Claim readiness, like Range readiness, is calculated on a locked copy only.
// The original admission later performs the real atomic payment. No test
// writes bucket fields, refunds credit or reconstructs a worker capability.
func detachedPhysicalQuotaRefundClaimReady(t *testing.T, ctx context.Context, b *Backend, claims *WorkerBudgetShare) {
	t.Helper()
	for {
		b.archiveLife.mu.Lock()
		bucket := claims.bucket
		b.archiveLife.mu.Unlock()
		require.Positive(t, bucket.rate)
		require.GreaterOrEqual(t, bucket.capacity, uint64(1))
		now := time.Now()
		ready := dueWorkerCreditAt(bucket, now, 1)
		if !ready.After(now) {
			return
		}
		timer := time.NewTimer(time.Until(ready))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			t.Fatalf("waiting for the original earned Claim credit: %v", ctx.Err())
		}
		timer.Stop()
	}
}

// This branch consumes the same original physical source and paid session as
// the accepted history-only component. It expects a usable authenticated v2
// history and performs the actual existing quota protocol. Seventh-step failure
// remains a hard failure before any writer is exercised.
// Neither a host-issued terminal signature nor metadata seeding is permitted.
func detachedPhysicalPaidQuotaRefund(t *testing.T, f *cleanupDetachedNoEffectNativeFixture, cursor *coldQuotaWorkerHistory, sourceKeys []string, before []*mvccpb.KeyValue) {
	t.Helper()
	require.Equal(t, "1", os.Getenv("SANDBOX_DETACHED_QUOTA_REFUND_PHYSICAL_NATIVE"))
	b, origin := f.b, cursor.origin
	owner, session, partition, ranges := origin.owner, origin.session, origin.partition, origin.ranges
	claims, err := b.AcquireWorkerBudget(f.ctx, session, WorkerBudgetClaim, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, b.ReleaseWorkerBudget(context.Background(), claims)) }()
	roots := &detachedPhysicalQuotaRefundRoots{cursor: cursor}
	detachedPhysicalQuotaRefundMeter(t, roots, "constructed")

	for phase := 0; phase < 7; phase++ {
		detachedPhysicalQuotaHistoryRangeReady(t, f.ctx, b, ranges)
		history, err := b.stepColdQuotaWorkerHistory(owner, cursor, session, partition, ranges)
		require.NoError(t, err, "usable v2 history after logical paid phase %d", phase+1)
		if phase < 6 {
			require.Nil(t, history)
		} else {
			require.NotNil(t, history, "the original caller must receive usable quota-writer input")
			roots.history = history
		}
		require.Equal(t, origin, cursor.origin)
		detachedPhysicalQuotaRefundMeter(t, roots, "history")
	}
	require.Equal(t, coldQuotaWorkerHistoryDone, cursor.phase)
	require.NotNil(t, cursor.releaseProof)
	require.Nil(t, cursor.releaseProof.local)
	require.NotNil(t, cursor.releaseProof.detached)
	require.Nil(t, roots.history.release, "v2 must not be cast into local Complete/archive proof")
	require.Equal(t, before, detachedPhysicalQuotaHistoryPoints(t, f, sourceKeys), "authentication alone cannot refund quota")

	if os.Getenv("SANDBOX_DETACHED_QUOTA_REFUND_UNKNOWN_PHYSICAL_NATIVE") == "1" {
		detachedPhysicalQuotaRefundUnknown(t, f, roots, claims, sourceKeys, before)
		return
	}

	// Absent return and absent journal are two separately paid original reads.
	roots.recovery, err = b.newColdQuotaWorkerRecovery(owner, roots.history, session, partition, ranges, claims)
	require.NoError(t, err)
	detachedPhysicalQuotaRefundMeter(t, roots, "recovery-constructed")
	for phase := 0; phase < 2; phase++ {
		detachedPhysicalQuotaHistoryRangeReady(t, f.ctx, b, ranges)
		result, err := b.stepColdQuotaWorkerRecovery(owner, roots.history, roots.recovery, session, partition, ranges, claims)
		require.NoError(t, err)
		if phase == 0 {
			require.Nil(t, result)
		} else {
			require.NotNil(t, result)
			require.Nil(t, result.entry)
			require.Nil(t, result.previous)
			require.Nil(t, result.aborted)
			roots.recovered = result
		}
		detachedPhysicalQuotaRefundMeter(t, roots, "recovery")
	}
	roots.prepare, err = b.newColdQuotaWorkerPrepare(owner, roots.history, session, partition, ranges)
	require.NoError(t, err)
	detachedPhysicalQuotaRefundMeter(t, roots, "prepare-constructed")
	for phase := 0; phase < 2; phase++ {
		detachedPhysicalQuotaHistoryRangeReady(t, f.ctx, b, ranges)
		prepared, err := b.stepColdQuotaWorkerPrepare(owner, roots.history, roots.prepare, session, partition, ranges)
		require.NoError(t, err)
		if phase == 0 {
			require.Nil(t, prepared)
		} else {
			require.NotNil(t, prepared)
			roots.prepared = prepared
		}
		detachedPhysicalQuotaRefundMeter(t, roots, "prepare")
	}

	// Refusal copies never enter journal or Grant. The real prepared mutation
	// and locator remain untouched for the actual protocol immediately below.
	prepared := roots.prepared
	require.NoError(t, b.validateColdQuotaReturnFrozenValue(prepared))
	originalReference := prepared.reference
	returnWrite := -1
	for i, write := range prepared.mutation.Writes {
		if write.Key == prepared.returnKey {
			require.Equal(t, -1, returnWrite)
			returnWrite = i
		}
	}
	require.NotEqual(t, -1, returnWrite)
	originalValue := append([]byte(nil), prepared.mutation.Writes[returnWrite].Value...)
	badBytes := *prepared
	badBytes.mutation.Writes = append([]Write(nil), prepared.mutation.Writes...)
	badBytes.mutation.Writes[returnWrite].Value = append([]byte(nil), originalValue...)
	require.NotEmpty(t, badBytes.mutation.Writes[returnWrite].Value)
	badBytes.mutation.Writes[returnWrite].Value[0] ^= 1
	require.ErrorIs(t, b.validateColdQuotaReturnFrozenValue(&badBytes), ErrInvalidMutation)
	badLocator := *prepared
	badLocator.reference.AttemptID = uuid.NewString()
	require.ErrorIs(t, b.validateColdQuotaReturnFrozenValue(&badLocator), ErrInvalidMutation)
	require.Equal(t, originalReference, prepared.reference)
	require.Equal(t, originalValue, prepared.mutation.Writes[returnWrite].Value)
	require.NoError(t, b.validateColdQuotaReturnFrozenValue(prepared))

	// Journal and the complete Stage cohort spend independent earned turns.
	// Actual transport uses ActiveConnection.Invoke; no KV/Lease wrapper is
	// claimed to count these frames or their internal retry dispatches.
	detachedPhysicalQuotaHistoryRangeReady(t, f.ctx, b, ranges)
	detachedPhysicalQuotaRefundClaimReady(t, f.ctx, b, claims)
	roots.journal, err = b.journalColdRuntimeQuotaReturnWorker(owner, roots.prepared, session, partition, ranges, claims, roots.recovered.previous, roots.recovered.aborted)
	require.NoError(t, err)
	require.NotNil(t, roots.journal)
	detachedPhysicalQuotaRefundMeter(t, roots, "journal-ack")
	detachedPhysicalQuotaHistoryRangeReady(t, f.ctx, b, ranges)
	detachedPhysicalQuotaRefundClaimReady(t, f.ctx, b, claims)
	roots.permit, err = b.continueColdRuntimeQuotaReturnWorker(owner, roots.journal, session, partition, ranges, claims)
	require.NoError(t, err)
	require.NotNil(t, roots.permit)
	detachedPhysicalQuotaRefundMeter(t, roots, "commit-original-revoke-joined")
	roots.postread, err = b.newColdQuotaWorkerPostread(owner, roots.permit, session, partition, ranges)
	require.NoError(t, err)
	detachedPhysicalQuotaRefundMeter(t, roots, "postread-constructed")
	var entry *ColdRuntimeQuotaReturnEntry
	for phase := 0; phase < 2; phase++ {
		detachedPhysicalQuotaHistoryRangeReady(t, f.ctx, b, ranges)
		entry, err = b.stepColdQuotaWorkerPostread(owner, roots.permit, roots.postread, session, partition, ranges)
		require.NoError(t, err)
		if phase == 0 {
			require.Nil(t, entry)
		} else {
			require.NotNil(t, entry)
		}
		roots.entry = entry
		detachedPhysicalQuotaRefundMeter(t, roots, "postread")
	}
	require.Equal(t, f.ref, entry.Task)
	require.Equal(t, roots.history.reservation.Owner, entry.Owner)
	require.Equal(t, roots.history.reservation.Quota, entry.Quota)
	require.Equal(t, roots.prepared.reference, roots.permit.reference)
	returnPoints := detachedPhysicalQuotaHistoryPoints(t, f, []string{roots.prepared.returnKey})
	record, err := decodeDetachedColdRuntimeQuotaReturn(returnPoints[0])
	require.NoError(t, err)
	require.Equal(t, cursor.releaseProof.detached.record, record.Release)
	require.Equal(t, entry.SHA256, workspaceSyncIntentDigest(string(returnPoints[0].Value)))
	_, receiptKey, err := b.stageKeys(roots.prepared.reference)
	require.NoError(t, err)
	receipt := detachedPhysicalQuotaHistoryPoints(t, f, []string{receiptKey})
	outcome, err := decodeReceipt(receipt[0], roots.prepared.reference)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	require.Equal(t, returnPoints[0].CreateRevision, receipt[0].CreateRevision)
	journalKey, err := b.namespace.coldRuntimeQuotaAttemptKey(origin.workspace.Partition(), origin.intent)
	require.NoError(t, err)
	journal := detachedPhysicalQuotaHistoryPoints(t, f, []string{journalKey})
	attempt, err := b.decodeColdRuntimeQuotaAttempt(roots.history, journal[0])
	require.NoError(t, err)
	require.Equal(t, roots.prepared.reference, attempt.Reference)

	slotKey, freeKey, err := b.namespace.coldSlotKeys(entry.Quota)
	require.NoError(t, err)
	free := detachedPhysicalQuotaHistoryPoints(t, f, []string{slotKey, freeKey})
	require.NotNil(t, free[0])
	require.NotNil(t, free[1])
	var slot coldSlotRecord
	require.NoError(t, coldDecode(free[0], &slot))
	require.False(t, slot.Occupied)
	require.Equal(t, entry.Quota, slot.Slot)
	require.Equal(t, free[0].Value, free[1].Value)
	public, err := b.SettleColdRuntimeQuota(f.ctx, origin.workspace, origin.intent, f.ref)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, public.Outcome)
	require.Equal(t, entry, public.Entry)
	require.Equal(t, roots.prepared.reference, public.Reference)

	// Use the original Acquire/Claim/Reserve producers to reuse the freed slot.
	// This creates ordinary pending creation metadata, not a replacement runtime
	// or permission to sign another terminal proof. Never seed a slot by Put.
	detachedPhysicalQuotaRefundReuse(t, f, entry.Quota)
	reused := detachedPhysicalQuotaHistoryPoints(t, f, []string{slotKey, freeKey})
	require.NotNil(t, reused[0])
	require.Nil(t, reused[1])
	var later coldSlotRecord
	require.NoError(t, coldDecode(reused[0], &later))
	require.True(t, later.Occupied)
	require.Equal(t, entry.Quota.ID, later.Slot.ID)
	require.Greater(t, later.Slot.Epoch, entry.Quota.Epoch)
	require.NotEqual(t, entry.Owner, later.Owner)
	public, err = b.SettleColdRuntimeQuota(f.ctx, origin.workspace, origin.intent, f.ref)
	require.NoError(t, err, "committed public replay must precede the now-reused mutable quota")
	require.Equal(t, OutcomeCommitted, public.Outcome)
	require.Equal(t, entry, public.Entry)
	require.Equal(t, roots.prepared.reference, public.Reference)
	require.Equal(t, reused, detachedPhysicalQuotaHistoryPoints(t, f, []string{slotKey, freeKey}))

	// The paid recovery replay also authenticates return then receipt, with no
	// journal/slot phase and no new prepared or Commit permission.
	roots.replayRecovery, err = b.newColdQuotaWorkerRecovery(owner, roots.history, session, partition, ranges, claims)
	require.NoError(t, err)
	detachedPhysicalQuotaRefundMeter(t, roots, "replay-constructed")
	for phase := 0; phase < 2; phase++ {
		detachedPhysicalQuotaHistoryRangeReady(t, f.ctx, b, ranges)
		result, err := b.stepColdQuotaWorkerRecovery(owner, roots.history, roots.replayRecovery, session, partition, ranges, claims)
		require.NoError(t, err)
		if phase == 0 {
			require.Nil(t, result)
			require.Equal(t, coldQuotaRecoveryWantCommittedReceipt, roots.replayRecovery.phase)
		} else {
			require.NotNil(t, result)
			require.Equal(t, entry, result.entry)
			roots.replayResult = result
		}
		detachedPhysicalQuotaRefundMeter(t, roots, "committed-replay")
	}
	for i, key := range sourceKeys {
		if i >= 6 && i <= 9 {
			continue // The expected refund/reuse changes slot/free/return/journal.
		}
		require.Equal(t, before[i], detachedPhysicalQuotaHistoryPoints(t, f, []string{key})[0], "original source remains unchanged: %s", key)
	}
	require.Equal(t, returnPoints, detachedPhysicalQuotaHistoryPoints(t, f, []string{roots.prepared.returnKey}))
	require.Equal(t, journal, detachedPhysicalQuotaHistoryPoints(t, f, []string{journalKey}))
	require.Equal(t, receipt, detachedPhysicalQuotaHistoryPoints(t, f, []string{receiptKey}))
	require.Equal(t, reused, detachedPhysicalQuotaHistoryPoints(t, f, []string{slotKey, freeKey}))
	t.Log("actual v2 refund: paid recovery/prepare/journal/Stage/original-Revoke/postread, public committed replay, genuine quota reuse and paid replay; all retained phases metered; P deletion/managed admission and wire dispatch counts are not proved")
	if os.Getenv("SANDBOX_DETACHED_OCCUPATION_SETTLE_PHYSICAL_NATIVE") == "1" {
		detachedPhysicalSettleOccupation(t, f, roots, sourceKeys)
	}
}

func detachedPhysicalQuotaRefundReuse(t *testing.T, f *cleanupDetachedNoEffectNativeFixture, quota ColdSlotReference) {
	t.Helper()
	idempotency := uuid.NewString()
	workspace, err := f.b.DetachedIdentity("detached-refund-reuse", idempotency)
	require.NoError(t, err)
	input := AcquireIntentInput{
		Workspace: workspace, Principal: "detached-refund-reuse", IdempotencyKey: idempotency,
		RequestID: uuid.NewString(), IntentID: uuid.NewString(), SandboxID: uuid.NewString(), SnapshotVersion: uuid.NewString(),
		Payload: json.RawMessage(`{"fixture":"lawful quota reuse"}`), Now: time.Now().UTC(), TTL: time.Hour,
	}
	acquired, err := f.b.AcquireIntent(f.ctx, input)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, acquired.Outcome)
	creation, err := f.b.ClaimCreation(f.ctx, workspace, input.IntentID, "detached-refund-reuse", 30*time.Second)
	require.NoError(t, err)
	defer func() { require.NoError(t, f.b.ReleaseCreationClaim(context.Background(), creation)) }()
	reserved, err := f.b.ReserveColdPreparation(f.ctx, creation, quota.Partition)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, reserved.Outcome)
	require.NotNil(t, reserved.Record)
	require.Equal(t, quota.Kind, reserved.Record.Quota.Kind)
	require.Equal(t, quota.Partition, reserved.Record.Quota.Partition)
	require.Equal(t, quota.ID, reserved.Record.Quota.ID)
	require.Greater(t, reserved.Record.Quota.Epoch, quota.Epoch)
}
