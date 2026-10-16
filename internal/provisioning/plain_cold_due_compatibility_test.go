package provisioning

import (
	"context"
	"testing"

	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/stretchr/testify/require"
)

func TestPlainColdDuePublicationRecoveryVersions(t *testing.T) {
	w, err := state.NewWorkspaceIdentity("s3", "storage", "bucket", "workspace/")
	require.NoError(t, err)
	s := &PlainColdService{config: launcherconfig.Config{Binding: launcherconfig.Binding{Namespace: "/sandbox/test/cell/", RestoreEpoch: "restore"}}}
	for _, stage := range []string{"runtime_publish_enrolled_v2", "runtime_publish_enrolled_v3"} {
		result := PlainColdResult{RequestID: "request", Stage: state.StageReference{Namespace: s.config.Binding.Namespace, RestoreEpoch: "restore", Partition: w.Partition(), RequestID: "request", StageID: stage}}
		outcome, err := s.arbitrate(context.Background(), w, result)
		require.Equal(t, state.OutcomeUnknown, outcome)
		require.ErrorIs(t, err, state.ErrInvalidConfiguration, "real recovery must route both explicit domains to enrolled arbitration")
		require.NotErrorIs(t, err, ErrPlainColdProfile)
	}
	result := PlainColdResult{RequestID: "request", Stage: state.StageReference{Namespace: s.config.Binding.Namespace, RestoreEpoch: "restore", Partition: w.Partition(), RequestID: "request", StageID: "runtime_publish_enrolled_v4"}}
	_, err = s.arbitrate(context.Background(), w, result)
	require.ErrorIs(t, err, ErrPlainColdProfile)
}
