package metrics

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFUSEMetricsInitialize(t *testing.T) {
	ResetForTest()
	require.NoError(t, InitNoop())
	require.NotNil(t, SandboxWorkspacePoolSize)
	require.NotNil(t, SandboxWorkspacePoolAcquire)
	require.NotNil(t, SandboxWorkspaceMountDuration)
	require.NotNil(t, SandboxWorkspaceLeaseLost)
	require.NotNil(t, SandboxWorkspacePoolCleanupTotal)
	require.NotNil(t, SandboxWorkspacePoolCleanupDuration)
}
