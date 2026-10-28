package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	einofiles "github.com/goairix/sandbox/examples/eino-files-backend"
	sandbox "github.com/goairix/sandbox/sdk/go"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if err := verify(ctx, os.Stdout, os.Getenv("EINO_FILES_LIVE_BASE_URL"), os.Getenv("EINO_FILES_LIVE_API_KEY")); err != nil {
		fmt.Fprintln(os.Stderr, "verification failed:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "verification passed")
}

// verify calls Eino's registered tools directly. No ChatModel or agent run is
// needed, and every command executes inside the disposable sandbox.
func verify(ctx context.Context, output io.Writer, baseURL, apiKey string) (resultErr error) {
	if baseURL == "" || apiKey == "" {
		return errors.New("set EINO_FILES_LIVE_BASE_URL and EINO_FILES_LIVE_API_KEY")
	}
	client := sandbox.NewClient(baseURL, apiKey, sandbox.WithTimeout(3*time.Minute))
	running, err := client.NewSandbox(ctx, sandbox.SandboxOptions{
		Mode:               sandbox.ModePersistent,
		Timeout:            600,
		WorkspacePath:      fmt.Sprintf("eino-files-cli/%d", time.Now().UnixNano()),
		WorkspaceMountMode: sandbox.WorkspaceMountFUSE,
	})
	if err != nil {
		return fmt.Errorf("create FUSE sandbox: %w", err)
	}
	if running.Reused() {
		return fmt.Errorf("refusing to modify reused sandbox %s", running.ID())
	}
	_, _ = fmt.Fprintf(output, "sandbox: %s\n", running.ID())
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		removed, removeErr := running.Run(cleanupCtx, "bash", "rm -f -- /workspace/verify.txt")
		if removeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove test file: %w", removeErr))
		} else if removed.ExitCode != 0 {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove test file exited %d: %s", removed.ExitCode, removed.Stderr))
		} else {
			_, _ = fmt.Fprintln(output, "removed test file")
		}
		if err := running.Close(cleanupCtx); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close sandbox %s: %w", running.ID(), err))
		} else {
			_, _ = fmt.Fprintf(output, "closed sandbox: %s\n", running.ID())
		}
	}()

	commands := einofiles.NewCommandBackend(running)
	fileMiddleware, err := einofiles.NewDiffMiddleware(ctx, running.Files(), einofiles.WithShell(commands))
	if err != nil {
		return err
	}
	fileTools, err := registeredTools(ctx, fileMiddleware)
	if err != nil {
		return err
	}
	for _, step := range []struct {
		name, request string
		want          []string
	}{
		{"write_file", `{"file_path":"/verify.txt","content":"alpha\nworld\n"}`, []string{"Updated file"}},
		{"read_file", `{"file_path":"/verify.txt"}`, []string{"world"}},
		{"edit_file", `{"file_path":"/verify.txt","old_string":"world","new_string":"etcd"}`, []string{"-world", "+etcd"}},
		{"read_file", `{"file_path":"/verify.txt"}`, []string{"etcd"}},
		{"ls", `{"path":"/"}`, []string{"/verify.txt"}},
		{"glob", `{"path":"/","pattern":"*.txt"}`, []string{"/verify.txt"}},
		{"grep", `{"path":"/","pattern":"etcd"}`, []string{"/verify.txt:2:etcd"}},
		{"execute", `{"command":"cat /workspace/verify.txt; exit 7"}`, []string{"etcd", "exit code 7"}},
	} {
		if err = runTool(ctx, output, fileTools, step.name, step.request, step.want...); err != nil {
			return err
		}
	}

	streamMiddleware, err := einofiles.NewDiffMiddleware(ctx, running.Files(), einofiles.WithStreamingShell(commands))
	if err != nil {
		return err
	}
	streamTools, err := registeredTools(ctx, streamMiddleware)
	if err != nil {
		return err
	}
	execute, ok := streamTools["execute"].(tool.StreamableTool)
	if !ok {
		return errors.New("Eino streaming execute tool is missing")
	}
	stream, err := execute.StreamableRun(ctx, `{"command":"printf stream-out; sleep 1; printf stream-err >&2; exit 4"}`)
	if err != nil {
		return fmt.Errorf("start streaming execute: %w", err)
	}
	defer stream.Close()
	_, _ = fmt.Fprintln(output, "execute (streaming):")
	var observed strings.Builder
	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return fmt.Errorf("streaming execute: %w", recvErr)
		}
		observed.WriteString(chunk)
		fmt.Fprint(output, chunk)
	}
	_, _ = fmt.Fprintln(output)
	for _, want := range []string{"stream-out", "stream-err", "exit code 4"} {
		if !strings.Contains(observed.String(), want) {
			return fmt.Errorf("streaming execute result missing %q", want)
		}
	}
	return nil
}

func registeredTools(ctx context.Context, middleware adk.ChatModelAgentMiddleware) (map[string]tool.BaseTool, error) {
	_, run, err := middleware.BeforeAgent(ctx, &adk.ChatModelAgentContext{})
	if err != nil {
		return nil, err
	}
	result := make(map[string]tool.BaseTool, len(run.Tools))
	for _, candidate := range run.Tools {
		info, err := candidate.Info(ctx)
		if err != nil {
			return nil, err
		}
		result[info.Name] = candidate
	}
	return result, nil
}

func runTool(ctx context.Context, output io.Writer, tools map[string]tool.BaseTool, name, request string, expected ...string) error {
	invoke, ok := tools[name].(tool.InvokableTool)
	if !ok {
		return fmt.Errorf("Eino tool %s is missing", name)
	}
	response, err := invoke.InvokableRun(ctx, request)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	fmt.Fprintf(output, "%s:\n%s\n", name, response)
	for _, want := range expected {
		if !strings.Contains(response, want) {
			return fmt.Errorf("%s result missing %q", name, want)
		}
	}
	return nil
}
