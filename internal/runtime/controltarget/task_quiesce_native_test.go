//go:build linux

package controltarget

import (
	"github.com/stretchr/testify/require"
	"os"
	"syscall"
	"testing"
)

// Root owns the isolated, quota-bounded Linux process and root0700 tmpfs.
// This gate proves changed journal IO only, not PID1 or namespace drain.
func TestTaskQuiesceJournalNative(t *testing.T) {
	if os.Getenv("SANDBOX_TASK_QUIESCE_JOURNAL_NATIVE") != "1" {
		t.Skip("requires Root-owned native journal fixture")
	}
	require.Zero(t, os.Geteuid())
	require.Zero(t, os.Getegid())
	require.Equal(t, "/journal", os.Getenv("TMPDIR"))
	info, err := os.Lstat("/journal")
	require.NoError(t, err)
	require.True(t, info.IsDir())
	require.Equal(t, os.FileMode(0700), info.Mode().Perm())
	require.Zero(t, info.Mode()&os.ModeSymlink)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	require.Zero(t, stat.Uid)
	require.Zero(t, stat.Gid)
	t.Run("Lifecycle", TestTaskQuiesceJournal)
	t.Run("Persistence", TestTaskQuiesceJournalFaults)
	t.Run("LateAuthentication", TestTaskQuiesceJournalLate)
	t.Run("Capacity", TestTaskQuiesceCapacity)
	t.Run("ColdHistory", TestTaskQuiesceHistory)
	t.Run("Conflicts", TestTaskQuiesceHistoryConflicts)
	t.Run("Authorization", TestTaskQuiesceJournalAuthorization)
	t.Run("HistoricalPrerequisite", TestTaskQuiesceJournalHistoricalPrerequisite)
	t.Run("Signer", TestTaskQuiesceJournalSigner)
	t.Run("SignerClose", TestTaskQuiesceJournalSignerClose)
}
