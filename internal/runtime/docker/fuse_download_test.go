package docker

import (
	"archive/tar"
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
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
)

type fuseDownloadDockerAPI struct {
	*fakeDockerAPI
	stat          runtime.ExecResult
	tarExit       int
	tarStderr     string
	attachErr     error
	inspectErr    error
	tarInspectErr error
}

func (f *fuseDownloadDockerAPI) ContainerExecAttach(_ context.Context, id string, _ container.ExecAttachOptions) (types.HijackedResponse, error) {
	if f.attachErr != nil {
		return types.HijackedResponse{}, f.attachErr
	}
	f.mu.Lock()
	opts := f.execs[id].options
	f.mu.Unlock()
	var stdout, stderr bytes.Buffer
	if opts.Cmd[0] == "sh" && strings.Contains(opts.Cmd[2], "stat") {
		stdout.WriteString(f.stat.Stdout)
		stderr.WriteString(f.stat.Stderr)
	} else if f.tarExit != 0 {
		stderr.WriteString(f.tarStderr)
	} else {
		tw := tar.NewWriter(&stdout)
		if err := tw.WriteHeader(&tar.Header{Name: "workspace/empty.txt", Mode: 0o644}); err != nil {
			return types.HijackedResponse{}, err
		}
		if err := tw.Close(); err != nil {
			return types.HijackedResponse{}, err
		}
	}
	var framed bytes.Buffer
	if _, err := stdcopy.NewStdWriter(&framed, stdcopy.Stdout).Write(stdout.Bytes()); err != nil {
		return types.HijackedResponse{}, err
	}
	if _, err := stdcopy.NewStdWriter(&framed, stdcopy.Stderr).Write(stderr.Bytes()); err != nil {
		return types.HijackedResponse{}, err
	}
	conn := newFakeHijackConn(func([]byte) []byte { return framed.Bytes() })
	if err := conn.CloseWrite(); err != nil {
		return types.HijackedResponse{}, err
	}
	return types.HijackedResponse{Conn: conn, Reader: bufio.NewReader(conn)}, nil
}

func (f *fuseDownloadDockerAPI) ContainerExecInspect(_ context.Context, id string) (container.ExecInspect, error) {
	if f.inspectErr != nil {
		return container.ExecInspect{}, f.inspectErr
	}
	f.mu.Lock()
	opts := f.execs[id].options
	f.mu.Unlock()
	if opts.Cmd[0] == "sh" && strings.Contains(opts.Cmd[2], "stat") {
		return container.ExecInspect{ExitCode: f.stat.ExitCode}, nil
	}
	if f.tarInspectErr != nil {
		return container.ExecInspect{}, f.tarInspectErr
	}
	return container.ExecInspect{ExitCode: f.tarExit}, nil
}

func newFUSEDownloadRuntime(t *testing.T) (*Runtime, *fuseDownloadDockerAPI) {
	t.Helper()
	rt, base := newFakeDockerRuntime(t)
	base.containers["runtime-a"] = &fakeContainer{config: &container.Config{Labels: map[string]string{"sandbox.managed": "true", "sandbox.role": "fuse-runtime"}}, name: "runtime-a", running: true}
	fake := &fuseDownloadDockerAPI{fakeDockerAPI: base, stat: runtime.ExecResult{Stdout: "regular empty file\n"}}
	rt.cli = fake
	return rt, fake
}

func TestDockerFUSEDownloadMissingFileFailsBeforeTar(t *testing.T) {
	rt, fake := newFUSEDownloadRuntime(t)
	fake.stat = runtime.ExecResult{ExitCode: 1, Stderr: "stat: cannot statx '/workspace/missing.txt': No such file or directory\n"}
	fake.tarExit = 2
	fake.tarStderr = "tar: /workspace/missing.txt: Cannot stat: No such file or directory\n"
	reader, err := rt.DownloadFile(context.Background(), "runtime-a", "/workspace/missing.txt")
	if reader != nil {
		_, _ = io.Copy(io.Discard, reader)
		require.NoError(t, reader.Close())
	}
	require.ErrorIs(t, err, runtime.ErrFileNotFound)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, call := range fake.execs {
		require.NotEqual(t, "tar", call.options.Cmd[0], "known missing file must not open a tar stream")
	}
}

func TestDockerFUSEDownloadEmptyFileStreamsAsTar(t *testing.T) {
	rt, fake := newFUSEDownloadRuntime(t)
	reader, err := rt.DownloadFile(context.Background(), "runtime-a", "/workspace/empty.txt")
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()
	tr := tar.NewReader(reader)
	header, err := tr.Next()
	require.NoError(t, err)
	require.Zero(t, header.Size)
	_, err = io.Copy(io.Discard, reader)
	require.NoError(t, err)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, call := range fake.execs {
		require.Equal(t, dockerPublicUser, call.options.User)
	}
}

func TestDockerFUSEDownloadPreservesPreflightAndTarFailures(t *testing.T) {
	for _, tc := range []struct {
		name          string
		stat          runtime.ExecResult
		tarExit       int
		tarStderr     string
		attachErr     error
		inspectErr    error
		tarInspectErr error
		stream        bool
	}{
		{name: "permission", stat: runtime.ExecResult{ExitCode: 1, Stderr: "stat: cannot statx '/workspace/a': Permission denied\n"}},
		{name: "wrong ENOENT exit", stat: runtime.ExecResult{ExitCode: 2, Stderr: "stat: invalid argument: No such file or directory\n"}},
		{name: "unrelated not found", stat: runtime.ExecResult{ExitCode: 1, Stderr: "stat: command not found\n"}},
		{name: "transport", attachErr: errors.New("connection reset")},
		{name: "runtime inspect", inspectErr: errors.New("container gone")},
		{name: "late tar failure", stat: runtime.ExecResult{Stdout: "regular file\n"}, tarExit: 2, tarStderr: "tar: Permission denied\n", stream: true},
		{name: "late tar inspect failure", stat: runtime.ExecResult{Stdout: "regular file\n"}, tarInspectErr: errors.New("exec inspection unavailable"), stream: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, fake := newFUSEDownloadRuntime(t)
			fake.stat, fake.tarExit, fake.tarStderr, fake.attachErr, fake.inspectErr = tc.stat, tc.tarExit, tc.tarStderr, tc.attachErr, tc.inspectErr
			fake.tarInspectErr = tc.tarInspectErr
			reader, err := rt.DownloadFile(context.Background(), "runtime-a", "/workspace/a")
			if tc.stream {
				require.NoError(t, err)
				_, err = io.Copy(io.Discard, reader)
			}
			if reader != nil {
				require.NoError(t, reader.Close())
			}
			require.Error(t, err)
			require.NotErrorIs(t, err, runtime.ErrFileNotFound)
			if tc.attachErr != nil {
				require.ErrorIs(t, err, tc.attachErr)
			}
			if tc.inspectErr != nil {
				require.ErrorIs(t, err, tc.inspectErr)
			}
			if tc.tarInspectErr != nil {
				require.ErrorIs(t, err, tc.tarInspectErr)
			}
		})
	}
}

func TestDockerFUSEDownloadTarDiagnosticsAreBounded(t *testing.T) {
	rt, fake := newFUSEDownloadRuntime(t)
	fake.tarExit = 2
	fake.tarStderr = strings.Repeat("diagnostic", 4096)
	reader, err := rt.DownloadFile(context.Background(), "runtime-a", "/workspace/a")
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()
	_, err = io.Copy(io.Discard, reader)
	require.Error(t, err)
	require.Less(t, len(err.Error()), 5<<10)
}
