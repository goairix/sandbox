//go:build linux

package etcd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Both independent long-input tests use these same initial producer inputs.
// No field is changed after provisioning or the first root signature.
func newCleanupDetachedNoEffectSignedLongFixture(t *testing.T) (*cleanupDetachedNoEffectNativeFixture, cleanupDetachedNoEffectSourceShape) {
	t.Helper()
	// Root is exactly 512 bytes: /prefix(252)/scope(128)/cell(128)/.
	// Keep the unique cell under this test's existing exact namespace cleanup.
	namespace, err := NewNamespace("/"+strings.Repeat("n", 252), strings.Repeat("s", 128), uuid.NewString()+strings.Repeat("c", 92))
	require.NoError(t, err)
	require.Len(t, namespace.Root(), 512)
	shape := cleanupDetachedNoEffectSourceShape{
		backend: integrationBackendShape{namespace: namespace, runtimeID: strings.Repeat("a", 128), restoreEpoch: strings.Repeat("e", 128)},
		target:  strings.Repeat("t", 128),
		runtime: RuntimeReference{ID: strings.Repeat("<", 112) + strings.Repeat("i", 16), UID: strings.Repeat("u", 128), BootID: uuid.NewString()},
	}
	// Runtime identifiers are opaque noncontrol UTF-8 of at most 128 bytes.
	// The ID's ordinary signer JSON encoding expands '<' to \u003c; UID remains
	// ASCII so the combined activation companion still fits its original 8KiB
	// cap. Authority, target and worker remain valid native Task domain segments.
	// Enrolled target activation additionally requires BootID to be a UUID.
	f := newCleanupDetachedNoEffectNativeFixtureWithSource(t, &shape, strings.Repeat("w", 128))
	return f, shape
}

// A legal long input exercises the actual root-signed source, issuer tickets,
// runtime receipts and native writer. This is not an absolute maximum over all
// protocol fields, managed consumer admission or physical PID1 verification.
func TestCleanupDetachedNoEffectNativeSignedQuiescenceAbove16KiB(t *testing.T) {
	f, shape := newCleanupDetachedNoEffectSignedLongFixture(t)
	namespace := shape.backend.namespace
	qKey, err := f.b.namespace.taskQuiescenceKey(f.ref)
	require.NoError(t, err)
	point, err := f.raw.Get(f.ctx, qKey)
	require.NoError(t, err)
	require.Len(t, point.Kvs, 1)
	kv := point.Kvs[0]
	require.Greater(t, len(kv.Value), 16<<10, "actual signed producer output must cross the former reader cap")
	require.LessOrEqual(t, len(kv.Value), 32<<10)
	require.Zero(t, kv.Lease)
	require.EqualValues(t, 1, kv.Version)
	require.Equal(t, kv.CreateRevision, kv.ModRevision)
	var actual TaskQuiescenceRecord
	require.NoError(t, decodeTaskQuiescenceRecord(kv, &actual))
	require.Equal(t, shape.runtime, actual.Task.Runtime)
	require.Equal(t, namespace.Root(), actual.Context.Current.Namespace)
	require.Equal(t, shape.backend.runtimeID, actual.Context.Current.AuthorityID)
	require.Equal(t, shape.target, actual.Context.Current.Target)
	require.LessOrEqual(t, len(actual.Ticket), 8192)
	require.LessOrEqual(t, len(actual.CloseDataReceipt), 8192)

	draft, err := f.b.prepareCleanupDetachedNoEffect(f.ctx, f.job, f.drained, f.destination)
	require.NoError(t, err, "actual signed history reader and dual-drain revalidation must accept this legal long source")
	require.Equal(t, kv.CreateRevision, draft.record.Quiescence.CreateRevision)
	require.Equal(t, workspaceSyncIntentDigest(string(kv.Value)), draft.record.Quiescence.SHA256)
	require.Equal(t, actual.Context.Current, draft.record.QuiescenceContext)
	require.Equal(t, f.drained.emptyRevision, draft.record.EmptyRevision)
	require.Equal(t, f.job.runID, draft.record.RunID)
	require.Equal(t, detachedNoEffectClaim(f.job.claim.reference), draft.record.WriterClaim)
	require.Equal(t, draft.record.AttemptID, f.job.state.record.Pending.AttemptID)
	require.Equal(t, draft.record.BindingDigest, f.job.state.record.Pending.Digest)
	writes := captureCleanupDetachedNoEffectWrite(t, f, draft, nil)
	result, err := f.b.commitCleanupDetachedNoEffect(f.ctx, f.job, draft)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.Equal(t, 1, *writes)
	loaded, err := f.b.loadTaskDetachedNoEffect(f.ctx, f.ref)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	require.Equal(t, result.Entry.record, loaded.record)
	require.Equal(t, result.Entry.kv, loaded.kv)
	t.Logf("actual signed legal-long input: namespace=%d quiescence=%d ticket=%d close-receipt=%d settlement=%d bytes", len(namespace.Root()), len(kv.Value), len(actual.Ticket), len(actual.CloseDataReceipt), len(loaded.kv.Value))
}

// A present fact still retains the original writer's entire owned Claim graph
// until the original cold actor replies. This measures the accepted long input
// under the existing 128KiB gate, with no DTO replacement or context exemption.
// Failure remains a real retained-cap rejection, not a successful boundary test.
func TestCleanupDetachedNoEffectNativeSignedLongRetainedRecoveryGraph(t *testing.T) {
	f, _ := newCleanupDetachedNoEffectSignedLongFixture(t)
	qKey, err := f.b.namespace.taskQuiescenceKey(f.ref)
	require.NoError(t, err)
	qPoint, err := f.raw.Get(f.ctx, qKey)
	require.NoError(t, err)
	require.Len(t, qPoint.Kvs, 1)
	require.Greater(t, len(qPoint.Kvs[0].Value), 16<<10)
	draft, err := f.b.prepareCleanupDetachedNoEffect(f.ctx, f.job, f.drained, f.destination)
	require.NoError(t, err)
	writes := captureCleanupDetachedNoEffectWrite(t, f, draft, nil)
	committed, err := f.b.commitCleanupDetachedNoEffect(f.ctx, f.job, draft)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, committed.Outcome)
	require.NotNil(t, committed.Entry)
	require.Equal(t, 1, *writes)
	fact := committed.Entry
	key := string(fact.kv.Key)
	noneKey, err := f.b.namespace.taskFinalSyncAttemptKey(f.ref)
	require.NoError(t, err)
	noneBefore, err := f.raw.Get(f.ctx, noneKey)
	require.NoError(t, err)
	require.Len(t, noneBefore.Kvs, 1)
	t.Logf("actual retained-input sources: q-bytes=%d q-sha256=%s fact-bytes=%d fact-sha256=%s none-bytes=%d none-sha256=%s", len(qPoint.Kvs[0].Value), workspaceSyncIntentDigest(string(qPoint.Kvs[0].Value)), len(fact.kv.Value), workspaceSyncIntentDigest(string(fact.kv.Value)), len(noneBefore.Kvs[0].Value), workspaceSyncIntentDigest(string(noneBefore.Kvs[0].Value)))

	claim, prepared, state := draft.claim, draft.prepared, draft.state
	quiescenceBefore := *claim.quiescenceDraft
	priorBefore := *quiescenceBefore.prior
	wire, execution := draft.value, bytes.Clone(draft.record.ExecutionValue)
	attempt, binding := draft.record.AttemptID, draft.record.BindingDigest
	none := claim.finalSyncDraft
	require.NotNil(t, none)
	noneValue := none.value
	require.NoError(t, f.b.ReleaseTaskClaim(f.ctx, claim))
	f.job.claim = nil
	// This component has no background physical loop: its synchronous effects
	// and mTLS sessions already returned. Preserve the actual native join path.
	f.job.effectsJoined = true
	require.NoError(t, f.b.joinCleanupRun(f.ctx, f.job))
	require.True(t, f.job.state.record.Run.Joined)
	require.Nil(t, f.job.owner)
	joined := f.job.state.execution

	// The existing helper logs actual original root byte counts on rejection
	// and retains require.NoError. It never fabricates a smaller candidate.
	gate := prepareCleanupDetachedRecoveryForTest(t, f)
	requireCleanupDetachedQuiescencePayloadRetired(t, claim.quiescenceDraft, quiescenceBefore, priorBefore)
	owned, err := f.b.cleanupDetachedNoEffectOwnedBytes(f.job)
	require.NoError(t, err)
	require.LessOrEqual(t, owned, uint64(128<<10))
	t.Logf("actual signed-long original draft+recovery graph=%d bytes", owned)
	require.Same(t, draft, f.job.detachedNoEffect)
	require.Same(t, claim, draft.claim)
	require.Same(t, prepared, draft.prepared)
	require.Same(t, state, draft.state)
	require.Equal(t, wire, draft.value)
	require.Equal(t, execution, draft.record.ExecutionValue)
	require.Equal(t, attempt, draft.record.AttemptID)
	require.Equal(t, binding, draft.record.BindingDigest)
	require.True(t, draft.writeAttempted)
	require.Same(t, none, claim.finalSyncDraft)
	require.Equal(t, noneValue, none.value)

	original := f.b.client.KV
	runWrites := 0
	f.b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		if putsKey(joined.key)(yes) {
			runWrites++
			require.Equal(t, 51, len(cmps)+len(yes)+len(no))
			require.Contains(t, cmps, clientv3.Compare(clientv3.ModRevision(key), "=", fact.kv.ModRevision))
			require.Same(t, gate, f.job.detachedRecovery)
			require.Same(t, draft, f.job.detachedNoEffect, "both original roots remain until actual actor reply")
		}
		return original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
	}}
	t.Cleanup(func() { f.b.client.KV = original })
	_, err = f.worker.cleanupContinuation(f.job, &cleanupLocalContinuation{kind: "resume-detached-noeffect"})
	require.NoError(t, err)
	require.Equal(t, 1, runWrites)
	require.Equal(t, 1, *writes, "recovery cannot resubmit the old settlement")
	require.Nil(t, f.job.detachedRecovery)
	require.Nil(t, f.job.detachedNoEffect)
	after, err := f.b.cleanupDetachedNoEffectOwnedBytes(f.job)
	require.NoError(t, err)
	require.Zero(t, after)
	loaded, err := f.b.loadTaskDetachedNoEffect(f.ctx, f.ref)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	require.Equal(t, fact.kv, loaded.kv)
	noneAfter, err := f.raw.Get(f.ctx, noneKey)
	require.NoError(t, err)
	require.Equal(t, noneBefore.Kvs, noneAfter.Kvs)
}
