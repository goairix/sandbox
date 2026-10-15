package etcd

import (
	"context"
	"testing"
	"time"

	k "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/stretchr/testify/require"
)

func TestLocalRuntimeReleaseLocalBinding(t *testing.T) {
	for _, b := range []*Backend{nil, {}} {
		for _, ctx := range []context.Context{nil, context.Background()} {
			slot, err := b.BindTaskWorkspaceArchiveLocal(ctx, nil, TaskWorkspaceSyncPlan{}, nil, "/tmp/untrusted")
			require.Error(t, err)
			require.Zero(t, slot)
		}
	}
}

func TestLocalRuntimeReleasePurposeOwners(t *testing.T) {
	b := &Backend{archiveLife: &taskArchiveBackend{}, localRuntimeRelease: &LocalRuntimeReleaseOptions{MaxActive: 1, Timeout: time.Second, Provider: &k.TaskRuntimeCleanupClient{}}}
	seed := recoveryTestSeed(t)
	c := &TaskClaim{origin: b, parentCtx: context.Background(), reference: TaskClaimReference{Task: seed.Task, ClaimID: seed.ClaimID, CreateRevision: 2, LeaseID: 3}, deadline: time.Now().Add(time.Minute)}
	c.self = c
	bind, err := b.beginLocalRuntimeReleasePurpose(context.Background(), c, "bind")
	require.NoError(t, err)
	require.Nil(t, c.runtimeReleaseAttempt)
	require.Nil(t, c.ownerReleaseAttempt)
	_, err = b.beginLocalRuntimeReleasePurpose(context.Background(), c, "release")
	require.Error(t, err, "all purposes share one active Task owner")
	bind.finish()
	terminate, err := b.beginLocalRuntimeRelease(context.Background(), c)
	require.NoError(t, err)
	terminate.finish()
	release, err := b.beginLocalRuntimeReleasePurpose(context.Background(), c, "release")
	require.NoError(t, err)
	release.finish()
	for _, purpose := range []string{"terminate", "release", "other"} {
		_, err := b.beginLocalRuntimeReleasePurpose(context.Background(), c, purpose)
		require.Error(t, err)
	}
	require.Same(t, terminate, c.runtimeReleaseAttempt)
	require.Same(t, release, c.ownerReleaseAttempt)
	require.NoError(t, b.Close())
}
