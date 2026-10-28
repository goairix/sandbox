package einofiles

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/filesystem"
	"github.com/cloudwego/eino/components/tool"
	sandbox "github.com/goairix/sandbox/sdk/go"
)

type recordingCommands struct {
	language, code string
	response       sandbox.ExecResponse
	events         []sandbox.SSEEvent
	runErr         error
}

func (r *recordingCommands) Run(_ context.Context, language, code string) (sandbox.ExecResponse, error) {
	r.language, r.code = language, code
	return r.response, r.runErr
}

func (r *recordingCommands) RunStream(_ context.Context, language, code string) (sandbox.EventStream, error) {
	r.language, r.code = language, code
	if r.runErr != nil {
		return nil, r.runErr
	}
	return &recordingEventStream{events: r.events}, nil
}

type recordingEventStream struct {
	events []sandbox.SSEEvent
	index  int
}

func (s *recordingEventStream) Recv() (sandbox.SSEEvent, error) {
	if s.index == len(s.events) {
		return sandbox.SSEEvent{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *recordingEventStream) Close() error { return nil }

func TestShellExecutesInSandbox(t *testing.T) {
	runner := &recordingCommands{response: sandbox.ExecResponse{Stdout: "out\n", Stderr: "err\n", ExitCode: 7}}
	shell := NewCommandBackend(runner)
	result, err := shell.Execute(context.Background(), &filesystem.ExecuteRequest{Command: "pwd; exit 7"})
	if err != nil || result.Output != "out\nerr\n" || result.ExitCode == nil || *result.ExitCode != 7 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if runner.language != "bash" || runner.code != "pwd; exit 7" {
		t.Fatalf("command was not sent to the sandbox: %+v", runner)
	}
	if _, err := shell.Execute(context.Background(), &filesystem.ExecuteRequest{Command: "sleep 1", RunInBackendGround: true}); err == nil {
		t.Fatal("background execution was accepted without a completion contract")
	}
	if _, err := shell.Execute(context.Background(), &filesystem.ExecuteRequest{Command: "  "}); err == nil {
		t.Fatal("empty command was accepted")
	}
}

func TestStreamingShellForwardsChunksAndRequiresTerminalEvent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		events  []sandbox.SSEEvent
		want    []string
		wantErr string
	}{
		{"complete", []sandbox.SSEEvent{{Type: sandbox.SSEEventStdout, Content: "one"}, {Type: sandbox.SSEEventStderr, Content: "two"}, {Type: sandbox.SSEEventDone, ExitCode: 4}}, []string{"one", "two", ""}, ""},
		{"remote error", []sandbox.SSEEvent{{Type: sandbox.SSEEventError, Content: "execution failed"}}, nil, "execution failed"},
		{"missing done", []sandbox.SSEEvent{{Type: sandbox.SSEEventStdout, Content: "partial"}}, []string{"partial"}, "without a terminal event"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingCommands{events: tc.events}
			stream, err := NewCommandBackend(runner).ExecuteStreaming(context.Background(), &filesystem.ExecuteRequest{Command: "echo test"})
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			var chunks []string
			var last *filesystem.ExecuteResponse
			for {
				chunk, recvErr := stream.Recv()
				if errors.Is(recvErr, io.EOF) {
					break
				}
				if recvErr != nil {
					if !strings.Contains(recvErr.Error(), tc.wantErr) || tc.wantErr == "" {
						t.Fatalf("recv error = %v", recvErr)
					}
					break
				}
				chunks = append(chunks, chunk.Output)
				last = chunk
			}
			if len(chunks) != len(tc.want) {
				t.Fatalf("chunks=%q want=%q", chunks, tc.want)
			}
			for i := range chunks {
				if chunks[i] != tc.want[i] {
					t.Fatalf("chunks=%q want=%q", chunks, tc.want)
				}
			}
			if tc.name == "complete" && (last.ExitCode == nil || *last.ExitCode != 4 || runner.language != "bash") {
				t.Fatalf("terminal=%+v runner=%+v", last, runner)
			}
		})
	}
}

func TestStreamingShellRecognizesKubernetesNonzeroExit(t *testing.T) {
	runner := &recordingCommands{events: []sandbox.SSEEvent{
		{Type: sandbox.SSEEventStdout, Content: "partial"},
		{Type: sandbox.SSEEventError, Content: "command terminated with exit code 4"},
	}}
	stream, err := NewCommandBackend(runner).ExecuteStreaming(context.Background(), &filesystem.ExecuteRequest{Command: "exit 4"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	first, err := stream.Recv()
	if err != nil || first.Output != "partial" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	last, err := stream.Recv()
	if err != nil || last.ExitCode == nil || *last.ExitCode != 4 {
		t.Fatalf("terminal=%+v err=%v", last, err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("stream end = %v", err)
	}
}

func TestMiddlewareRegistersChosenExecuteTool(t *testing.T) {
	for _, tc := range []struct {
		name   string
		option MiddlewareOption
		stream bool
	}{
		{"shell", WithShell(NewCommandBackend(&recordingCommands{response: sandbox.ExecResponse{Stdout: "hello\n"}})), false},
		{"stream", WithStreamingShell(NewCommandBackend(&recordingCommands{events: []sandbox.SSEEvent{{Type: sandbox.SSEEventStdout, Content: "hello\n"}, {Type: sandbox.SSEEventDone}}})), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			middleware, err := NewDiffMiddleware(context.Background(), &diffFiles{recordingFiles: &recordingFiles{}}, tc.option)
			if err != nil {
				t.Fatal(err)
			}
			_, run, err := middleware.BeforeAgent(context.Background(), &adk.ChatModelAgentContext{})
			if err != nil || len(run.Tools) != 7 {
				t.Fatalf("tools=%d err=%v", len(run.Tools), err)
			}
			for _, candidate := range run.Tools {
				info, _ := candidate.Info(context.Background())
				if info.Name != "execute" {
					continue
				}
				if tc.stream {
					stream, ok := candidate.(tool.StreamableTool)
					if !ok {
						t.Fatal("execute is not streamable")
					}
					output, err := stream.StreamableRun(context.Background(), `{"command":"echo hello"}`)
					if err != nil {
						t.Fatal(err)
					}
					defer output.Close()
					chunk, err := output.Recv()
					if err != nil || chunk != "hello\n" {
						t.Fatalf("chunk=%q err=%v", chunk, err)
					}
				} else {
					output, err := candidate.(tool.InvokableTool).InvokableRun(context.Background(), `{"command":"echo hello"}`)
					if err != nil || output != "hello\n" {
						t.Fatalf("output=%q err=%v", output, err)
					}
				}
				return
			}
			t.Fatal("execute tool missing")
		})
	}
}
