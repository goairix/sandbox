package einofiles

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	sandbox "github.com/goairix/sandbox/sdk/go"
)

// TestLiveEinoFilesystem is opt-in: it creates and destroys one FUSE sandbox.
// Eino tools are invoked directly, so no ChatModel or external agent is needed.
func TestLiveEinoFilesystem(t *testing.T) {
	baseURL, apiKey := os.Getenv("EINO_FILES_LIVE_BASE_URL"), os.Getenv("EINO_FILES_LIVE_API_KEY")
	if baseURL == "" || apiKey == "" {
		t.Skip("set EINO_FILES_LIVE_BASE_URL and EINO_FILES_LIVE_API_KEY for a live cluster test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	client := sandbox.NewClient(baseURL, apiKey, sandbox.WithTimeout(3*time.Minute))
	workspace := fmt.Sprintf("eino-files-live/%d", time.Now().UnixNano())
	running, err := client.NewSandbox(ctx, sandbox.SandboxOptions{
		Mode: sandbox.ModePersistent, Timeout: 600,
		WorkspacePath: workspace, WorkspaceMountMode: sandbox.WorkspaceMountFUSE,
	})
	if err != nil {
		t.Fatalf("create FUSE sandbox: %v", err)
	}
	if running.Reused() {
		t.Fatalf("unexpected reused sandbox %s", running.ID())
	}
	t.Logf("created sandbox %s, workspace %s", running.ID(), workspace)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if err := running.Close(cleanupCtx); err != nil {
			t.Errorf("destroy sandbox %s: %v", running.ID(), err)
		} else {
			t.Logf("destroyed sandbox %s", running.ID())
		}
	}()

	middleware, err := NewDiffMiddleware(ctx, running.Files())
	if err != nil {
		t.Fatal(err)
	}
	tools := liveFilesystemTools(t, ctx, middleware)
	for _, step := range []struct {
		name, input, want string
	}{
		{"write_file", `{"file_path":"/eino-demo.txt","content":"alpha\nworld\n"}`, "Updated file"},
		{"read_file", `{"file_path":"/eino-demo.txt"}`, "world"},
		{"edit_file", `{"file_path":"/eino-demo.txt","old_string":"world","new_string":"etcd"}`, "@@"},
		{"read_file", `{"file_path":"/eino-demo.txt"}`, "etcd"},
		{"ls", `{"path":"/"}`, "/eino-demo.txt"},
		{"glob", `{"path":"/","pattern":"*.txt"}`, "/eino-demo.txt"},
		{"grep", `{"path":"/","pattern":"etcd"}`, "/eino-demo.txt"},
	} {
		result, err := tools[step.name].InvokableRun(ctx, step.input)
		if err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		t.Logf("%s:\n%s", step.name, result)
		if !strings.Contains(result, step.want) {
			t.Fatalf("%s result %q lacks %q", step.name, result, step.want)
		}
		if step.name == "edit_file" && (!strings.Contains(result, "-world") || !strings.Contains(result, "+etcd")) {
			t.Fatalf("edit_file did not return the expected unified diff: %q", result)
		}
	}
}

func liveFilesystemTools(t *testing.T, ctx context.Context, middleware adk.ChatModelAgentMiddleware) map[string]tool.InvokableTool {
	t.Helper()
	_, run, err := middleware.BeforeAgent(ctx, &adk.ChatModelAgentContext{})
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string]tool.InvokableTool, len(run.Tools))
	for _, candidate := range run.Tools {
		info, err := candidate.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		invokable, ok := candidate.(tool.InvokableTool)
		if !ok {
			t.Fatalf("tool %s is not invokable", info.Name)
		}
		result[info.Name] = invokable
	}
	for _, name := range []string{"write_file", "read_file", "edit_file", "ls", "glob", "grep"} {
		if result[name] == nil {
			t.Fatalf("Eino tool %s is not registered", name)
		}
	}
	return result
}
