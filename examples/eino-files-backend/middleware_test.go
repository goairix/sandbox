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
	return namedToolFromMiddleware(t, middleware, "edit_file")
}

func namedToolFromMiddleware(t *testing.T, middleware adk.ChatModelAgentMiddleware, name string) tool.InvokableTool {
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
		if info.Name == name {
			invokable, ok := candidate.(tool.InvokableTool)
			if !ok {
				t.Fatalf("%s is not invokable", name)
			}
			return invokable
		}
	}
	t.Fatalf("%s tool missing", name)
	return nil
}

func TestEinoLsAndGrepShowNativeStyleResults(t *testing.T) {
	for _, makeMiddleware := range []struct {
		name string
		new  func(context.Context, *diffFiles) (adk.ChatModelAgentMiddleware, error)
	}{
		{"standard", func(ctx context.Context, files *diffFiles) (adk.ChatModelAgentMiddleware, error) {
			return NewMiddleware(ctx, files)
		}},
		{"diff", func(ctx context.Context, files *diffFiles) (adk.ChatModelAgentMiddleware, error) {
			return NewDiffMiddleware(ctx, files)
		}},
	} {
		t.Run(makeMiddleware.name, func(t *testing.T) {
			files := &diffFiles{recordingFiles: &recordingFiles{}}
			middleware, err := makeMiddleware.new(context.Background(), files)
			if err != nil {
				t.Fatal(err)
			}

			ls, err := namedToolFromMiddleware(t, middleware, "ls").InvokableRun(context.Background(), `{"path":"/"}`)
			if err != nil || !strings.Contains(ls, "-????????? ? ? ? 3 Jan  1 00:00 2026 /a.txt") || !strings.Contains(ls, "d????????? ? ? ? 0 - /dir/") {
				t.Fatalf("ls=%q err=%v", ls, err)
			}

			grep, err := namedToolFromMiddleware(t, middleware, "grep").InvokableRun(context.Background(), `{"path":"/","pattern":"match"}`)
			if err != nil || grep != "/a.txt:2:match" {
				t.Fatalf("grep=%q err=%v", grep, err)
			}
		})
	}
}

func TestEinoGrepModesAndContext(t *testing.T) {
	files := &diffFiles{recordingFiles: &recordingFiles{grepMatches: []sandbox.FileV2Match{
		{Path: "/workspace/z.go", LineNumber: 5, Content: "match\n", Before: []string{"before\n"}, After: []string{"after\n"}},
		{Path: "/workspace/a.go", LineNumber: 2, Content: "match\n"},
	}}}
	middleware, err := NewMiddleware(context.Background(), files)
	if err != nil {
		t.Fatal(err)
	}
	grep := namedToolFromMiddleware(t, middleware, "grep")
	ctx := context.Background()

	content, err := grep.InvokableRun(ctx, `{"path":"/","pattern":"match","output_mode":"content","-C":1}`)
	if err != nil || content != "/a.go:2:match\n/z.go-4-before\n/z.go:5:match\n/z.go-6-after" {
		t.Fatalf("content=%q err=%v", content, err)
	}
	if files.grepRequest.BeforeContext != 1 || files.grepRequest.AfterContext != 1 {
		t.Fatalf("context was not forwarded: %+v", files.grepRequest)
	}

	paths, err := grep.InvokableRun(ctx, `{"pattern":"match","output_mode":"files_with_matches"}`)
	if err != nil || paths != "/a.go\n/z.go" {
		t.Fatalf("paths=%q err=%v", paths, err)
	}
	counts, err := grep.InvokableRun(ctx, `{"pattern":"match","output_mode":"count"}`)
	if err != nil || counts != "/a.go:1\n/z.go:1" {
		t.Fatalf("counts=%q err=%v", counts, err)
	}
	page, err := grep.InvokableRun(ctx, `{"pattern":"match","offset":1,"head_limit":1,"-n":false}`)
	if err != nil || page != "/z.go:match" {
		t.Fatalf("page=%q err=%v", page, err)
	}
	if _, err := grep.InvokableRun(ctx, `{"pattern":"match","head_limit":-1}`); err == nil {
		t.Fatal("negative head_limit accepted")
	}
	files.truncated = true
	if _, err := grep.InvokableRun(ctx, `{"pattern":"match"}`); !errors.Is(err, ErrIncompleteSearch) {
		t.Fatalf("truncated grep error=%v", err)
	}
}

func TestLsHumanSize(t *testing.T) {
	for _, tc := range []struct {
		bytes int64
		want  string
	}{
		{0, "0"}, {1023, "1023"}, {1024, "1.0K"},
		{1025, "1.1K"}, {1536, "1.5K"}, {2 << 20, "2.0M"},
	} {
		if got := humanSize(tc.bytes); got != tc.want {
			t.Errorf("humanSize(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
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
		case "/api/v2/sandboxes/sandbox-test/files/list":
			if r.Method != http.MethodGet || r.URL.Query().Get("path") != "/workspace" {
				t.Errorf("unexpected list %s %s", r.Method, r.URL)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"files":[{"path":"/workspace/a.txt","size":11,"mod_time":"2026-10-29T12:00:00Z","mode":"-rw-r--r--","nlink":1,"uid":1000,"gid":1000},{"path":"/workspace/.env","size":1536,"mod_time":"2026-10-29T12:00:00Z","mode":"-rw-------","nlink":1,"uid":1000,"gid":1000}]}`)
		case "/api/v2/sandboxes/sandbox-test/files/grep":
			var input sandbox.FileV2GrepRequest
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Path != "/workspace" || input.Pattern != "etcd" {
				t.Errorf("unexpected grep %+v: %v", input, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"matches":[{"path":"/workspace/a.txt","line_number":2,"content":"etcd\n"}]}`)
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
	ls, err := namedToolFromMiddleware(t, middleware, "ls").InvokableRun(ctx, `{"path":"/"}`)
	if err != nil || ls != "-rw------- 1 1000 1000 1.5K Oct 29 12:00 2026 /.env\n-rw-r--r-- 1 1000 1000   11 Oct 29 12:00 2026 /a.txt" {
		t.Fatalf("SDK ls=%q err=%v", ls, err)
	}
	t.Logf("ls -alh:\n%s", ls)
	grep, err := namedToolFromMiddleware(t, middleware, "grep").InvokableRun(ctx, `{"pattern":"etcd"}`)
	if err != nil || grep != "/a.txt:2:etcd" {
		t.Fatalf("SDK grep=%q err=%v", grep, err)
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
