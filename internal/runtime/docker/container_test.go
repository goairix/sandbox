package docker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func TestCreateContainerConfigUsesConfiguredTmpDiskLimit(t *testing.T) {
	_, hostConfig, err := createContainerConfig(runtime.SandboxSpec{
		ID:      "sandbox-test",
		Image:   "sandbox:latest",
		TmpDisk: "200Mi",
	})

	require.NoError(t, err)
	assert.Equal(t, "size=209715200", hostConfig.Tmpfs["/tmp"])
}

func TestCreateContainerConfigDefaultsTmpDiskLimitTo50Mi(t *testing.T) {
	_, hostConfig, err := createContainerConfig(runtime.SandboxSpec{
		ID:    "sandbox-test",
		Image: "sandbox:latest",
	})

	require.NoError(t, err)
	assert.Equal(t, "size=52428800", hostConfig.Tmpfs["/tmp"])
}
