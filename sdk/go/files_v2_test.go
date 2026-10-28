package sandbox_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	sandbox "github.com/goairix/sandbox/sdk/go"
)

func TestFilesV2SDK(t *testing.T) {
	var written string
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing v2 authorization")
		}
		if r.URL.Path == "/api/v1/sandboxes" {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "sandbox-a", "reused": false})
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/v2/sandboxes/sandbox-a/files/") {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.URL.Path {
		case "/api/v2/sandboxes/sandbox-a/files/stat":
			if r.URL.Query().Get("path") != "/workspace/a.txt" {
				t.Errorf("stat path: %s", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"name":"a.txt","path":"/workspace/a.txt","size":3,"is_dir":false,"mod_time":"2026-01-01T00:00:00Z","mode":"-rw-r--r--","nlink":1,"uid":1000,"gid":1000}`)
		case "/api/v2/sandboxes/sandbox-a/files/list":
			_, _ = io.WriteString(w, `{"files":[],"next_cursor":""}`)
		case "/api/v2/sandboxes/sandbox-a/files/glob":
			_, _ = io.WriteString(w, `{"files":[],"truncated":true}`)
		case "/api/v2/sandboxes/sandbox-a/files/grep":
			_, _ = io.WriteString(w, `{"matches":[{"path":"/workspace/a.txt","line_number":1,"content":"abc\n"}],"truncated":false}`)
		case "/api/v2/sandboxes/sandbox-a/files/content":
			if r.Method == http.MethodPut {
				if r.ContentLength != 3 || r.URL.Query().Get("create_parents") != "true" {
					t.Errorf("write metadata: length=%d query=%s", r.ContentLength, r.URL.RawQuery)
				}
				body, _ := io.ReadAll(r.Body)
				written = string(body)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_, _ = io.WriteString(w, written)
		case "/api/v2/sandboxes/sandbox-a/files/text/read-lines":
			_, _ = io.WriteString(w, `{"content":"hello\n"}`)
		case "/api/v2/sandboxes/sandbox-a/files/text/edit":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected v2 route %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	handle, err := client.NewSandbox(context.Background(), sandbox.SandboxOptions{Mode: sandbox.ModePersistent})
	if err != nil {
		t.Fatal(err)
	}
	files := handle.Files()
	info, err := files.Stat(context.Background(), "/workspace/a.txt")
	if err != nil || info.Path != "/workspace/a.txt" || info.Size != 3 || info.Mode != "-rw-r--r--" || info.Nlink != 1 || info.UID != 1000 {
		t.Fatalf("stat = %+v, %v", info, err)
	}
	if _, err := files.List(context.Background(), "/workspace", "", 10); err != nil {
		t.Fatal(err)
	}
	page, err := files.Glob(context.Background(), sandbox.FileV2GlobRequest{Pattern: "**/*.go"})
	if err != nil || !page.Truncated {
		t.Fatalf("glob = %+v, %v", page, err)
	}
	grep, err := files.Grep(context.Background(), sandbox.FileV2GrepRequest{Pattern: "abc"})
	if err != nil || len(grep.Matches) != 1 || grep.Matches[0].LineNumber != 1 {
		t.Fatalf("grep = %+v, %v", grep, err)
	}
	if err := files.Write(context.Background(), "/workspace/a.txt", strings.NewReader("abc"), 3, true); err != nil {
		t.Fatal(err)
	}
	stream, err := files.Read(context.Background(), "/workspace/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil || string(data) != "abc" {
		t.Fatalf("read = %q, %v", data, err)
	}
	text, err := files.ReadLines(context.Background(), "/workspace/a.txt", 1, 0)
	if err != nil || text != "hello\n" {
		t.Fatalf("lines = %q, %v", text, err)
	}
	if err := files.Edit(context.Background(), "/workspace/a.txt", "abc", "ABC", false); err != nil {
		t.Fatal(err)
	}
}

func TestFilesV2StreamingTransferOutlivesUnaryHTTPTimeout(t *testing.T) {
	server, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sandboxes":
			_, _ = io.WriteString(w, `{"id":"sandbox-a","reused":false}`)
		case r.Method == http.MethodGet:
			w.Header().Set("Content-Length", "2")
			_, _ = io.WriteString(w, "a")
			w.(http.Flusher).Flush()
			time.Sleep(80 * time.Millisecond)
			_, _ = io.WriteString(w, "b")
		case r.Method == http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != "ab" {
				t.Errorf("upload = %q, %v", body, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	client := sandbox.NewClient(server.URL, "test-key", sandbox.WithTimeout(25*time.Millisecond))
	handle, err := client.NewSandbox(context.Background(), sandbox.SandboxOptions{Mode: sandbox.ModePersistent})
	if err != nil {
		t.Fatal(err)
	}
	files := handle.Files()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := files.Read(ctx, "/workspace/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	content, readErr := io.ReadAll(stream)
	closeErr := stream.Close()
	if readErr != nil || closeErr != nil || string(content) != "ab" {
		t.Fatalf("read = %q, %v, close %v", content, readErr, closeErr)
	}
	reader, writer := io.Pipe()
	go func() {
		_, _ = io.WriteString(writer, "a")
		time.Sleep(80 * time.Millisecond)
		_, _ = io.WriteString(writer, "b")
		_ = writer.Close()
	}()
	defer reader.Close()
	if err := files.Write(ctx, "/workspace/a.txt", reader, 2, false); err != nil {
		t.Fatal(err)
	}
}
