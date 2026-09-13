package kubernetes

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
)

func TestExactPodCommandRejectsReplacementBeforeAnySideEffect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "written")
	ctx := runtime.WithExactRuntimeRef(context.Background(), runtime.RuntimeRef{ID: "pod-a", UID: "uid-old"})
	argv, err := exactPodCommand(ctx, "pod-a", []string{"touch", path})
	require.NoError(t, err)
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Env = append(os.Environ(), "SANDBOX_POD_UID=uid-replacement")
	err = command.Run()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	require.Equal(t, 125, exit.ExitCode())
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err))
	command = exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Env = append(os.Environ(), "SANDBOX_POD_UID=uid-old")
	require.NoError(t, command.Run())
	_, err = exactPodCommand(ctx, "other-pod", []string{"true"})
	require.ErrorIs(t, err, runtime.ErrInvalidRuntimeRef)
}
