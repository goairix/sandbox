//go:build linux

package etcd

import (
	"archive/tar"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTaskWorkspaceSyncLocalFinalizationNativeRegular(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	data := archiveTarBytes(t, []*tar.Header{{Name: "workspace", Typeflag: tar.TypeDir, Mode: 0000}, {Name: "workspace/z", Typeflag: tar.TypeDir, Mode: 0555}, {Name: "workspace/z/file", Typeflag: tar.TypeReg, Mode: 0444, Size: 4}, {Name: "workspace/empty", Typeflag: tar.TypeDir, Mode: 0000}}, []string{"", "", "body", ""})
	var root string
	f, wire, slot := recoveryExecutionColdData(t, data, 4, func(_ *PreparedTaskWorkspaceSyncSource, _ *taskWorkspaceSyncBody, target *taskWorkspaceSyncLocalTarget) {
		root = target.pin.Root
		t.Cleanup(func() {
			_ = os.Chmod(filepath.Join(root, "z"), 0700)
			_ = os.Chmod(filepath.Join(root, "empty"), 0700)
		})
		require.NoError(t, os.WriteFile(filepath.Join(root, "z"), []byte("blocking"), 0600))
	})
	r, err := f.b.FinalizeTaskWorkspaceSyncLocal(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker)
	require.NoError(t, err)
	require.True(t, r.LocalComplete)
	require.NotNil(t, r.Completion)
	actual, err := os.ReadFile(filepath.Join(root, "z/file"))
	require.NoError(t, err)
	require.Equal(t, "body", string(actual))
	for path, mode := range map[string]os.FileMode{"z": 0555, "empty": 0000, ".": 0700} {
		st, err := os.Stat(filepath.Join(root, path))
		require.NoError(t, err)
		require.Equal(t, mode, st.Mode().Perm())
	}
}
