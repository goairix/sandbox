//go:build linux

package etcd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// These bounded diagnostics preserve all acceptance assertions. They identify
// the original failed constructor/graph root without changing production caps
// or manufacturing a successful preflight after a rejection.
func prepareCleanupDetachedRecoveryForTest(t *testing.T, f *cleanupDetachedNoEffectNativeFixture) *cleanupDetachedNoEffectRecovery {
	t.Helper()
	gate, err := f.b.prepareCleanupDetachedNoEffectRecovery(f.ctx, f.job)
	if err != nil {
		t.Logf("recovery preflight error=%v original-job=%v ctx=%v", err, f.b.validateCleanupDetachedRecoveryJob(f.job), f.job.ctx.Err())
		h, historyErr := f.b.readCleanupDetachedNoEffectHistory(f.ctx, f.ref)
		if historyErr == nil {
			t.Logf("actual history Joined=%t confirmed=%s exact-E=%t exact-S=%t", h.state.record.Run.Joined, h.state.record.Confirmed, h.state.execution == f.job.state.execution, *h.state.slot == *f.job.state.slot)
		} else {
			t.Logf("actual history error=%v", historyErr)
		}
		if d := f.job.detachedNoEffect; d != nil {
			for _, root := range []struct {
				name  string
				value any
			}{
				{"draft", d}, {"claim", d.claim}, {"prepared", d.prepared}, {"close", d.claim.closeDraft}, {"quiescence", d.claim.quiescenceDraft}, {"final-sync", d.claim.finalSyncDraft}, {"state", d.state}, {"wire", d.value},
			} {
				bytes, memoryErr := cleanupDetachedNoEffectGraphBytes(f.b, f.job, f.worker, f.job.ctx, workspaceSyncLocalNativeWorker, root.value)
				t.Logf("original retained root=%s partial-bytes=%d error=%v", root.name, bytes, memoryErr)
			}
		}
	}
	require.NoError(t, err)
	return gate
}

// This component fixture keeps the original writer Unknown without dispatching
// its settlement Put. The old real Claim is actually released. Its Run has not
// joined, so a missing lease/settlement cannot yet authorize recovery.
func cleanupDetachedUnknownRecoveryFixture(t *testing.T) (*cleanupDetachedNoEffectNativeFixture, *cleanupDetachedNoEffectDraft) {
	t.Helper()
	f := newCleanupDetachedNoEffectNativeFixture(t)
	return f, cleanupDetachedMakeWriterUnknown(t, f)
}

// Keep the same actual fixture while allowing a test to retain original
// Prepare results before this helper permanently releases its real Claim.
func cleanupDetachedMakeWriterUnknown(t *testing.T, f *cleanupDetachedNoEffectNativeFixture) *cleanupDetachedNoEffectDraft {
	t.Helper()
	draft, err := f.b.prepareCleanupDetachedNoEffect(f.ctx, f.job, f.drained, f.destination)
	require.NoError(t, err)
	key, err := f.b.namespace.taskDetachedNoEffectKey(f.ref)
	require.NoError(t, err)
	original := f.b.client.KV
	writes := 0
	f.b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		if putsKey(key)(yes) {
			writes++
			return nil, context.DeadlineExceeded
		}
		return original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
	}}
	t.Cleanup(func() { f.b.client.KV = original })
	result, err := f.b.commitCleanupDetachedNoEffect(f.ctx, f.job, draft)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, OutcomeUnknown, result.Outcome)
	require.Equal(t, 1, writes)
	require.NoError(t, f.b.ReleaseTaskClaim(f.ctx, f.job.claim))
	f.job.claim = nil
	// Later fresh attempts use the ordinary native client. The original draft
	// is still attached and latched, so restoring transport cannot resend it.
	f.b.client.KV = original
	return draft
}

func TestCleanupDetachedNoEffectNativeRecoveryRequiresActualJoined(t *testing.T) {
	f, draft := cleanupDetachedUnknownRecoveryFixture(t)
	quiescenceBefore := *draft.claim.quiescenceDraft
	closeValueBefore := draft.claim.closeDraft.recordValue
	quiescenceBefore.certificate = bytes.Clone(quiescenceBefore.certificate)
	priorBefore := *quiescenceBefore.prior
	priorBefore.proof = bytes.Clone(priorBefore.proof)
	ticketBefore := quiescenceBefore.ticket.Wire()
	closeStage, quiescenceStage := draft.claim.closeDraft.stage, draft.claim.quiescenceDraft.stage
	require.NotNil(t, closeStage)
	require.NotNil(t, quiescenceStage)
	gate, err := f.b.prepareCleanupDetachedNoEffectRecovery(f.ctx, f.job)
	require.Error(t, err, "real Claim release alone cannot join the native Run")
	require.Nil(t, gate)
	require.Nil(t, f.job.detachedRecovery)
	require.Same(t, draft, f.job.detachedNoEffect)
	require.Same(t, closeStage, draft.claim.closeDraft.stage, "lease loss cannot retire an unjoined Run's Stage")
	require.Same(t, quiescenceStage, draft.claim.quiescenceDraft.stage)
	require.Equal(t, quiescenceBefore, *draft.claim.quiescenceDraft)
	require.Equal(t, priorBefore, *draft.claim.quiescenceDraft.prior)
	require.Equal(t, ticketBefore, draft.claim.quiescenceDraft.ticket.Wire())
	require.Equal(t, closeValueBefore, draft.claim.closeDraft.recordValue)

	// Deliberately forge a schema-valid diagnostic Joined projection while
	// leaving native E untouched. This negative DTO supplies no join authority.
	original := f.job.state
	forged := *original
	run := *original.record.Run
	run.Joined = true
	forged.record.Run = &run
	forged.execution.value, err = encodeCleanupExecution(forged.record)
	require.NoError(t, err)
	f.job.state = &forged
	defer func() { f.job.state = original }()
	gate, err = f.b.prepareCleanupDetachedNoEffectRecovery(f.ctx, f.job)
	require.Error(t, err)
	require.Nil(t, gate)
	require.Nil(t, f.job.detachedRecovery)
	require.Same(t, closeStage, draft.claim.closeDraft.stage, "a diagnostic Joined DTO cannot retire original Stage ownership")
	require.Same(t, quiescenceStage, draft.claim.quiescenceDraft.stage)
	require.Equal(t, quiescenceBefore, *draft.claim.quiescenceDraft)
	require.Equal(t, priorBefore, *draft.claim.quiescenceDraft.prior)
	require.Equal(t, ticketBefore, draft.claim.quiescenceDraft.ticket.Wire())
	require.Equal(t, closeValueBefore, draft.claim.closeDraft.recordValue)
	actual, err := f.raw.Get(f.ctx, original.execution.key)
	require.NoError(t, err)
	require.Len(t, actual.Kvs, 1)
	require.Equal(t, original.execution.value, string(actual.Kvs[0].Value))
}

// These snapshots are assertion-only diagnostics. They never enter a metadata
// reader, gate, counter or transaction as replacement capability/evidence.
func requireCleanupDetachedQuiescencePayloadRetired(t *testing.T, q *taskQuiescenceDraft, original taskQuiescenceDraft, prior taskQuiescencePrerequisite) {
	t.Helper()
	require.Nil(t, q.certificate)
	require.Nil(t, q.issuer)
	require.Empty(t, q.issuerValue)
	require.Equal(t, p.TaskUserQuiescenceTicketClaims{}, q.claims)
	require.Empty(t, q.ticket.Wire())
	require.Empty(t, q.ticket.Digest())
	require.Empty(t, q.recordValue)
	require.Same(t, original.prior, q.prior)
	require.Nil(t, q.prior.entry)
	require.Empty(t, q.prior.intent.value)
	require.Empty(t, q.prior.proof)

	require.Equal(t, original.task, q.task)
	require.Equal(t, original.attemptReference, q.attemptReference)
	require.Equal(t, original.reservation, q.reservation)
	require.Equal(t, original.reservationValue, q.reservationValue)
	require.Equal(t, original.reservationSent, q.reservationSent)
	require.Equal(t, original.commandID, q.commandID)
	require.Equal(t, original.reference, q.reference)
	require.Equal(t, original.deadline, q.deadline)
	require.Equal(t, original.issuerKey, q.issuerKey)
	require.Equal(t, original.outcome, q.outcome)
	require.Equal(t, original.cleaned, q.cleaned)
	require.Equal(t, original.lost, q.lost)
	require.Equal(t, original.deliveryAttempted, q.deliveryAttempted)
	require.Equal(t, prior.intent.key, q.prior.intent.key)
	require.Equal(t, prior.intent.create, q.prior.intent.create)
	require.Equal(t, prior.intent.mod, q.prior.intent.mod)
	require.Equal(t, prior.intent.lease, q.prior.intent.lease)
	require.Equal(t, prior.receiptKey, q.prior.receiptKey)
	require.Equal(t, prior.receiptRevision, q.prior.receiptRevision)
}

// Only completed, cleaned mutation/authorization payloads may leave the dead
// Claim after actual native join. Original prerequisite, coordinates and the
// Unknown writer's one-send latch remain until the recovery actor replies.
func TestCleanupDetachedNoEffectNativeRecoveryRetiresCompletedStagesAfterJoin(t *testing.T) {
	f := newCleanupDetachedNoEffectNativeFixture(t)
	claim := f.job.claim
	closeResult, err := f.b.PrepareTaskCloseData(f.ctx, claim)
	require.NoError(t, err)
	require.NotNil(t, closeResult.Prepared)
	quiescenceResult, err := f.b.PrepareTaskQuiescence(f.ctx, claim, f.destination)
	require.NoError(t, err)
	require.NotNil(t, quiescenceResult.Prepared)
	draft := cleanupDetachedMakeWriterUnknown(t, f)
	closeDraft, quiescenceDraft := claim.closeDraft, claim.quiescenceDraft
	quiescenceBefore, priorBefore := *quiescenceDraft, *quiescenceDraft.prior
	closeBefore := *closeDraft
	require.NotNil(t, closeDraft.stage)
	require.NotNil(t, quiescenceDraft.stage)
	require.Equal(t, OutcomeCommitted, closeDraft.outcome)
	require.Equal(t, OutcomeCommitted, quiescenceDraft.outcome)
	require.True(t, closeDraft.cleaned)
	require.True(t, quiescenceDraft.cleaned)
	prepared, state := draft.prepared, draft.state
	wire, execution := draft.value, bytes.Clone(draft.record.ExecutionValue)
	attempt, binding := draft.record.AttemptID, draft.record.BindingDigest
	none := claim.finalSyncDraft
	require.NotNil(t, none)
	noneValue := none.value

	f.job.effectsJoined = true
	require.NoError(t, f.b.joinCleanupRun(f.ctx, f.job))
	require.Nil(t, f.job.owner, "the original actor owner already returned before physical preparation")
	gate, prepareErr := f.b.prepareCleanupDetachedNoEffectRecovery(f.ctx, f.job)

	// Reuse genuine original Prepare results, never a reconstructed dispatch
	// DTO. Lost Claim calls must not allocate a new Stage or send application
	// commands, whether preparation succeeded or still rejected its byte cap.
	lease := &creationFaultLease{Lease: f.b.client.Lease}
	f.b.client.Lease = lease
	defer func() { f.b.client.Lease = lease.Lease }()
	effects, queries := f.effectCount.Load(), f.queryCount.Load()
	_, err = f.b.PrepareTaskCloseData(f.ctx, claim)
	require.Error(t, err)
	_, err = f.b.PrepareTaskQuiescence(f.ctx, claim, f.destination)
	require.Error(t, err)
	_, err = f.b.DeliverTaskCloseData(f.ctx, closeResult.Prepared, f.destination)
	require.Error(t, err)
	_, err = f.b.DeliverTaskQuiescence(f.ctx, quiescenceResult.Prepared, f.destination)
	require.Error(t, err)
	require.Zero(t, lease.grants.Load())
	require.Equal(t, effects, f.effectCount.Load())
	require.Equal(t, queries, f.queryCount.Load())

	require.True(t, closeDraft.stage == nil, "actual joined dead Claim must release its completed close Stage tree")
	require.True(t, quiescenceDraft.stage == nil, "actual joined dead Claim must release its completed quiescence Stage tree")
	requireCleanupDetachedQuiescencePayloadRetired(t, quiescenceDraft, quiescenceBefore, priorBefore)
	closeBefore.stage = nil
	closeBefore.recordValue = ""
	require.True(t, closeDraft.recordValue == "", "the dead original close authorization record cache must retire")
	require.Equal(t, closeBefore, *closeDraft, "all other original close payload and coordinates remain intact")
	require.NoError(t, prepareErr)
	require.Same(t, gate, f.job.detachedRecovery)
	require.Same(t, draft, f.job.detachedNoEffect)
	require.Same(t, claim, draft.claim)
	require.Same(t, prepared, draft.prepared)
	require.Same(t, state, draft.state)
	require.Same(t, closeDraft, claim.closeDraft)
	require.Same(t, quiescenceDraft, claim.quiescenceDraft)
	require.Equal(t, wire, draft.value)
	require.Equal(t, execution, draft.record.ExecutionValue)
	require.Equal(t, attempt, draft.record.AttemptID)
	require.Equal(t, binding, draft.record.BindingDigest)
	require.True(t, draft.writeAttempted)
	require.Same(t, none, claim.finalSyncDraft)
	require.Equal(t, noneValue, none.value)

	// None's original uncertain-write path can still diagnose committed native
	// history with a dead Claim. Retiring Q send caches must not remove the
	// original none record/value, Claim fences or prepared native history pins.
	originalKV := f.b.client.KV
	noneKey, err := f.b.namespace.taskFinalSyncAttemptKey(f.ref)
	require.NoError(t, err)
	nonePuts := 0
	f.b.client.KV = &finalSyncRecordingKV{KV: originalKV, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		if putsKey(noneKey)(yes) {
			nonePuts++
		}
		return originalKV.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
	}}
	defer func() { f.b.client.KV = originalKV }()
	noneResult, err := f.b.ReserveTaskFinalSync(f.ctx, claim, prepared, f.destination, none.record.Plan)
	require.Error(t, err)
	require.Equal(t, OutcomeCommitted, noneResult.Outcome)
	require.Equal(t, none.record.reference(), noneResult.Reference)
	require.Zero(t, nonePuts)
	require.Zero(t, lease.grants.Load())
	require.Equal(t, effects, f.effectCount.Load())
	require.Equal(t, queries, f.queryCount.Load())
	owned, err := f.b.cleanupDetachedNoEffectOwnedBytes(f.job)
	require.NoError(t, err)
	require.LessOrEqual(t, owned, uint64(128<<10))
}

func TestCleanupDetachedNoEffectNativeJoinedAbsenceNewRunFreshDrain(t *testing.T) {
	f, oldDraft := cleanupDetachedUnknownRecoveryFixture(t)
	oldRun, oldEmpty := f.job.runID, f.drained.emptyRevision
	f.job.effectsJoined = true
	require.NoError(t, f.b.joinCleanupRun(f.ctx, f.job))
	joined := f.job.state.execution
	require.True(t, f.job.state.record.Run.Joined)
	history, err := f.b.readCleanupDetachedNoEffectHistory(f.ctx, f.ref)
	require.NoError(t, err)
	settlementKey, err := f.b.namespace.taskDetachedNoEffectKey(f.ref)
	require.NoError(t, err)
	gate := prepareCleanupDetachedRecoveryForTest(t, f)
	require.Same(t, gate, f.job.detachedRecovery)
	require.Same(t, oldDraft, f.job.detachedNoEffect)
	copyJob := *f.job
	_, err = f.b.prepareCleanupDetachedNoEffectRecovery(f.ctx, &copyJob)
	require.Error(t, err)

	// A copied gate cannot borrow the real registry job or actor owner.
	copyGate := *gate
	f.job.detachedRecovery = &copyGate
	_, err = f.worker.cleanupContinuation(f.job, &cleanupLocalContinuation{kind: "resume-detached-noeffect"})
	require.Error(t, err)
	f.job.detachedRecovery = gate
	priorQueries := f.queryCount.Load()
	original := f.b.client.KV
	runWrites := 0
	f.b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		if putsKey(joined.key)(yes) {
			runWrites++
			require.Equal(t, 51, len(cmps)+len(yes)+len(no))
			require.Len(t, cmps, 48)
			require.Len(t, yes, 1)
			require.Len(t, no, 2)
			base, err := f.b.cleanupEvidenceComparisons(f.job.owner, f.job.evidence)
			require.NoError(t, err)
			require.Equal(t, base, cmps[:32], "original worker and T/I/receipt fences remain exact")
			for _, expected := range append(joined.comparisons(), history.state.slot.comparisons()...) {
				require.Contains(t, cmps, expected)
			}
			for _, pin := range []taskFence{history.quiescencePin.intent, history.quiescencePin.receipt, history.finalSyncFence, history.snapshotFence, history.intentFence, history.publicationFence} {
				require.Contains(t, cmps, clientv3.Compare(clientv3.ModRevision(pin.key), "=", pin.mod))
			}
			require.Contains(t, cmps, clientv3.Compare(clientv3.ModRevision(history.quiescencePin.reservation.key), "=", history.quiescencePin.reservation.revision))
			require.Contains(t, cmps, clientv3.Compare(clientv3.CreateRevision(settlementKey), "=", 0))
			require.Same(t, gate, f.job.detachedRecovery, "gate remains owned until actual actor reply")
			require.Same(t, oldDraft, f.job.detachedNoEffect)
			p := f.b.cleanupProjection("detached-recovery-run", cmps, taskProtoWrites([]Write{{Key: joined.key, Value: yes[0].ValueBytes()}}), taskProtoPutResponse(1))
			require.NoError(t, validateCleanupWire(p))
			require.LessOrEqual(t, p.request.Size(), 128<<10)
			require.LessOrEqual(t, p.failure.Size(), 256<<10)
		}
		return original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
	}}
	_, err = f.worker.cleanupContinuation(f.job, &cleanupLocalContinuation{kind: "resume-detached-noeffect"})
	f.b.client.KV = original
	require.NoError(t, err)
	require.Equal(t, 1, runWrites)
	require.NotEqual(t, oldRun, f.job.runID)
	require.Equal(t, f.job.runID, f.job.state.record.Run.ID)
	require.False(t, f.job.state.record.Run.Joined)
	require.Greater(t, f.job.state.execution.mod, joined.mod)
	require.Nil(t, f.job.detachedRecovery)
	require.Nil(t, f.job.detachedNoEffect)
	require.False(t, f.job.effectsJoined)

	noneKey, err := f.b.namespace.taskFinalSyncAttemptKey(f.ref)
	require.NoError(t, err)
	noneBefore, err := f.raw.Get(f.ctx, noneKey)
	require.NoError(t, err)
	require.Len(t, noneBefore.Kvs, 1)
	// The genuine public Claim proves only this private metadata component;
	// it does not exercise the managed Claim/inventory admission pipeline.
	f.job.claim, err = f.b.ClaimTask(f.job.ctx, f.ref, "recovery-component", 30*time.Second)
	require.NoError(t, err)
	f.drained, err = f.b.PrepareAfterDualDrain(f.ctx, f.job.claim, f.destination)
	require.NoError(t, err)
	require.Greater(t, f.queryCount.Load(), priorQueries)
	require.Greater(t, f.drained.emptyRevision, oldEmpty)
	require.GreaterOrEqual(t, f.drained.emptyRevision, f.job.claim.reference.CreateRevision)
	require.NoError(t, f.job.progress("drained", f.job.pending("final-sync", StageReference{}), nil))
	draft, err := f.b.prepareCleanupDetachedNoEffect(f.ctx, f.job, f.drained, f.destination)
	require.NoError(t, err)
	require.NotEqual(t, oldDraft.record.AttemptID, draft.record.AttemptID)
	require.NotEqual(t, oldDraft.record.WriterClaim, draft.record.WriterClaim)
	require.Equal(t, oldDraft.record.FinalSyncClaim, draft.record.FinalSyncClaim)
	result, err := f.b.commitCleanupDetachedNoEffect(f.ctx, f.job, draft)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	noneAfter, err := f.raw.Get(f.ctx, noneKey)
	require.NoError(t, err)
	require.Equal(t, noneBefore.Kvs, noneAfter.Kvs, "a replacement Claim must never overwrite or re-reserve the old none plan")
}

// Unknown Run writes keep their original candidate and both owned roots until
// the actor really confirms the native result. A repeated continuation reads
// that same candidate and never creates another Run/Claim UUID or another Put.
func TestCleanupDetachedNoEffectNativeRecoveryLostACKKeepsCandidate(t *testing.T) {
	f, oldDraft := cleanupDetachedUnknownRecoveryFixture(t)
	f.job.effectsJoined = true
	require.NoError(t, f.b.joinCleanupRun(f.ctx, f.job))
	gate := prepareCleanupDetachedRecoveryForTest(t, f)
	key := f.job.state.execution.key
	original := f.b.client.KV
	writes := 0
	var submitted string
	f.b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		response, err := original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
		if putsKey(key)(yes) {
			writes++
			submitted = string(yes[0].ValueBytes())
			require.NoError(t, err)
			require.True(t, response.Succeeded)
			return nil, context.DeadlineExceeded
		}
		return response, err
	}}
	t.Cleanup(func() { f.b.client.KV = original })
	_, err := f.worker.cleanupContinuation(f.job, &cleanupLocalContinuation{kind: "resume-detached-noeffect"})
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Same(t, gate, f.job.detachedRecovery)
	require.Same(t, oldDraft, f.job.detachedNoEffect)
	candidateRun := f.job.runID
	require.Contains(t, submitted, candidateRun)
	_, err = f.worker.cleanupContinuation(f.job, &cleanupLocalContinuation{kind: "resume-detached-noeffect"})
	require.NoError(t, err)
	require.Equal(t, 1, writes)
	require.Equal(t, candidateRun, f.job.runID)
	require.Equal(t, submitted, f.job.state.execution.value)
	require.Nil(t, f.job.detachedRecovery)
	require.Nil(t, f.job.detachedNoEffect)
	require.False(t, strings.Contains(submitted, `"joined":true`))
}

// Present settlement remains the original historical fact across later joined
// Runs. These are metadata-only recovery turns: no new physical effect or
// managed Claim admission is claimed by either iteration.
func TestCleanupDetachedNoEffectNativeRecoveryHistoricalFactAcrossRuns(t *testing.T) {
	f := newCleanupDetachedNoEffectNativeFixture(t)
	draft, err := f.b.prepareCleanupDetachedNoEffect(f.ctx, f.job, f.drained, f.destination)
	require.NoError(t, err)
	committed, err := f.b.commitCleanupDetachedNoEffect(f.ctx, f.job, draft)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, committed.Outcome)
	fact := committed.Entry
	key, err := f.b.namespace.taskDetachedNoEffectKey(f.ref)
	require.NoError(t, err)
	require.NoError(t, f.b.ReleaseTaskClaim(f.ctx, f.job.claim))
	f.job.claim = nil
	for n := 0; n < 2; n++ {
		f.job.effectsJoined = true // all this original job's native/transport calls returned
		require.NoError(t, f.b.joinCleanupRun(f.ctx, f.job))
		previousRun := f.job.runID
		prepareCleanupDetachedRecoveryForTest(t, f)
		original := f.b.client.KV
		writes := 0
		f.b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
			if putsKey(f.job.state.execution.key)(yes) {
				writes++
				require.Equal(t, 51, len(cmps)+len(yes)+len(no))
				require.Contains(t, cmps, clientv3.Compare(clientv3.ModRevision(key), "=", fact.kv.ModRevision))
				require.NotContains(t, cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
			}
			return original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
		}}
		_, err = f.worker.cleanupContinuation(f.job, &cleanupLocalContinuation{kind: "resume-detached-noeffect"})
		f.b.client.KV = original
		require.NoError(t, err)
		require.Equal(t, 1, writes)
		require.NotEqual(t, previousRun, f.job.runID)
		loaded, err := f.b.loadTaskDetachedNoEffect(f.ctx, f.ref)
		require.NoError(t, err)
		require.Equal(t, fact.record, loaded.record)
		require.Equal(t, fact.kv, loaded.kv)
		require.NotEqual(t, loaded.record.RunID, f.job.runID)
	}
}

// Retain the original gate at the raw reply boundary after a genuinely paid
// actor CAS/read, rather than fabricating candidate E or confirmed fields. This
// isolates reconciliation of an already published candidate; it does not claim
// to reproduce a particular outer owner.postcheck cancellation schedule.
func TestCleanupDetachedNoEffectNativeRecoveryUnacceptedConfirmedCandidate(t *testing.T) {
	f, oldDraft := cleanupDetachedUnknownRecoveryFixture(t)
	f.job.effectsJoined = true
	require.NoError(t, f.b.joinCleanupRun(f.ctx, f.job))
	gate := prepareCleanupDetachedRecoveryForTest(t, f)
	key := f.job.state.execution.key
	original := f.b.client.KV
	writes := 0
	f.b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		if putsKey(key)(yes) {
			writes++
		}
		return original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
	}}
	t.Cleanup(func() { f.b.client.KV = original })
	request := &cleanupLocalContinuation{kind: "resume-detached-noeffect", reply: make(chan cleanupLocalReply, 1)}
	f.b.archiveLife.mu.Lock()
	available := f.job.request == nil
	if available {
		// This is controlled fixture mailbox scheduling, never copied Run
		// permission. The existing actor authenticates and executes the gate.
		f.job.request, f.job.step = request, "detached-recovery-run"
	}
	f.b.archiveLife.mu.Unlock()
	require.True(t, available)
	f.worker.wakeCleanup()
	select {
	case reply := <-request.reply:
		require.NoError(t, reply.err)
	case <-time.After(12 * time.Second):
		t.Fatal("original recovery actor did not publish its finite reply")
	}
	f.joinActorTail(t)
	require.True(t, gate.confirmed)
	require.Same(t, gate, f.job.detachedRecovery)
	require.Same(t, oldDraft, f.job.detachedNoEffect)
	candidate := f.job.state.execution
	require.Equal(t, gate.value, candidate.value)
	_, err := f.worker.cleanupContinuation(f.job, &cleanupLocalContinuation{kind: "resume-detached-noeffect"})
	require.NoError(t, err, "the original exact confirmed candidate must remain query-only reconcilable")
	require.Equal(t, 1, writes)
	require.Equal(t, candidate, f.job.state.execution)
	require.Nil(t, f.job.detachedRecovery)
	require.Nil(t, f.job.detachedNoEffect)
}
