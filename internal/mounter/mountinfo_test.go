package mounter

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseMountInfoUsesSeparatorAndDecodesEscapes(t *testing.T) {
	raw := "36 25 0:32 / /workspace rw,nosuid - tmpfs tmpfs rw\n" +
		"52 36 0:55 / /workspace rw,nosuid shared:1 - fuse.s3fs bucket:/workspaces/a rw,user_id=0\n" +
		"53 36 0:56 / /workspace\\040other rw - fuse.s3fs bucket:/other rw\n"
	mounts, err := ParseMountInfo(strings.NewReader(raw))
	require.NoError(t, err)
	require.Len(t, mounts, 3)
	assert.Equal(t, "fuse.s3fs", mounts[1].FilesystemType)
	assert.Equal(t, "/workspace other", mounts[2].MountPoint)
	assert.Equal(t, "bucket:/workspaces/a", mounts[1].Source)
}

func TestEffectiveMountUsesKernelMountIDForStackedMount(t *testing.T) {
	raw := "36 25 0:32 / /workspace rw - tmpfs tmpfs rw\n52 36 0:55 / /workspace rw - fuse.s3fs bucket:/a rw\n"
	mount, err := EffectiveMount(strings.NewReader(raw), "/workspace", func(string) (uint64, error) { return 52, nil })
	require.NoError(t, err)
	assert.Equal(t, "fuse.s3fs", mount.FilesystemType)

	_, err = EffectiveMount(strings.NewReader(raw), "/workspace", func(string) (uint64, error) { return 99, nil })
	require.Error(t, err)
}

func TestParseMountInfoRejectsMalformedAndOversizedInput(t *testing.T) {
	_, err := ParseMountInfo(strings.NewReader("36 25 malformed\n"))
	require.Error(t, err)
	_, err = ParseMountInfo(strings.NewReader(strings.Repeat("x", maxMountInfoBytes+1)))
	require.Error(t, err)
}

func TestParseMountInfoRejectsUnknownEscapesEmptyAndConflictingIDs(t *testing.T) {
	for _, raw := range []string{
		"",
		"36 25 0:32 / /work\\141space rw - tmpfs tmpfs rw\n",
		"36 25 0:32 / /workspace rw - tmpfs tmpfs rw\n36 25 0:33 / /workspace rw - fuse.s3fs bucket:/a rw\n",
	} {
		_, err := ParseMountInfo(strings.NewReader(raw))
		require.Error(t, err, raw)
	}
}

func TestParseMountInfoAcceptsLiteralBackslashEscape(t *testing.T) {
	mounts, err := ParseMountInfo(strings.NewReader("36 25 0:32 / /work\\134space rw - tmpfs tmpfs rw\n"))
	require.NoError(t, err)
	require.Len(t, mounts, 1)
	assert.Equal(t, `/work\space`, mounts[0].MountPoint)
}
