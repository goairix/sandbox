package docker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
)

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
