package einofiles

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/cloudwego/eino/adk/filesystem"
	"github.com/cloudwego/eino/schema"
	sandbox "github.com/goairix/sandbox/sdk/go"
)

const maxShellOutputBytes = 1 << 20

// CommandClient is the command surface of one already-created sandbox.
// *sandbox.Sandbox implements it; no command can run on the API host.
type CommandClient interface {
	Run(context.Context, string, string) (sandbox.ExecResponse, error)
	RunStream(context.Context, string, string) (sandbox.EventStream, error)
}

// CommandBackend adapts sandbox execution to both Eino shell interfaces.
// Eino registers only one execute tool at a time; callers select the regular
// or streaming interface using WithShell or WithStreamingShell.
type CommandBackend struct{ commands CommandClient }

var _ filesystem.Shell = (*CommandBackend)(nil)
var _ filesystem.StreamingShell = (*CommandBackend)(nil)

func NewCommandBackend(commands CommandClient) *CommandBackend {
	return &CommandBackend{commands: commands}
}

func (b *CommandBackend) validate(input *filesystem.ExecuteRequest) error {
	if b == nil || b.commands == nil {
		return errors.New("missing sandbox command client")
	}
	if input == nil || strings.TrimSpace(input.Command) == "" {
		return errors.New("missing shell command")
	}
	if input.RunInBackendGround {
		return errors.New("background shell execution is unsupported")
	}
	return nil
}

func (b *CommandBackend) Execute(ctx context.Context, input *filesystem.ExecuteRequest) (*filesystem.ExecuteResponse, error) {
	if err := b.validate(input); err != nil {
		return nil, err
	}
	result, err := b.commands.Run(ctx, "bash", input.Command)
	if err != nil {
		return nil, err
	}
	output := result.Stdout + result.Stderr
	truncated := len(output) > maxShellOutputBytes
	if truncated {
		output = output[:maxShellOutputBytes]
	}
	code := result.ExitCode
	return &filesystem.ExecuteResponse{Output: output, ExitCode: &code, Truncated: truncated}, nil
}

func (b *CommandBackend) ExecuteStreaming(ctx context.Context, input *filesystem.ExecuteRequest) (*schema.StreamReader[*filesystem.ExecuteResponse], error) {
	if err := b.validate(input); err != nil {
		return nil, err
	}
	events, err := b.commands.RunStream(ctx, "bash", input.Command)
	if err != nil {
		return nil, err
	}
	if events == nil {
		return nil, errors.New("sandbox command stream is missing")
	}
	reader, writer := schema.Pipe[*filesystem.ExecuteResponse](8)
	go forwardCommandEvents(ctx, events, writer)
	return reader, nil
}

func forwardCommandEvents(ctx context.Context, events sandbox.EventStream, writer *schema.StreamWriter[*filesystem.ExecuteResponse]) {
	defer writer.Close()
	defer events.Close()
	remaining := maxShellOutputBytes
	truncationReported := false
	for {
		if err := ctx.Err(); err != nil {
			writer.Send(nil, err)
			return
		}
		event, recvErr := events.Recv()
		if recvErr != nil {
			if errors.Is(recvErr, io.EOF) {
				writer.Send(nil, errors.New("sandbox command stream ended without a terminal event"))
			} else {
				writer.Send(nil, recvErr)
			}
			return
		}
		switch event.Type {
		case sandbox.SSEEventStdout, sandbox.SSEEventStderr:
			output := event.Content
			if len(output) > remaining {
				output = output[:remaining]
			}
			remaining -= len(output)
			truncated := len(output) < len(event.Content) && !truncationReported
			if truncated {
				truncationReported = true
			}
			if output != "" || truncated {
				if writer.Send(&filesystem.ExecuteResponse{Output: output, Truncated: truncated}, nil) {
					return
				}
			}
		case sandbox.SSEEventDone:
			code := event.ExitCode
			writer.Send(&filesystem.ExecuteResponse{ExitCode: &code}, nil)
			return
		case sandbox.SSEEventError:
			// Older servers report a nonzero process exit in this exact form.
			// New servers send done(exit_code); other errors remain errors.
			if raw, ok := strings.CutPrefix(event.Content, "command terminated with exit code "); ok {
				if code, err := strconv.Atoi(raw); err == nil && code > 0 && code <= 255 {
					writer.Send(&filesystem.ExecuteResponse{ExitCode: &code}, nil)
					return
				}
			}
			writer.Send(nil, fmt.Errorf("sandbox command: %s", event.Content))
			return
		case sandbox.SSEEventPing:
			// Heartbeats have no command output.
		}
	}
}
