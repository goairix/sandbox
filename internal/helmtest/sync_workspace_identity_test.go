//go:build helmtests

package helmtest

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A sync-only object-store deployment still needs the immutable namespace
// identity for its distributed workspace lease. Disabling FUSE must not erase
// that identity from the API process configuration.
func TestSyncOnlyWorkspaceKeepsStorageIdentity(t *testing.T) {
	docs := render(t,
		"config.workspace.enabledMountModes={sync}",
		"config.workspace.defaultMountMode=sync",
		"config.storage.filesystem.preset=minio",
		"config.storage.filesystem.storageIdentity=sync-object-store",
	)
	deployment := object(t, docs, "Deployment", "sandbox-api")
	pod := mapping(t, mapping(t, mapping(t, deployment["spec"])["template"])["spec"])
	api := container(t, pod, "containers", "sandbox")
	variables := env(t, api)

	require.Equal(t, "sync-object-store", variables["SANDBOX_WORKSPACE_BACKEND_STORAGE_IDENTITY"]["value"])
	require.Equal(t, "minio", variables["SANDBOX_WORKSPACE_BACKEND_PRESET"]["value"])
	require.Equal(t, "minio-sigv4-path-style-v1", variables["SANDBOX_WORKSPACE_BACKEND_PROFILE"]["value"])
}

func TestSyncOnlyWorkspaceUsesPrivateHTTPMinIOProfile(t *testing.T) {
	docs := render(t,
		"config.workspace.enabledMountModes={sync}",
		"config.workspace.defaultMountMode=sync",
		"config.storage.filesystem.preset=minio",
		"config.storage.filesystem.useSSL=false",
		"config.storage.filesystem.storageIdentity=sync-object-store",
	)
	deployment := object(t, docs, "Deployment", "sandbox-api")
	pod := mapping(t, mapping(t, mapping(t, deployment["spec"])["template"])["spec"])
	api := container(t, pod, "containers", "sandbox")
	variables := env(t, api)

	require.Equal(t, "minio-sigv4-path-style-private-http-v1", variables["SANDBOX_WORKSPACE_BACKEND_PROFILE"]["value"])
}
