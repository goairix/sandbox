package workspaceprobe

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcEnumerationExcludesExitedProcessesButKeepsBlockedProcesses(t *testing.T) {
	root := t.TempDir()
	for pid, state := range map[int]byte{10: 'Z', 11: 'X', 12: 'x', 13: 'D', 14: 'S', 15: 'T'} {
		dir := filepath.Join(root, fmt.Sprint(pid))
		require.NoError(t, os.Mkdir(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "stat"), []byte(fmt.Sprintf("%d (worker) %c 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 1 18 98765 20\n", pid, state)), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "status"), []byte("Uid:\t1000\t1000\t1000\t1000\n"), 0o600))
	}
	processes, err := (procFS{root: root}).listSameUID(1000, 99)
	require.NoError(t, err)
	var pids []int
	for _, process := range processes {
		pids = append(pids, process.PID)
	}
	assert.ElementsMatch(t, []int{13, 14, 15}, pids, "exited processes cannot execute; a blocked live process must still prevent unsafe quiescence")
}

func TestProcEnumerationRetainsZombieLeaderWithLiveSiblingThreads(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "10")
	require.NoError(t, os.Mkdir(dir, 0o755))
	// Field 20 counts the thread group, including the zombie leader. Other
	// threads do not appear in the top-level /proc directory enumeration.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stat"), []byte("10 (worker) Z 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 2 18 98765 20\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "status"), []byte("Uid:\t1000\t1000\t1000\t1000\n"), 0o600))
	processes, err := (procFS{root: root}).listSameUID(1000, 99)
	require.NoError(t, err)
	require.Len(t, processes, 1, "leader exit must not hide a live worker from quiescence")
	assert.Equal(t, 10, processes[0].PID)
}

func TestParseProcStatHandlesParenthesesAndSpaces(t *testing.T) {
	identity, err := parseProcStat(42, []byte("42 (odd ) process) T 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 98765 20\n"))
	require.NoError(t, err)
	assert.Equal(t, 42, identity.PID)
	assert.Equal(t, byte('T'), identity.State)
	assert.Equal(t, uint64(98765), identity.StartTime)
}

func TestParseProcStatRejectsUnknownThreadCount(t *testing.T) {
	for _, count := range []string{"0", "-1", "unknown"} {
		_, err := parseProcStat(42, []byte(fmt.Sprintf("42 (worker) Z 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 %s 18 98765 20\n", count)))
		require.Error(t, err, "unknown thread count must not prove the entire process exited")
	}
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
