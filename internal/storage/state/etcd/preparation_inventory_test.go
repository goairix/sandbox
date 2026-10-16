package etcd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestColdPreparationInventoryConfiguration(t *testing.T) {
	// An empty or unbounded inventory must be rejected before an operator RPC.
	require.ErrorIs(t, validatePreparationInventory(&PreparationInventoryOptions{}), ErrInvalidConfiguration)
	profile := &PreparationInventoryOptions{}
	profile.QuotaSlots[7] = 40
	require.NoError(t, validatePreparationInventory(profile))
	profile.QuotaSlots[7] = 1000001
	require.ErrorIs(t, validatePreparationInventory(profile), ErrInvalidConfiguration)
	require.NoError(t, validatePreparationInventory(nil), "unconfigured legacy backends remain available")
}

func TestColdPreparationInventoryScopeBinding(t *testing.T) {
	first, err := NewNamespace("/cold", "authority", "cell-a")
	require.NoError(t, err)
	second, err := NewNamespace("/cold", "authority", "cell-b")
	require.NoError(t, err)
	profile := &PreparationInventoryOptions{}
	profile.QuotaSlots[7] = 40
	identity := Identity{SchemaVersion: 1, Prefix: "/cold", AuthorityID: "authority", Cell: "cell-a", ClusterID: 1, StorageID: "storage", RuntimeID: "runtime", RestoreEpoch: "restore"}
	original := preparationProfileDigest(first, identity, profile)
	require.NotEqual(t, original, preparationProfileDigest(second, identity, profile), "same counts and restore cannot authorize another namespace")
	for _, change := range []func(*Identity){
		func(i *Identity) { i.ClusterID = 2 },
		func(i *Identity) { i.StorageID = "other-storage" },
		func(i *Identity) { i.RuntimeID = "other-runtime" },
		func(i *Identity) { i.RestoreEpoch = "other-restore" },
	} {
		other := identity
		change(&other)
		require.NotEqual(t, original, preparationProfileDigest(first, other, profile))
	}
}
