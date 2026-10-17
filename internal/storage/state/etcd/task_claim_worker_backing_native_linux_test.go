//go:build linux

package etcd

import (
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
)

// WorkerID is a visible bounded string but may borrow a much larger caller
// allocation. The genuine Claim constructor must detach that backing before
// the real draft/prepared graph retains it. Pointer comparison is deterministic;
// no GC timing, allocation sampling or fabricated authority is involved.
func TestCleanupDetachedNoEffectNativeClaimWorkerBackingIsOwned(t *testing.T) {
	base := strings.Repeat("w", (128<<10)+4096)
	workerID := base[:16]
	f := newCleanupDetachedNoEffectNativeFixture(t, workerID)
	draft, err := f.b.prepareCleanupDetachedNoEffect(f.ctx, f.job, f.drained, f.destination)
	require.NoError(t, err)
	for _, retained := range []string{draft.claim.reference.WorkerID, draft.prepared.claimReference.WorkerID, draft.record.WriterClaim.WorkerID} {
		require.Equal(t, workerID, retained)
		require.True(t, unsafe.StringData(base) != unsafe.StringData(retained), "the real Claim/draft must not retain caller's oversized backing")
	}
	runtime.KeepAlive(base)
}
