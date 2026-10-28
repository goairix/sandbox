package einofiles

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
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
		{"grep", `{"path":"/","pattern":"etcd"}`, "/eino-demo.txt:2:etcd"},
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
		if step.name == "ls" && (!strings.Contains(result, " 11 ") || !strings.Contains(result, "/eino-demo.txt")) {
			t.Fatalf("ls did not report the observed 11-byte file size: %q", result)
		}
	}

	// Exercise -a and -h against the mounted filesystem, not the fake SDK.
	hiddenInput, err := json.Marshal(map[string]string{
		"file_path": "/.eino-hidden",
		"content":   strings.Repeat("x", 1536),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tools["write_file"].InvokableRun(ctx, string(hiddenInput)); err != nil {
		t.Fatalf("write hidden file: %v", err)
	}
	listing, err := tools["ls"].InvokableRun(ctx, `{"path":"/"}`)
	if err != nil {
		t.Fatalf("list hidden file: %v", err)
	}
	t.Logf("ls -alh with hidden 1536-byte file:\n%s", listing)
	for _, line := range strings.Split(listing, "\n") {
		if !strings.HasSuffix(line, " /.eino-hidden") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 10 || len(fields[0]) != 10 || fields[0][0] != '-' || strings.Contains(fields[0], "?") || fields[4] != "1.5K" {
			t.Fatalf("hidden file lacks long listing or human size: %q", line)
		}
		for _, value := range fields[1:4] {
			if _, err := strconv.ParseUint(value, 10, 32); err != nil {
				t.Fatalf("hidden file lacks numeric links/owner/group: %q", line)
			}
		}
		return
	}
	t.Fatalf("ls omitted hidden file: %q", listing)
}

// TestLiveEinoShell exercises both Eino execute variants in a real FUSE
// sandbox. It is separate from Files v2 so the running API need not include
// unreleased file-list metadata changes.
func TestLiveEinoShell(t *testing.T) {
	baseURL, apiKey := os.Getenv("EINO_FILES_LIVE_BASE_URL"), os.Getenv("EINO_FILES_LIVE_API_KEY")
	if baseURL == "" || apiKey == "" {
		t.Skip("set EINO_FILES_LIVE_BASE_URL and EINO_FILES_LIVE_API_KEY for a live cluster test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	client := sandbox.NewClient(baseURL, apiKey, sandbox.WithTimeout(3*time.Minute))
	running, err := client.NewSandbox(ctx, sandbox.SandboxOptions{
		Mode: sandbox.ModePersistent, Timeout: 600,
		WorkspacePath:      fmt.Sprintf("eino-shell-live/%d", time.Now().UnixNano()),
		WorkspaceMountMode: sandbox.WorkspaceMountFUSE,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("created shell test sandbox %s", running.ID())
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if err := running.Close(cleanupCtx); err != nil {
			t.Errorf("destroy sandbox %s: %v", running.ID(), err)
		}
	}()

	commands := NewCommandBackend(running)
	for _, variant := range []struct {
		name   string
		option MiddlewareOption
	}{
		{"shell", WithShell(commands)},
		{"streaming", WithStreamingShell(commands)},
	} {
		middleware, err := NewMiddleware(ctx, running.Files(), variant.option)
		if err != nil {
			t.Fatal(err)
		}
		_, run, err := middleware.BeforeAgent(ctx, &adk.ChatModelAgentContext{})
		if err != nil {
			t.Fatal(err)
		}
		var execute tool.BaseTool
		for _, candidate := range run.Tools {
			info, err := candidate.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if info.Name == "execute" {
				execute = candidate
				break
			}
		}
		if execute == nil {
			t.Fatal("Eino execute tool missing")
		}
		if variant.name == "shell" {
			result, err := execute.(tool.InvokableTool).InvokableRun(ctx, `{"command":"printf shell-ok; exit 7"}`)
			t.Logf("Eino shell: %s", result)
			if err != nil || !strings.Contains(result, "shell-ok") || !strings.Contains(result, "exit code 7") {
				t.Fatalf("shell result=%q err=%v", result, err)
			}
			continue
		}
		stream, err := execute.(tool.StreamableTool).StreamableRun(ctx, `{"command":"printf stream-out; printf stream-err >&2; exit 4"}`)
		if err != nil {
			t.Fatal(err)
		}
		var chunks strings.Builder
		for {
			chunk, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				stream.Close()
				t.Fatal(err)
			}
			chunks.WriteString(chunk)
		}
		stream.Close()
		result := chunks.String()
		t.Logf("Eino streaming shell: %s", result)
		if !strings.Contains(result, "stream-out") || !strings.Contains(result, "stream-err") || !strings.Contains(result, "exit code 4") {
			t.Fatalf("streaming shell result=%q", result)
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
