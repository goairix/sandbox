//go:build linux

package etcd

import (
	"context"
	"math"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// Count the actual reachable original Claim/prepared/E/wire graph, not a copied
// wire-length estimate. This acceptance is per-job retained ownership only;
// it does not enable consumer admission or reserve the global 64MiB budget.
func TestCleanupDetachedNoEffectNativeRetainedGraphAndFinish(t *testing.T) {
	f, draft := cleanupDetachedUnknownRecoveryFixture(t)
	f.job.effectsJoined = true
	require.NoError(t, f.b.joinCleanupRun(f.ctx, f.job))
	prepareCleanupDetachedRecoveryForTest(t, f)
	bytes, err := f.b.cleanupDetachedNoEffectOwnedBytes(f.job)
	require.NoError(t, err)
	require.Greater(t, bytes, uint64(len(draft.value)+len(draft.record.ExecutionValue)))
	require.LessOrEqual(t, bytes, uint64(128<<10), "candidate bound must cover the real old draft and new recovery graph together")
	t.Logf("actual original detached retained graph=%d bytes", bytes)

	// Equal visible bytes can retain a larger backing allocation. The real
	// draft is never submitted again; this changes capacity for memory testing
	// only, and restores the exact original evidence before any lifecycle work.
	prior := draft.record.ExecutionValue
	grown := make([]byte, len(prior), cap(prior)+1024)
	copy(grown, prior)
	draft.record.ExecutionValue = grown
	larger, err := f.b.cleanupDetachedNoEffectOwnedBytes(f.job)
	require.NoError(t, err)
	require.GreaterOrEqual(t, larger-bytes, uint64(1024))
	draft.record.ExecutionValue = make([]byte, len(prior), (128<<10)+1)
	_, err = f.b.cleanupDetachedNoEffectOwnedBytes(f.job)
	require.ErrorIs(t, err, ErrInvalidRecord)
	draft.record.ExecutionValue = prior
	bytesAgain, err := f.b.cleanupDetachedNoEffectOwnedBytes(f.job)
	require.NoError(t, err)
	require.Equal(t, bytes, bytesAgain)

	j := f.job
	f.b.finishCleanupJob(j, nil)
	f.job = nil // the actual finish already removed and closed this registry job
	require.Nil(t, j.detachedNoEffect)
	require.Nil(t, j.detachedRecovery)
	after, err := f.b.cleanupDetachedNoEffectOwnedBytes(j)
	require.NoError(t, err)
	require.Zero(t, after)
}

// The pure bounded graph walker is shared by the job counter. It has no KV or
// effect capability; malformed mutable roots and arithmetic cannot evade caps.
func TestCleanupDetachedNoEffectRetainedGraphWalkerBoundaries(t *testing.T) {
	payload := &struct{ Wire []byte }{Wire: make([]byte, 1, 4096)}
	one, err := cleanupDetachedNoEffectGraphBytes(payload)
	require.NoError(t, err)
	require.GreaterOrEqual(t, one, uint64(4096))
	aliased, err := cleanupDetachedNoEffectGraphBytes(payload, payload)
	require.NoError(t, err)
	require.Equal(t, one, aliased, "pointer aliases cannot recursively double-count or loop")
	_, err = cleanupDetachedNoEffectGraphBytes(map[string][]byte{"mutable": {1}})
	require.ErrorIs(t, err, ErrInvalidRecord)
	_, err = cleanupDetachedNoEffectGraphBytes(func() {})
	require.ErrorIs(t, err, ErrInvalidRecord)
	_, err = cleanupDetachedNoEffectAddBytes(math.MaxUint64, 1)
	require.ErrorIs(t, err, ErrInvalidRecord, "overflow must not wrap into an accepted small total")

	// Only explicitly shared infrastructure roots are omitted. Prepared and
	// Claim are deliberately absent from this list and remain owned traversal.
	b := &Backend{}
	w := &DueWorker{}
	j := &cleanupLocalJob{}
	shared, err := cleanupDetachedNoEffectGraphBytes(b, w, j, context.Background(), workspaceSyncLocalNativeWorker)
	require.NoError(t, err)
	require.Zero(t, shared)
	// An object sharing a type with a registered shared root is not thereby
	// shared. Count the nested foreign Backend or explicitly refuse its graph.
	foreign, err := cleanupDetachedNoEffectGraphBytes(b, &struct {
		Original *Backend
		Foreign  *Backend
	}{Original: b, Foreign: &Backend{}})
	if err == nil {
		require.GreaterOrEqual(t, foreign, uint64(reflect.TypeOf(Backend{}).Size()))
	} else {
		require.ErrorIs(t, err, ErrInvalidRecord)
	}
	prepared, err := cleanupDetachedNoEffectGraphBytes(&PreparedTaskQuiescence{})
	require.NoError(t, err)
	require.Positive(t, prepared)
	claim, err := cleanupDetachedNoEffectGraphBytes(&TaskClaim{value: "owned original claim wire"})
	require.NoError(t, err)
	require.Greater(t, claim, uint64(len("owned original claim wire")))
}
