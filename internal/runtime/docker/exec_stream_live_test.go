package docker

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
)

// Set SANDBOX_DOCKER_STREAM_CONTAINER to a disposable, running FUSE runtime
// container ID to exercise the real Docker attach protocol and file helper.
func TestDockerExecStreamAndFileV2Live(t *testing.T) {
	id := os.Getenv("SANDBOX_DOCKER_STREAM_CONTAINER")
	if id == "" {
		t.Skip("set SANDBOX_DOCKER_STREAM_CONTAINER to a disposable FUSE runtime container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rt, err := NewForInspection(ctx, "", "")
	require.NoError(t, err)
	defer rt.Close()

	stream, err := rt.ExecStream(ctx, id, runtime.ExecRequest{Command: "printf 'first\\n'; sleep 1; printf 'second\\n' >&2; exit 7"})
	require.NoError(t, err)
	select {
	case first := <-stream:
		require.Equal(t, runtime.StreamStdout, first.Type)
		require.Equal(t, "first\n", first.Content)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Docker buffered the first output until the process exited")
	}
	var events []runtime.StreamEvent
	for event := range stream {
		events = append(events, event)
	}
	require.Equal(t, []runtime.StreamEvent{
		{Type: runtime.StreamStderr, Content: "second\n"},
		{Type: runtime.StreamDone, Content: "7"},
	}, events)

	stream, err = rt.ExecStream(ctx, id, runtime.ExecRequest{Command: "cat", Stdin: "docker-input"})
	require.NoError(t, err)
	events = events[:0]
	for event := range stream {
		events = append(events, event)
	}
	require.Equal(t, []runtime.StreamEvent{
		{Type: runtime.StreamStdout, Content: "docker-input"},
		{Type: runtime.StreamDone, Content: "0"},
	}, events)

	file := "/workspace/.codex-v2-stream-smoke"
	defer func() {
		cleanup, cleanupErr := rt.ExecStream(context.Background(), id, runtime.ExecRequest{Command: "rm -f " + file})
		if cleanupErr == nil {
			for range cleanup {
			}
		}
	}()
	writer, err := rt.RunFileOp(ctx, id, []string{"write", file, "12"}, strings.NewReader("file-v2-data"))
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, writer)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	reader, err := rt.RunFileOp(ctx, id, []string{"read", file, "0", "12"}, nil)
	require.NoError(t, err)
	content, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, "file-v2-data", string(content))
}
