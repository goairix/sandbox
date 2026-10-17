package mounter

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Removing the actual PID1 check must fail before any configured directory is
// created. These paths configure storage only; they cannot supply an identity.
func TestNativeSupervisorRejectsNonPID1(t *testing.T) {
	if os.Getpid() == 1 {
		t.Skip("the non-PID1 rejection case runs outside the native PID namespace")
	}

	root := t.TempDir()
	supervisor, err := NewNativeSupervisor(NativeConfig{
		RunDir:         filepath.Join(root, "run"),
		CredentialRoot: filepath.Join(root, "credentials"),
		CacheRoot:      filepath.Join(root, "cache"),
		MountPath:      filepath.Join(root, "workspace"),
	})
	if supervisor != nil {
		t.Cleanup(func() { _ = supervisor.Close() })
	}

	require.Error(t, err)
	require.Nil(t, supervisor)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries, "identity rejection must precede directory preparation")
}
