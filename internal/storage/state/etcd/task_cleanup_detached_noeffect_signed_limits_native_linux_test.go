//go:build linux

package etcd

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// A legal long input exercises the actual root-signed source, issuer tickets,
// runtime receipts and native writer. This is not an absolute maximum over all
// protocol fields, managed consumer admission or physical PID1 verification.
func TestCleanupDetachedNoEffectNativeSignedQuiescenceAbove16KiB(t *testing.T) {
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
