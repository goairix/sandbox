package workspaceprobe

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMountInfoRequiresEffectiveFuseS3FS(t *testing.T) {
	fixture := strings.NewReader(
		"20 1 0:1 / / rw - overlay overlay rw\n" +
			"21 20 0:2 / /workspace rw,nosuid - fuse.s3fs bucket rw\n",
	)
	require.NoError(t, requireEffectiveS3FS(fixture, "/workspace", 21))
}

func TestMountInfoRejectsUnderlyingOrWrongFilesystem(t *testing.T) {
	for _, fixture := range []string{
		"20 1 0:1 / / rw - fuse.s3fs bucket rw\n",
		"20 1 0:1 / / rw - overlay overlay rw\n21 20 0:2 / /workspace rw - tmpfs tmpfs rw\n",
		"20 1 0:1 / / rw - overlay overlay rw\n21 20 0:2 / /workspace-old rw - fuse.s3fs bucket rw\n",
	} {
		require.Error(t, requireEffectiveS3FS(strings.NewReader(fixture), "/workspace", 21), fixture)
	}
}

func TestMountInfoDecodesEscapedMountpoint(t *testing.T) {
	fixture := strings.NewReader("21 20 0:2 / /work\\040space rw - fuse.s3fs bucket rw\n")
	require.NoError(t, requireEffectiveS3FS(fixture, "/work space", 21))
}

func TestMountInfoUsesStatxMountIDForStackedSamePathMounts(t *testing.T) {
	for _, fixture := range []string{
		"20 1 0:1 / /workspace rw - tmpfs tmpfs rw\n21 20 0:2 / /workspace rw - fuse.s3fs bucket rw\n",
		"21 20 0:2 / /workspace rw - fuse.s3fs bucket rw\n20 1 0:1 / /workspace rw - tmpfs tmpfs rw\n",
	} {
		require.NoError(t, requireEffectiveS3FS(strings.NewReader(fixture), "/workspace", 21))
		require.Error(t, requireEffectiveS3FS(strings.NewReader(fixture), "/workspace", 20))
	}
}

func TestMountInfoRejectsMissingStatxMountID(t *testing.T) {
	fixture := strings.NewReader("21 20 0:2 / /workspace rw - fuse.s3fs bucket rw\n")
	require.Error(t, requireEffectiveS3FS(fixture, "/workspace", 99))
}
