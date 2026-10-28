package einofiles

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	sandbox "github.com/goairix/sandbox/sdk/go"
)

type diffFiles struct {
	*recordingFiles
	content      string
	edits        int
	postReadFail bool
	postReadText string
}

func (f *diffFiles) Read(_ context.Context, _ string) (io.ReadCloser, error) {
	if f.postReadFail && f.edits > 0 {
		return nil, errors.New("post-edit read unavailable")
	}
	if f.postReadText != "" && f.edits > 0 {
		return io.NopCloser(strings.NewReader(f.postReadText)), nil
	}
	return io.NopCloser(strings.NewReader(f.content)), nil
}

func (f *diffFiles) Edit(_ context.Context, _ string, oldText, newText string, replaceAll bool) error {
	matches := strings.Count(f.content, oldText)
	if matches == 0 || (!replaceAll && matches != 1) {
		return errors.New("file edit conflict")
	}
	if replaceAll {
		f.content = strings.ReplaceAll(f.content, oldText, newText)
	} else {
		f.content = strings.Replace(f.content, oldText, newText, 1)
	}
	f.edits++
	return nil
}

func editToolFromMiddleware(t *testing.T, middleware adk.ChatModelAgentMiddleware) tool.InvokableTool {
	t.Helper()
	ctx := context.Background()
	_, run, err := middleware.BeforeAgent(ctx, &adk.ChatModelAgentContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Tools) != 6 {
		t.Fatalf("tool count = %d, want 6", len(run.Tools))
	}
	for _, candidate := range run.Tools {
		info, err := candidate.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.Name == "edit_file" {
			invokable, ok := candidate.(tool.InvokableTool)
			if !ok {
				t.Fatal("edit_file is not invokable")
			}
			return invokable
		}
	}
	t.Fatal("edit_file tool missing")
	return nil
}

func TestEinoMiddlewareEditDiff(t *testing.T) {
	files := &diffFiles{recordingFiles: &recordingFiles{}, content: "alpha\nworld\n"}
	middleware, err := NewDiffMiddleware(context.Background(), files)
	if err != nil {
		t.Fatal(err)
	}
	edit := editToolFromMiddleware(t, middleware)
	result, err := edit.InvokableRun(context.Background(), `{"file_path":"/a.txt","old_string":"world","new_string":"etcd"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"--- /a.txt", "+++ /a.txt", "-world", "+etcd"} {
		if !strings.Contains(result, expected) {
			t.Fatalf("diff %q lacks %q", result, expected)
		}
	}
	if files.edits != 1 || files.content != "alpha\netcd\n" {
		t.Fatalf("edit count=%d content=%q", files.edits, files.content)
	}
}

func TestEinoMiddlewareUsesSDKFilesV2(t *testing.T) {
	content := "alpha\nworld\n"
	reads, edits := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/sandboxes":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"sandbox-test","reused":false}`)
		case "/api/v2/sandboxes/sandbox-test/files/content":
			if r.Method != http.MethodGet || r.URL.Query().Get("path") != "/workspace/a.txt" {
				t.Errorf("unexpected read %s %s", r.Method, r.URL)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			reads++
			_, _ = io.WriteString(w, content)
		case "/api/v2/sandboxes/sandbox-test/files/text/edit":
			var input struct {
				Path string `json:"path"`
				Old  string `json:"old_string"`
				New  string `json:"new_string"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if input.Path != "/workspace/a.txt" || input.Old != "world" || input.New != "etcd" {
				t.Errorf("unexpected edit %+v", input)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			edits++
			content = strings.Replace(content, input.Old, input.New, 1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	client := sandbox.NewClient(server.URL, "test-key")
	running, err := client.NewSandbox(ctx, sandbox.SandboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := NewDiffMiddleware(ctx, running.Files())
	if err != nil {
		t.Fatal(err)
	}
	result, err := editToolFromMiddleware(t, middleware).InvokableRun(ctx, `{"file_path":"/a.txt","old_string":"world","new_string":"etcd"}`)
	if err != nil || !strings.Contains(result, "+etcd") || reads != 2 || edits != 1 {
		t.Fatalf("result=%q err=%v reads=%d edits=%d", result, err, reads, edits)
	}
}

func TestEinoDefaultMiddlewarePreservesDefaultEditTool(t *testing.T) {
	files := &diffFiles{recordingFiles: &recordingFiles{}, content: "world\n"}
	middleware, err := NewMiddleware(context.Background(), files)
	if err != nil {
		t.Fatal(err)
	}
	result, err := editToolFromMiddleware(t, middleware).InvokableRun(context.Background(), `{"file_path":"/a.txt","old_string":"world","new_string":"etcd"}`)
	if err != nil || !strings.Contains(result, "Successfully replaced") || files.edits != 1 {
		t.Fatalf("default result=%q err=%v edits=%d", result, err, files.edits)
	}
}

func TestEinoMiddlewareEditDiffFailureBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name          string
		content       string
		postReadFail  bool
		postReadText  string
		wantError     bool
		wantEditCount int
		wantText      string
	}{
		{name: "conflict does not write", content: "other\n", wantError: true, wantText: "other\n"},
		{name: "oversized snapshot does not write", content: strings.Repeat("x", maxDiffSnapshotBytes+1), wantError: true},
		{name: "post-read failure reports applied edit", content: "world\n", postReadFail: true, wantEditCount: 1, wantText: "etcd\n"},
		{name: "concurrent change does not claim diff", content: "world\n", postReadText: "someone else\n", wantEditCount: 1, wantText: "etcd\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := &diffFiles{recordingFiles: &recordingFiles{}, content: tc.content, postReadFail: tc.postReadFail, postReadText: tc.postReadText}
			middleware, err := NewDiffMiddleware(context.Background(), files)
			if err != nil {
				t.Fatal(err)
			}
			result, err := editToolFromMiddleware(t, middleware).InvokableRun(context.Background(), `{"file_path":"/a.txt","old_string":"world","new_string":"etcd"}`)
			if (err != nil) != tc.wantError {
				t.Fatalf("result=%q error=%v, wantError=%v", result, err, tc.wantError)
			}
			if files.edits != tc.wantEditCount {
				t.Fatalf("edit count=%d, want=%d", files.edits, tc.wantEditCount)
			}
			if tc.wantText != "" && files.content != tc.wantText {
				t.Fatalf("content=%q, want=%q", files.content, tc.wantText)
			}
			if tc.postReadFail && !strings.Contains(result, "applied") {
				t.Fatalf("successful edit not identified: %q", result)
			}
			if tc.postReadText != "" && strings.Contains(result, "@@") {
				t.Fatalf("concurrent result claimed a diff: %q", result)
			}
		})
	}
}
