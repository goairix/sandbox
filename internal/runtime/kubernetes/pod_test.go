package kubernetes

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/goairix/sandbox/internal/runtime"
)

func TestWorkspaceFUSEContractIsUnsupported(t *testing.T) {
	rt := &Runtime{}
	ctx := context.Background()

	_, err := rt.PrepareSandbox(ctx, runtime.SandboxSpec{})
	require.ErrorIs(t, err, runtime.ErrWorkspaceFUSEUnsupported)
	require.ErrorIs(t, rt.AuthorizeWorkspaceMount(ctx, "id", runtime.WorkspaceMountAuthorization{}), runtime.ErrWorkspaceFUSEUnsupported)
	_, err = rt.WaitSandboxReady(ctx, "id")
	require.ErrorIs(t, err, runtime.ErrWorkspaceFUSEUnsupported)
	require.ErrorIs(t, rt.PreparedSandboxHealth(ctx, "id", "pool"), runtime.ErrWorkspaceFUSEUnsupported)
	_, err = rt.WorkspaceHealth(ctx, "id")
	require.ErrorIs(t, err, runtime.ErrWorkspaceFUSEUnsupported)
	_, err = rt.QuiesceWorkspace(ctx, "id")
	require.ErrorIs(t, err, runtime.ErrWorkspaceFUSEUnsupported)
	require.ErrorIs(t, rt.ResumeWorkspace(ctx, "id", runtime.WorkspaceQuiesceToken{}), runtime.ErrWorkspaceFUSEUnsupported)
	require.ErrorIs(t, rt.FlushWorkspace(ctx, "id"), runtime.ErrWorkspaceFUSEUnsupported)
}

func TestCreatePodUsesConfiguredTmpDiskLimit(t *testing.T) {
	client := fake.NewSimpleClientset()

	pod, err := createPod(context.Background(), client, "default", runtime.SandboxSpec{
		ID:      "sandbox-test",
		Image:   "sandbox:latest",
		TmpDisk: "200Mi",
	})

	require.NoError(t, err)
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "tmp" {
			require.NotNil(t, volume.EmptyDir)
			require.NotNil(t, volume.EmptyDir.SizeLimit)
			assert.Equal(t, int64(200*1024*1024), volume.EmptyDir.SizeLimit.Value())
			return
		}
	}
	t.Fatal("tmp volume not found")
}

func TestCreatePodDefaultsTmpDiskLimitTo50Mi(t *testing.T) {
	client := fake.NewSimpleClientset()

	pod, err := createPod(context.Background(), client, "default", runtime.SandboxSpec{
		ID:    "sandbox-test",
		Image: "sandbox:latest",
	})

	require.NoError(t, err)
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "tmp" {
			require.NotNil(t, volume.EmptyDir)
			require.NotNil(t, volume.EmptyDir.SizeLimit)
			assert.Equal(t, int64(50*1024*1024), volume.EmptyDir.SizeLimit.Value())
			return
		}
	}
	t.Fatal("tmp volume not found")
}
