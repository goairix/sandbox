package einofiles

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk/filesystem"
	sandbox "github.com/goairix/sandbox/sdk/go"
)

type recordingFiles struct {
	paths       []string
	readStart   int
	readCount   int
	written     string
	createDirs  bool
	grepRequest sandbox.FileV2GrepRequest
	grepMatches []sandbox.FileV2Match
	globRequest sandbox.FileV2GlobRequest
	truncated   bool
}

func (f *recordingFiles) Stat(context.Context, string) (sandbox.FileInfo, error) {
	return sandbox.FileInfo{}, nil
}

func (f *recordingFiles) List(_ context.Context, path, cursor string, limit int) (sandbox.FileV2ListPage, error) {
	f.paths = append(f.paths, path+":"+cursor)
	if limit != 1000 {
		return sandbox.FileV2ListPage{}, errors.New("wrong page size")
	}
	if cursor == "" {
		return sandbox.FileV2ListPage{Files: []sandbox.FileInfo{{Path: "/workspace/a.txt", Size: 3, ModTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}, NextCursor: "page-2"}, nil
	}
	return sandbox.FileV2ListPage{Files: []sandbox.FileInfo{{Path: "/workspace/dir", IsDir: true}}}, nil
}

func (f *recordingFiles) Glob(_ context.Context, req sandbox.FileV2GlobRequest) (sandbox.FileV2SearchPage, error) {
	f.globRequest = req
	return sandbox.FileV2SearchPage{Files: []sandbox.FileInfo{{Path: "/workspace/dir/b.go", Size: 4}}, Truncated: f.truncated}, nil
}

func (f *recordingFiles) Grep(_ context.Context, req sandbox.FileV2GrepRequest) (sandbox.FileV2GrepResult, error) {
	f.grepRequest = req
	matches := f.grepMatches
	if matches == nil {
		matches = []sandbox.FileV2Match{{Path: "/workspace/a.txt", LineNumber: 2, Content: "match\n"}}
	}
	matches = append([]sandbox.FileV2Match(nil), matches...)
	for i := range matches {
		if req.BeforeContext == 0 {
			matches[i].Before = nil
		}
		if req.AfterContext == 0 {
			matches[i].After = nil
		}
	}
	return sandbox.FileV2GrepResult{Matches: matches, Truncated: f.truncated}, nil
}

func (f *recordingFiles) Read(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("bytes")), nil
}

func (f *recordingFiles) Write(_ context.Context, path string, source io.Reader, size int64, createParents bool) error {
	f.paths = append(f.paths, path)
	f.createDirs = createParents
	value, err := io.ReadAll(source)
	f.written = string(value)
	if int64(len(value)) != size {
		return errors.New("wrong declared length")
	}
	return err
}

func (f *recordingFiles) ReadLines(_ context.Context, path string, start, count int) (string, error) {
	f.paths = append(f.paths, path)
	f.readStart, f.readCount = start, count
	return "line two\n", nil
}

func (f *recordingFiles) Edit(_ context.Context, path, oldText, newText string, replaceAll bool) error {
	f.paths = append(f.paths, path)
	if oldText != "one" || newText != "two" || replaceAll {
		return errors.New("wrong edit semantics")
	}
	return nil
}

func TestBackendContract(t *testing.T) {
	ctx := context.Background()
	files := &recordingFiles{}
	backend := NewBackend(files)
	var _ filesystem.Backend = backend

	listed, err := backend.LsInfo(ctx, &filesystem.LsInfoRequest{Path: "/"})
	if err != nil || len(listed) != 2 || listed[0].Path != "/a.txt" || listed[1].Path != "/dir" || !listed[1].IsDir ||
		len(files.paths) != 2 || files.paths[0] != "/workspace:" || files.paths[1] != "/workspace:page-2" {
		t.Fatalf("paginated list = %+v, paths=%v, err=%v", listed, files.paths, err)
	}
	read, err := backend.Read(ctx, &filesystem.ReadRequest{FilePath: "/a.txt", Offset: 2, Limit: 1})
	if err != nil || read.Content != "line two\n" || files.readStart != 2 || files.readCount != 1 {
		t.Fatalf("line read = %+v, start=%d count=%d err=%v", read, files.readStart, files.readCount, err)
	}
	if err := backend.Write(ctx, &filesystem.WriteRequest{FilePath: "/dir/new.txt", Content: "hello"}); err != nil || files.written != "hello" || !files.createDirs {
		t.Fatalf("write = %q, parents=%v, err=%v", files.written, files.createDirs, err)
	}
	if err := backend.Edit(ctx, &filesystem.EditRequest{FilePath: "/a.txt", OldString: "one", NewString: "two"}); err != nil {
		t.Fatal(err)
	}
	if got := files.paths[len(files.paths)-1]; got != "/workspace/a.txt" {
		t.Fatalf("edit path = %q", got)
	}
	glob, err := backend.GlobInfo(ctx, &filesystem.GlobInfoRequest{Path: "/dir", Pattern: "**/*.go"})
	if err != nil || len(glob) != 1 || glob[0].Path != "/dir/b.go" || files.globRequest.Path != "/workspace/dir" {
		t.Fatalf("glob = %+v, request=%+v, err=%v", glob, files.globRequest, err)
	}
	grep, err := backend.GrepRaw(ctx, &filesystem.GrepRequest{Path: "/", Pattern: "match", FileType: "go", EnableMultiline: true, BeforeLines: 1})
	if err != nil || len(grep) != 1 || grep[0].Path != "/a.txt" || grep[0].Line != 2 || files.grepRequest.FileTypes[0] != "go" || !files.grepRequest.Multiline {
		t.Fatalf("grep = %+v, request=%+v, err=%v", grep, files.grepRequest, err)
	}
	files.truncated = true
	if _, err := backend.GlobInfo(ctx, &filesystem.GlobInfoRequest{Pattern: "**"}); err == nil {
		t.Fatal("truncated glob was treated as complete")
	}
	if _, err := backend.GrepRaw(ctx, &filesystem.GrepRequest{Pattern: "match"}); err == nil {
		t.Fatal("truncated grep was treated as complete")
	}
	if _, err := backend.Read(ctx, &filesystem.ReadRequest{FilePath: "/../etc/passwd"}); err == nil {
		t.Fatal("parent traversal accepted")
	}
}
