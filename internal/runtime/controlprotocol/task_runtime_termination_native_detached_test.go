package controlprotocol

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// A detached native consumer must have an explicit fixed key entry. The legacy
// local entry cannot accidentally select detached effects from missing data.
func TestDetachedRuntimeTerminationFixedNativeKeys(t *testing.T) {
	r := terminationRoundTrip(t, terminationMarshal(t, terminationDetachedFixture(t)))
	keys, err := DetachedTaskRuntimeTerminationKeysFor(r)
	require.NoError(t, err)
	partition := fmt.Sprintf("%sp/%02x/", r.Current.Namespace, r.Partition)
	task := partition + "tasks/" + r.Current.TaskID
	require.Equal(t, task+"/detached-noeffect", keys.Settlement)
	require.Equal(t, partition+"intents/"+r.CreationIntentID, keys.CreationIntent)
	require.Equal(t, keys.CreationIntent+"/runtime-publication", keys.Publication)
	require.Empty(t, keys.Archive)
	require.Empty(t, keys.LocalComplete)
	require.Equal(t, task+"/runtime-termination", keys.Intent)
	require.Equal(t, task+"/claim", keys.Claim)
	require.Equal(t, task+"/guards/"+r.Current.ClaimID, keys.Guard)

	legacy := terminationRoundTrip(t, terminationMarshal(t, terminationLegacyFixture(t)))
	_, err = DetachedTaskRuntimeTerminationKeysFor(legacy)
	require.Error(t, err, "detached entry must reject a local variant")
	local, err := TaskRuntimeTerminationKeysFor(legacy)
	require.NoError(t, err)
	require.Empty(t, local.Settlement)
	require.Empty(t, local.CreationIntent)
	require.Empty(t, local.Publication)

	r.Authority.Archive = legacy.Authority.Archive
	_, err = DetachedTaskRuntimeTerminationKeysFor(r)
	require.Error(t, err, "mixed effect profiles cannot acquire native keys")
}
