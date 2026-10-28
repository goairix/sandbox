package docker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
)

type fileV2DockerAPI struct {
	*fakeDockerAPI
	output   []byte
	rawFrame []byte
	exitCode int
	running  bool
}

func (f *fileV2DockerAPI) ContainerExecAttach(_ context.Context, _ string, _ container.ExecAttachOptions) (types.HijackedResponse, error) {
	var framed bytes.Buffer
	if f.rawFrame != nil {
		_, _ = framed.Write(f.rawFrame)
	} else {
		_, _ = stdcopy.NewStdWriter(&framed, stdcopy.Stdout).Write(f.output)
	}
	conn := newFakeHijackConn(func([]byte) []byte { return framed.Bytes() })
	return types.HijackedResponse{Conn: conn, Reader: bufio.NewReader(conn)}, nil
}

func (f *fileV2DockerAPI) ContainerExecInspect(context.Context, string) (container.ExecInspect, error) {
	return container.ExecInspect{ExitCode: f.exitCode, Running: f.running}, nil
}

func TestFileV2DockerTransport(t *testing.T) {
	rt, base := newFakeDockerRuntime(t)
	base.containers["runtime-a"] = &fakeContainer{config: &container.Config{Labels: map[string]string{"sandbox.managed": "true", "sandbox.role": "fuse-runtime"}}, running: true}
	fake := &fileV2DockerAPI{fakeDockerAPI: base, output: []byte(`{"name":"a"}`)}
	rt.cli = fake
	stream, err := rt.RunFileOp(context.Background(), "runtime-a", []string{"stat", "/workspace/a"}, nil)
	require.NoError(t, err)
	content, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.JSONEq(t, string(fake.output), string(content))
	require.NoError(t, stream.Close())
	base.mu.Lock()
	for _, exec := range base.execs {
		require.Equal(t, []string{"/usr/local/bin/sandbox-file-op", "stat", "/workspace/a"}, exec.options.Cmd)
		require.Equal(t, dockerPublicUser, exec.options.User)
		require.False(t, exec.options.Tty)
	}
	base.mu.Unlock()

	fake.exitCode = 7
	stream, err = rt.RunFileOp(context.Background(), "runtime-a", []string{"stat", "/workspace/a"}, nil)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, stream)
	require.ErrorIs(t, stream.Close(), runtime.ErrFileConflict)
	fake.exitCode = 2
	stream, err = rt.RunFileOp(context.Background(), "runtime-a", []string{"stat", "/workspace/missing"}, nil)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, stream)
	require.ErrorIs(t, stream.Close(), runtime.ErrFileNotFound)
	_, err = rt.RunFileOp(context.Background(), "runtime-a", []string{"stat", strings.Repeat("x", 4097)}, nil)
	require.Error(t, err)
}

func TestFileV2DockerTransportRejectsUnfinishedHelper(t *testing.T) {
	rt, base := newFakeDockerRuntime(t)
	base.containers["runtime-a"] = &fakeContainer{config: &container.Config{Labels: map[string]string{"sandbox.managed": "true", "sandbox.role": "fuse-runtime"}}, running: true}
	fake := &fileV2DockerAPI{fakeDockerAPI: base, running: true}
	rt.cli = fake
	stream, err := rt.RunFileOp(context.Background(), "runtime-a", []string{"write", "/workspace/a", "0"}, nil)
	require.NoError(t, err)
	_, readErr := io.Copy(io.Discard, stream)
	closeErr := stream.Close()
	require.Error(t, errors.Join(readErr, closeErr), "EOF on the attach stream is not proof of helper completion")
}

func TestFileV2DockerTransportRejectsTruncatedOutput(t *testing.T) {
	rt, base := newFakeDockerRuntime(t)
	base.containers["runtime-a"] = &fakeContainer{config: &container.Config{Labels: map[string]string{"sandbox.managed": "true", "sandbox.role": "fuse-runtime"}}, running: true}
	fake := &fileV2DockerAPI{fakeDockerAPI: base, rawFrame: []byte{1, 0, 0, 0, 0, 0, 0, 4, 'x'}}
	rt.cli = fake
	stream, err := rt.RunFileOp(context.Background(), "runtime-a", []string{"read", "/workspace/a", "0", "4"}, nil)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, stream)
	require.Error(t, stream.Close(), "a truncated Docker frame cannot be accepted as file content")
}
