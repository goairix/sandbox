package kubernetes

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/goairix/sandbox/internal/runtime"
)

type fileV2Executor struct {
	finished chan struct{}
	exitCode int
}

func (e *fileV2Executor) Stream(remotecommand.StreamOptions) error {
	panic("deprecated Stream must not be called")
}

func (e *fileV2Executor) StreamWithContext(ctx context.Context, opts remotecommand.StreamOptions) error {
	defer close(e.finished)
	if e.exitCode != 0 {
		return utilexec.CodeExitError{Err: errors.New("helper failed"), Code: e.exitCode}
	}
	_, _ = io.WriteString(opts.Stdout, "payload")
	<-ctx.Done()
	return ctx.Err()
}

func TestFileV2KubernetesExitStatus(t *testing.T) {
	executor := &fileV2Executor{finished: make(chan struct{}), exitCode: 2}
	stream := streamFileV2Pod(context.Background(), executor, nil)
	_, err := io.ReadAll(stream)
	require.ErrorIs(t, err, runtime.ErrFileNotFound)
	require.ErrorIs(t, stream.Close(), runtime.ErrFileNotFound)
}

func TestFileV2KubernetesTransportCloseJoins(t *testing.T) {
	executor := &fileV2Executor{finished: make(chan struct{})}
	stream := streamFileV2Pod(context.Background(), executor, nil)
	var buffer [7]byte
	_, err := io.ReadFull(stream, buffer[:])
	require.NoError(t, err)
	require.Equal(t, "payload", string(buffer[:]))
	require.ErrorIs(t, stream.Close(), context.Canceled)
	select {
	case <-executor.finished:
	case <-time.After(time.Second):
		t.Fatal("Close returned before SPDY stream completed")
	}
}

func TestFileV2KubernetesCommand(t *testing.T) {
	command, err := fileV2PodCommand(context.Background(), "sandbox-a", []string{"stat", "/workspace/a"})
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(strings.Join(command, " "), "/usr/local/bin/sandbox-file-op stat /workspace/a"))
	require.Equal(t, "/usr/bin/env", command[0])
	_, err = fileV2PodCommand(context.Background(), "sandbox-a", []string{"stat", strings.Repeat("x", 4097)})
	require.Error(t, err)
}
