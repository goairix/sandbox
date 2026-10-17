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

// This is one deterministic legal field combination, not an absolute maximum.
// Compared with the accepted long fixture, SandboxID and SnapshotVersion grow
// from UUID36 to domain128. Only SandboxID enters the activation companion,
// exactly three times: outer record, activation identity and runtime identity
// certificate. Replacing eight '<' bytes with 'i' saves 3*8*5 JSON bytes there.
// Its canonical upper projection is 7912 + 3*92 - 3*8*5 = 8068 <= 8192.
// That bound includes six UTC timestamps of at most 30 bytes, three base64
// Ed25519 signatures of 88 bytes, two public keys of 44 bytes, UUID36, hash64,
// fixed generation/gate epoch1, UID/GID1000 and network=false. No variable native
// revision occurs in that companion. All real Sign/Bind/Publish calls must still
// succeed; static projection is never counted as native producer acceptance.
// Task/I project below 3.2KiB, none below 5.8KiB, Q ticket below 6.2KiB, close
// ticket below 3.1KiB and Q record below 18KiB, under their original limits.
func TestCleanupDetachedNoEffectNativeMaxFieldsRetainedRecoveryGraph(t *testing.T) {
	namespace, err := NewNamespace("/"+strings.Repeat("n", 252), strings.Repeat("s", 128), uuid.NewString()+strings.Repeat("c", 92))
	require.NoError(t, err)
	require.Len(t, namespace.Root(), 512)
	shape := cleanupDetachedNoEffectSourceShape{
		backend:         integrationBackendShape{namespace: namespace, runtimeID: strings.Repeat("a", 128), restoreEpoch: strings.Repeat("e", 128)},
		target:          strings.Repeat("t", 128),
		runtime:         RuntimeReference{ID: strings.Repeat("<", 104) + strings.Repeat("i", 24), UID: strings.Repeat("u", 128), BootID: uuid.NewString()},
		sandboxID:       uuid.NewString() + strings.Repeat("s", 92),
		snapshotVersion: uuid.NewString() + strings.Repeat("v", 92),
	}
	f := newCleanupDetachedNoEffectNativeFixtureWithSource(t, &shape, strings.Repeat("w", 128))
	qKey, err := f.b.namespace.taskQuiescenceKey(f.ref)
	require.NoError(t, err)
	qPoint, err := f.raw.Get(f.ctx, qKey)
	require.NoError(t, err)
	require.Len(t, qPoint.Kvs, 1)
	require.Greater(t, len(qPoint.Kvs[0].Value), 16<<10)
	var q TaskQuiescenceRecord
	require.NoError(t, decodeTaskQuiescenceRecord(qPoint.Kvs[0], &q))
	require.Equal(t, shape.sandboxID, q.Task.Reference.SandboxID)
	require.Equal(t, shape.snapshotVersion, q.Task.Snapshot.Version)
	draft, err := f.b.prepareCleanupDetachedNoEffect(f.ctx, f.job, f.drained, f.destination)
	require.NoError(t, err)
	writes := captureCleanupDetachedNoEffectWrite(t, f, draft, nil)
	factResult, err := f.b.commitCleanupDetachedNoEffect(f.ctx, f.job, draft)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, factResult.Outcome)
	require.NotNil(t, factResult.Entry)
	require.Equal(t, 1, *writes)
	fact := factResult.Entry
	noneKey, err := f.b.namespace.taskFinalSyncAttemptKey(f.ref)
	require.NoError(t, err)
	noneBefore, err := f.raw.Get(f.ctx, noneKey)
	require.NoError(t, err)
	require.Len(t, noneBefore.Kvs, 1)
	t.Logf("actual max-field component bytes: task=%d cleanup-intent=%d q=%d q-ticket=%d close-receipt=%d none=%d E=%d fact=%d q-sha256=%s fact-sha256=%s", len(draft.claim.fences[0].value), len(draft.claim.fences[1].value), len(qPoint.Kvs[0].Value), len(q.Ticket), len(q.CloseDataReceipt), len(noneBefore.Kvs[0].Value), len(draft.record.ExecutionValue), len(fact.kv.Value), workspaceSyncIntentDigest(string(qPoint.Kvs[0].Value)), workspaceSyncIntentDigest(string(fact.kv.Value)))

	claim, prepared, state := draft.claim, draft.prepared, draft.state
	wire, execution := draft.value, bytes.Clone(draft.record.ExecutionValue)
	attempt, binding := draft.record.AttemptID, draft.record.BindingDigest
	none := claim.finalSyncDraft
	require.NotNil(t, none)
	noneValue := none.value
	require.NoError(t, f.b.ReleaseTaskClaim(f.ctx, claim))
	f.job.claim = nil
	// Every synchronous fixture effect and transport has actually returned;
	// this remains a native metadata component, without a physical consumer.
	f.job.effectsJoined = true
	require.NoError(t, f.b.joinCleanupRun(f.ctx, f.job))
	require.True(t, f.job.state.record.Run.Joined)
	require.Nil(t, f.job.owner)
	joined := f.job.state.execution
	gate := prepareCleanupDetachedRecoveryForTest(t, f)
	owned, err := f.b.cleanupDetachedNoEffectOwnedBytes(f.job)
	require.NoError(t, err)
	require.LessOrEqual(t, owned, uint64(128<<10))
	t.Logf("actual max-field original draft+recovery graph=%d bytes", owned)
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
			require.Contains(t, cmps, clientv3.Compare(clientv3.ModRevision(string(fact.kv.Key)), "=", fact.kv.ModRevision))
			require.Same(t, gate, f.job.detachedRecovery)
			require.Same(t, draft, f.job.detachedNoEffect)
		}
		return original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
	}}
	t.Cleanup(func() { f.b.client.KV = original })
	_, err = f.worker.cleanupContinuation(f.job, &cleanupLocalContinuation{kind: "resume-detached-noeffect"})
	require.NoError(t, err)
	require.Equal(t, 1, runWrites)
	require.Equal(t, 1, *writes)
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
