package workspaceprobe

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseProcStatHandlesParenthesesAndSpaces(t *testing.T) {
	identity, err := parseProcStat(42, []byte("42 (odd ) process) T 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 98765 20\n"))
	require.NoError(t, err)
	assert.Equal(t, 42, identity.PID)
	assert.Equal(t, byte('T'), identity.State)
	assert.Equal(t, uint64(98765), identity.StartTime)
}

func TestProcDescriptorsReadFlagsAndTargets(t *testing.T) {
	root := t.TempDir()
	fdDir := filepath.Join(root, "10", "fd")
	fdInfoDir := filepath.Join(root, "10", "fdinfo")
	require.NoError(t, os.MkdirAll(fdDir, 0o755))
	require.NoError(t, os.MkdirAll(fdInfoDir, 0o755))
	require.NoError(t, os.Symlink("/workspace/data", filepath.Join(fdDir, "3")))
	require.NoError(t, os.WriteFile(filepath.Join(fdInfoDir, "3"), []byte("pos:\t0\nflags:\t0100001\n"), 0o600))

	proc := procFS{root: root}
	fds, err := proc.descriptors(10)
	require.NoError(t, err)
	require.Len(t, fds, 1)
	assert.Equal(t, "/workspace/data", fds[0].Target)
	assert.Equal(t, syscall.O_WRONLY, fds[0].Flags&syscall.O_ACCMODE)
}

func TestWorkspaceTargetRequiresPathBoundary(t *testing.T) {
	assert.True(t, targetInWorkspace("/workspace/file", "/workspace"))
	assert.True(t, targetInWorkspace("/workspace/file (deleted)", "/workspace"))
	assert.False(t, targetInWorkspace("/workspace-escape/file", "/workspace"))
}

func TestParseProcessUIDUsesEffectiveIdentity(t *testing.T) {
	uid, err := parseEffectiveUID([]byte("Name:\ttest\nUid:\t501\t1000\t1000\t1000\n"))
	require.NoError(t, err)
	assert.Equal(t, 1000, uid)
}
