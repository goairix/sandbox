package etcd

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// The emitter keeps one widest scalar witness. Check all32 real canonical
// selection/slot encodings independently, so no retained protobuf maxima batch
// or an assumption about one-digit slots can hide a larger native candidate.
func TestCleanupDetachedWireExecutionWitnessBoundsAll32Candidates(t *testing.T) {
	b, input, dimensions := cleanupDetachedWireFixture(t)
	d, err := b.prepareDetachedWireBuilder(input, dimensions)
	require.NoError(t, err)
	witness := d.widestExecution()
	wire, err := encodeCleanupExecution(witness)
	require.NoError(t, err)
	slot, err := encodeCleanupExecutionSlot(cleanupSlotRecordFromExecution(witness))
	require.NoError(t, err)
	key, err := b.namespace.cleanupExecutionSlotKey(witness.Slot)
	require.NoError(t, err)
	seen := make(map[uint32]bool)
	for n := uint64(0); n < 32; n++ {
		r := d.execution
		r.Selection = math.MaxUint64 - n
		r.Slot = cleanupCandidate(r.TaskDigest, r.Selection)
		seen[r.Slot] = true
		candidate, err := encodeCleanupExecution(r)
		require.NoError(t, err)
		candidateSlot, err := encodeCleanupExecutionSlot(cleanupSlotRecordFromExecution(r))
		require.NoError(t, err)
		candidateKey, err := b.namespace.cleanupExecutionSlotKey(r.Slot)
		require.NoError(t, err)
		require.LessOrEqual(t, len(candidate), len(wire))
		require.LessOrEqual(t, len(candidateSlot), len(slot))
		require.Equal(t, len(key), len(candidateKey))
	}
	require.Len(t, seen, 32)
}
