package docker

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/stretchr/testify/require"
)

type fileV2DockerAPI struct {
	*fakeDockerAPI
	output   []byte
	exitCode int
}

func (f *fileV2DockerAPI) ContainerExecAttach(_ context.Context, _ string, _ container.ExecAttachOptions) (types.HijackedResponse, error) {
	var framed bytes.Buffer
	_, _ = stdcopy.NewStdWriter(&framed, stdcopy.Stdout).Write(f.output)
	conn := newFakeHijackConn(func([]byte) []byte { return framed.Bytes() })
	return types.HijackedResponse{Conn: conn, Reader: bufio.NewReader(conn)}, nil
}

func (f *fileV2DockerAPI) ContainerExecInspect(context.Context, string) (container.ExecInspect, error) {
	return container.ExecInspect{ExitCode: f.exitCode}, nil
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
	require.ErrorContains(t, stream.Close(), "exited with code 7")
	_, err = rt.RunFileOp(context.Background(), "runtime-a", []string{"stat", strings.Repeat("x", 4097)}, nil)
	require.Error(t, err)
}
