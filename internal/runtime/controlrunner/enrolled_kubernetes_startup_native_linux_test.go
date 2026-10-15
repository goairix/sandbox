//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"testing"

	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestEnrolledKubernetesStartupBridgeNative(t *testing.T) {
	runEnrolledLauncherFixture(t, []string{"startup"}, true, true)
}

func startupNativeInitialPermissions(t *testing.T) {
	t.Helper()
	for _, path := range []string{ct.ControlDirectory, "/workspace", "/tmp"} {
		var st unix.Stat_t
		require.NoError(t, unix.Stat(path, &st))
		require.Zero(t, st.Uid)
		require.Zero(t, st.Gid)
		if path == ct.ControlDirectory {
			require.Equal(t, uint32(unix.S_IFDIR|01777), st.Mode, "actual fresh tmpfs without mode option")
			var fs unix.Statfs_t
			require.NoError(t, unix.Statfs(path, &fs))
			require.Equal(t, int64(unix.TMPFS_MAGIC), fs.Type)
			require.LessOrEqual(t, uint64(fs.Bsize)*fs.Blocks, uint64(1048576))
			t.Logf("startup actual control tmpfs bytes=%d without mount mode override", uint64(fs.Bsize)*fs.Blocks)
		}
		if path == "/workspace" {
			require.Equal(t, uint32(unix.S_IFDIR|0777), st.Mode, "root-owned disk EmptyDir permission analogue on bounded tmpfs")
		}
		t.Logf("startup actual initial path=%s uid=%d gid=%d mode=%#o", path, st.Uid, st.Gid, st.Mode)
	}
}
