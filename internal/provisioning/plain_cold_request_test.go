package provisioning

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestPlainColdRequestCapsule(t *testing.T) {
	w, err := state.NewWorkspaceIdentity("s3", "storage", "bucket", "workspace/")
	require.NoError(t, err)
	r := PlainColdRequest{Principal: "principal", IdempotencyKey: "key", RequestID: uuid.NewString(), IntentID: uuid.NewString(), SandboxID: uuid.NewString(), SnapshotVersion: uuid.NewString(), TTL: time.Minute}
	profile := strings.Repeat("a", 64)
	wire, err := coldRequestCapsule(profile, w, r)
	require.NoError(t, err)
	var capsule plainColdCapsule
	require.NoError(t, json.Unmarshal(wire, &capsule))
	require.Equal(t, w.Hash(), capsule.WorkspaceHash)
	require.Equal(t, w.Partition(), capsule.Partition)
	require.Equal(t, r.TTL, capsule.TTL)
	again, err := coldRequestCapsule(profile, w, r)
	require.NoError(t, err)
	require.Equal(t, wire, again, "retry capsule must not capture a new clock observation")
	changed := r
	changed.TTL = 2 * time.Minute
	other, err := coldRequestCapsule(profile, w, changed)
	require.NoError(t, err)
	require.NotEqual(t, wire, other)
	for _, ttl := range []time.Duration{0, -1, 366 * 24 * time.Hour} {
		changed.TTL = ttl
		_, err := coldRequestCapsule(profile, w, changed)
		require.ErrorIs(t, err, ErrPlainColdProfile, "unresolved/never-expire/unsupported TTL must be rejected")
	}
	_, err = coldRequestCapsule(profile, state.WorkspaceIdentity{}, r)
	require.ErrorIs(t, err, ErrPlainColdProfile)
}
