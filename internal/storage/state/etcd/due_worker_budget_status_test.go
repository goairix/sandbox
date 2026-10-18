package etcd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Status must observe the original cache under the same lifetime lock, even
// between actor publications. This metadata-only fixture initializes automatic
// discovery normally; it supplies no physical admission or runtime authority.
func TestDueWorkerBudgetStatusTracksCacheWithoutPublication(t *testing.T) {
	f, w := newDueAutomaticMetadataFixture(t)
	w.publishStatus("listing", nil)
	before := w.Status()
	require.Equal(t, w.cache.snapshot().OwnedBytes, before.OwnedBytes)
	require.Zero(t, before.Queued)

	pages := &duePageFixture{b: f.b, server: f.server}
	candidates := pages.seed(t, 1, time.Now().UTC().Add(time.Minute))
	candidate := candidates[0]
	key, err := f.b.namespace.dueCandidateKey(candidate)
	require.NoError(t, err)
	wire, err := encodeDueCandidate(candidate)
	require.NoError(t, err)

	f.b.archiveLife.mu.Lock()
	err = w.cache.put(key, wire, candidate)
	f.b.archiveLife.mu.Unlock()
	require.NoError(t, err)
	populated := w.cache.snapshot()
	require.Equal(t, 1, populated.Keys)
	require.Greater(t, populated.OwnedBytes, before.OwnedBytes)

	// No publishStatus call follows the cache mutation. Waiting for another
	// actor turn would conceal stale bytes and queue length from the caller.
	current := w.Status()
	require.Equal(t, populated.OwnedBytes, current.OwnedBytes)
	require.Equal(t, populated.Keys, current.Queued)
	require.Equal(t, before.Phase, current.Phase)

	// An exact-key replacement owns one queue row, then real settlement removes
	// that row. Both changes must remain visible without a new publication.
	f.b.archiveLife.mu.Lock()
	err = w.cache.put(key, wire, candidate)
	f.b.archiveLife.mu.Unlock()
	require.NoError(t, err)
	replaced := w.Status()
	require.Equal(t, populated.OwnedBytes, replaced.OwnedBytes)
	require.Equal(t, 1, replaced.Queued)

	w.cache.settled(candidate)
	empty := w.cache.snapshot()
	require.Zero(t, empty.Keys)
	require.Equal(t, before.OwnedBytes, empty.OwnedBytes)
	current = w.Status()
	require.Equal(t, empty.OwnedBytes, current.OwnedBytes)
	require.Zero(t, current.Queued)
}
