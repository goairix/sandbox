package kubernetes

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/goairix/sandbox/internal/runtime"
)

func TestWriteSizedTarRejectsInvalidBodyLengths(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int64
		body string
	}{
		{name: "negative", size: -1},
		{name: "short", size: 5, body: "four"},
		{name: "excess", size: 4, body: "extra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := writeSizedTar(io.Discard, "a.txt", 0o644, 1000, 1000, tc.size, strings.NewReader(tc.body))
			require.ErrorIs(t, err, runtime.ErrInvalidUploadSize)
		})
	}
}

func TestRemovePartialPodUploadValidatesExecResult(t *testing.T) {
	original := partialUploadPodExec
	t.Cleanup(func() { partialUploadPodExec = original })
	transportErr := errors.New("transport failed")
	partialUploadPodExec = func(context.Context, kubernetes.Interface, *rest.Config, string, string, runtime.ExecRequest) (*runtime.ExecResult, error) {
		return nil, transportErr
	}
	require.ErrorIs(t, removePartialPodUpload(context.Background(), nil, nil, "runtime", "pod-a", "/tmp/a"), transportErr)

	partialUploadPodExec = func(context.Context, kubernetes.Interface, *rest.Config, string, string, runtime.ExecRequest) (*runtime.ExecResult, error) {
		return nil, nil
	}
	require.ErrorContains(t, removePartialPodUpload(context.Background(), nil, nil, "runtime", "pod-a", "/tmp/a"), "no result")

	partialUploadPodExec = func(context.Context, kubernetes.Interface, *rest.Config, string, string, runtime.ExecRequest) (*runtime.ExecResult, error) {
		return &runtime.ExecResult{ExitCode: 7, Stderr: "permission denied"}, nil
	}
	err := removePartialPodUpload(context.Background(), nil, nil, "runtime", "pod-a", "/tmp/a")
	require.ErrorContains(t, err, "exited 7")
	require.ErrorContains(t, err, "permission denied")
}

func TestUploadFileErrorJoinsPartialCleanupFailure(t *testing.T) {
	originalConsume := consumePodUpload
	originalCleanup := partialUploadPodExec
	t.Cleanup(func() {
		consumePodUpload = originalConsume
		partialUploadPodExec = originalCleanup
	})
	consumePodUpload = func(_ context.Context, _ kubernetes.Interface, _ *rest.Config, _, _, _ string, input io.Reader) error {
		_, _ = io.Copy(io.Discard, input)
		return nil
	}
	cleanupErr := errors.New("cleanup failed")
	partialUploadPodExec = func(context.Context, kubernetes.Interface, *rest.Config, string, string, runtime.ExecRequest) (*runtime.ExecResult, error) {
		return nil, cleanupErr
	}

	err := uploadFileToPod(context.Background(), nil, nil, "runtime", "pod-a", "/workspace/a", 5, strings.NewReader("four"))
	require.ErrorIs(t, err, runtime.ErrInvalidUploadSize)
	require.ErrorIs(t, err, cleanupErr)
}

func TestUploadFileErrorPreservesInvalidSizeWhenCleanupSucceeds(t *testing.T) {
	originalConsume := consumePodUpload
	originalCleanup := partialUploadPodExec
	t.Cleanup(func() {
		consumePodUpload = originalConsume
		partialUploadPodExec = originalCleanup
	})
	consumePodUpload = func(_ context.Context, _ kubernetes.Interface, _ *rest.Config, _, _, _ string, input io.Reader) error {
		_, _ = io.Copy(io.Discard, input)
		return nil
	}
	partialUploadPodExec = func(context.Context, kubernetes.Interface, *rest.Config, string, string, runtime.ExecRequest) (*runtime.ExecResult, error) {
		return &runtime.ExecResult{}, nil
	}

	err := uploadFileToPod(context.Background(), nil, nil, "runtime", "pod-a", "/workspace/a", 5, strings.NewReader("four"))
	require.ErrorIs(t, err, runtime.ErrInvalidUploadSize)
	assert.NotContains(t, err.Error(), "cleanup partial upload")
}

func TestUploadFileCommandRejectsDirectoryDestination(t *testing.T) {
	command := uploadFileCommand("/workspace/.sandbox-upload-temp", "/workspace/target")
	assert.Contains(t, command, "test ! -d")
	assert.Contains(t, command, "mv -f --")
}
