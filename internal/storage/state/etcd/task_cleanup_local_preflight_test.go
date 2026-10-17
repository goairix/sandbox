package etcd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// These are canonical metadata dimensions, not a Kernel or occupied Task.
// Both paths use the same complete protobuf inventory; only actual admission
// may bind that inventory to original E/S/Run/Claim and a genuine Kernel.
func TestCleanupLocalWirePreflight(t *testing.T) {
	t.Run("before-any-execution", func(t *testing.T) {
		b, input := cleanupLocalWireFixture(t)
		worker := input.state.record.Run.Worker
		input.state = nil
		require.NoError(t, b.preflightCleanupLocalWire(input, worker))
		require.Nil(t, input.state)
		require.Nil(t, b.client)
		_, err := b.buildCleanupLocalWirePlan(input)
		require.Error(t, err, "pure profile success cannot manufacture occupied state")
		_, err = b.prepareCleanupLocalWireInventory(input, nil)
		require.ErrorIs(t, err, ErrInvalidConfiguration)
	})
	t.Run("immutable-small-budget", func(t *testing.T) {
		b, input := cleanupLocalWireFixture(t)
		worker := input.state.record.Run.Worker
		input.state = nil
		b.workspaceSyncBudget.options.MaxIntentBytes = 4096
		wire, err := encodeOperationMetadata(taskWorkspaceSyncRecyclingBudgetRecord{Version: 2, TaskWorkspaceSyncBudgetOptions: b.workspaceSyncBudget.options}, 4096)
		require.NoError(t, err)
		b.workspaceSyncBudget.value = wire
		for i := 0; i < 2; i++ {
			err = b.preflightCleanupLocalWire(input, worker)
			require.ErrorIs(t, err, ErrInvalidConfiguration)
			require.ErrorContains(t, err, "workspace sync intent schema")
			require.Nil(t, input.state)
			require.Nil(t, b.client)
		}
	})
	t.Run("shared-complete-projections", func(t *testing.T) {
		b, input := cleanupLocalWireFixture(t)
		dimensions := cleanupLocalWireDimensions{runID: input.state.record.Run.ID, claimID: input.state.record.Pending.ClaimID, worker: input.state.record.Run.Worker}
		actual, err := b.buildCleanupLocalWirePlan(input)
		require.NoError(t, err)
		input.state = nil
		pure, err := b.buildCleanupLocalWireDimensions(input, dimensions)
		require.NoError(t, err)
		require.Equal(t, cleanupLocalWireFingerprint(t, actual), cleanupLocalWireFingerprint(t, pure))
		require.NoError(t, validateCleanupLocalWirePlan(pure))
		for _, family := range pure.families {
			for _, projection := range family.txns {
				require.LessOrEqual(t, projection.request.Size(), cleanupRequestBytes)
				require.LessOrEqual(t, projection.success.Size(), maxMutationBytes)
				require.LessOrEqual(t, projection.failure.Size(), maxMutationBytes)
				require.NoError(t, validateCleanupWire(projection))
			}
			for _, lease := range family.leases {
				require.NoError(t, validateCleanupLeaseWire(lease))
			}
		}
	})
	for _, name := range []string{"missing-descriptor", "wrong-digest", "missing-publication", "invalid-worker-dimensions"} {
		t.Run(name, func(t *testing.T) {
			b, input := cleanupLocalWireFixture(t)
			worker := input.state.record.Run.Worker
			input.state = nil
			switch name {
			case "missing-descriptor":
				input.snapshot.LocalCleanup = nil
			case "wrong-digest":
				input.snapshot.Snapshot.Digest = strings.Repeat("0", 64)
			case "missing-publication":
				input.publication = nil
			case "invalid-worker-dimensions":
				worker.PID = 0
			}
			require.Error(t, b.preflightCleanupLocalWire(input, worker))
			require.Nil(t, b.client)
		})
	}
}
