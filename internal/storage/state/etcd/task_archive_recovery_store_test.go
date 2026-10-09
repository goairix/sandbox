package etcd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func recoveryTestOptions(directory string, jobs uint64) TaskWorkspaceArchiveRecoveryOptions {
	l := archiveTestLimits()
	i, _ := taskArchiveIndexBytes(l.Entries)
	l.Jobs, l.TotalBytes = jobs, jobs*l.JobBytes
	l.JobIndexBytes = i + 4*taskArchiveRecoveryMetadataBytes
	l.TotalIndexBytes = jobs * l.JobIndexBytes
	return TaskWorkspaceArchiveRecoveryOptions{TaskWorkspaceArchiveSpoolOptions{directory, l.Jobs, l.JobBytes, l.TotalBytes, l.Entries, l.JobIndexBytes, l.TotalIndexBytes}}
}

func TestTaskArchiveRecoveryRetention(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux persistent profile; host codec/limits remain exercised")
	}
	d := archiveTestDirectory(t)
	o := recoveryTestOptions(d, 2)
	s, err := newTaskArchiveRecoveryStore(o)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.reserveRecovery(context.Background(), recoveryTestSeed(t))
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(d, r.name)
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(name); err != nil {
		t.Fatalf("persistent original archive missing after Close: %v", err)
	}
	cold, err := newTaskArchiveRecoveryStore(o)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cold.close()) })
	diagnostic, err := cold.coldSnapshot(0)
	require.NoError(t, err)
	require.True(t, diagnostic.Occupied)
	require.True(t, diagnostic.Quarantined, "pending TAR never becomes received authority")
	require.Equal(t, uint64(1), cold.jobs)
	require.Equal(t, o.JobBytes, cold.disk)
	_, err = cold.reserveRecovery(context.Background(), recoveryTestSeed(t))
	require.NoError(t, err, "a separate confirmed empty slot remains admissible")
}

func TestTaskArchiveRecoveryStoreUnsupported(t *testing.T) {
	if runtime.GOOS == "linux" {
		require.NoError(t, taskArchiveRecoverySupported())
		return
	}
	require.Error(t, taskArchiveRecoverySupported())
	_, err := newTaskArchiveRecoveryStore(recoveryTestOptions(archiveTestDirectory(t), 1))
	require.Error(t, err)
}

func TestTaskArchiveRecoveryColdContentQuota(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux descriptor store")
	}
	for _, extension := range []string{"tar", "json", "next"} {
		t.Run(extension, func(t *testing.T) {
			o := recoveryTestOptions(archiveTestDirectory(t), 2)
			limit := uint64(taskArchiveRecoveryMetadataBytes)
			if extension == "tar" {
				limit = o.JobBytes - 2*taskArchiveRecoveryMetadataBytes
			}
			path := filepath.Join(o.Directory, "slot-00."+extension)
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			require.NoError(t, err)
			require.NoError(t, f.Truncate(int64(limit+1)))
			require.NoError(t, f.Close())
			cold, err := newTaskArchiveRecoveryStore(o)
			if cold != nil {
				require.NoError(t, cold.close())
			}
			require.Error(t, err, "oversized orphan must reject empty-slot admission")
			st, err := os.Lstat(path)
			require.NoError(t, err)
			require.Equal(t, int64(limit+1), st.Size(), "rejection preserves content")
		})
	}
}
