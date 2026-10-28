package docker

import (
	"bufio"
	"bytes"
	"context"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
)

type execStreamDockerAPI struct {
	*fakeDockerAPI
	output  []byte
	running bool
	input   []byte
}

func (f *execStreamDockerAPI) ContainerExecAttach(_ context.Context, id string, _ container.ExecAttachOptions) (types.HijackedResponse, error) {
	f.mu.Lock()
	attachStdin := f.execs[id].options.AttachStdin
	f.mu.Unlock()
	conn := newFakeHijackConn(func(input []byte) []byte {
		f.input = append([]byte(nil), input...)
		return f.output
	})
	if !attachStdin {
		_ = conn.CloseWrite()
	}
	return types.HijackedResponse{Conn: conn, Reader: bufio.NewReader(conn)}, nil
}

func (f *execStreamDockerAPI) ContainerExecInspect(context.Context, string) (container.ExecInspect, error) {
	return container.ExecInspect{Running: f.running, ExitCode: 0}, nil
}

func TestDockerExecStreamRejectsBrokenOutputAndUnfinishedProcess(t *testing.T) {
	for _, tc := range []struct {
		name    string
		output  []byte
		running bool
	}{
		{"truncated Docker frame", []byte{1, 0, 0, 0, 0, 0, 0, 4, 'x'}, false},
		{"exec still running", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, base := newFakeDockerRuntime(t)
			base.containers["runtime-a"] = &fakeContainer{config: &container.Config{Labels: map[string]string{"sandbox.managed": "true", "sandbox.role": "fuse-runtime"}}, running: true}
			rt.cli = &execStreamDockerAPI{fakeDockerAPI: base, output: tc.output, running: tc.running}
			stream, err := rt.ExecStream(context.Background(), "runtime-a", runtime.ExecRequest{Command: "true"})
			require.NoError(t, err)
			var events []runtime.StreamEvent
			for event := range stream {
				events = append(events, event)
			}
			require.NotEmpty(t, events)
			require.Equal(t, runtime.StreamError, events[len(events)-1].Type, "a broken transport cannot become done(0)")
		})
	}
}

func TestDockerExecStreamDeliversOutputBeforeExit(t *testing.T) {
	rt, base := newFakeDockerRuntime(t)
	base.containers["runtime-a"] = &fakeContainer{config: &container.Config{Labels: map[string]string{"sandbox.managed": "true", "sandbox.role": "fuse-runtime"}}, running: true}
	var framed bytes.Buffer
	_, err := framed.Write([]byte{1, 0, 0, 0, 0, 0, 0, 5, 'h', 'e', 'l', 'l', 'o'})
	require.NoError(t, err)
	rt.cli = &execStreamDockerAPI{fakeDockerAPI: base, output: framed.Bytes()}
	stream, err := rt.ExecStream(context.Background(), "runtime-a", runtime.ExecRequest{Command: "printf hello"})
	require.NoError(t, err)
	first := <-stream
	require.Equal(t, runtime.StreamEvent{Type: runtime.StreamStdout, Content: "hello"}, first)
	last := <-stream
	require.Equal(t, runtime.StreamEvent{Type: runtime.StreamDone, Content: "0"}, last)
}

func TestDockerExecStreamSendsDeclaredStdin(t *testing.T) {
	rt, base := newFakeDockerRuntime(t)
	base.containers["runtime-a"] = &fakeContainer{config: &container.Config{Labels: map[string]string{"sandbox.managed": "true", "sandbox.role": "fuse-runtime"}}, running: true}
	fake := &execStreamDockerAPI{fakeDockerAPI: base}
	rt.cli = fake
	stream, err := rt.ExecStream(context.Background(), "runtime-a", runtime.ExecRequest{Command: "cat", Stdin: "input-data"})
	require.NoError(t, err)
	var events []runtime.StreamEvent
	for event := range stream {
		events = append(events, event)
	}
	require.Equal(t, []byte("input-data"), fake.input)
	require.Equal(t, []runtime.StreamEvent{{Type: runtime.StreamDone, Content: "0"}}, events)
}
