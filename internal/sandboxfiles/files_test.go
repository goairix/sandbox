package sandboxfiles

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func filesFixture(t *testing.T) (string, *os.Root) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return dir, root
}

func fileWrite(t *testing.T, dir, name string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, mode); err != nil {
		t.Fatal(err)
	}
}

func fileOK(t *testing.T, root *os.Root, req Request) Result {
	t.Helper()
	result := runAt(req, root)
	if result.Error != nil {
		t.Fatalf("%s %s failed: %+v", req.Operation, req.Path, result.Error)
	}
	return result
}

func fileFailure(t *testing.T, result Result, code string) {
	t.Helper()
	if result.Error == nil || result.Error.Code != code {
		t.Fatalf("want %s, got %+v", code, result)
	}
}

func TestSandboxFilesValidation(t *testing.T) {
	for _, req := range []Request{
		{Operation: "read", Path: "/etc/passwd"}, {Operation: "read", Path: "/workspace/../tmp/a"},
		{Operation: "read", Path: "/workspace/a\x00"}, {Operation: "read", Path: "workspace/a"},
		{Operation: "read", Path: "/workspace/" + strings.Repeat("a", 4096)},
		{Operation: "unknown", Path: "/workspace/a"}, {Operation: "glob", Path: "/workspace", Pattern: "["},
		{Operation: "glob", Path: "/workspace", Pattern: "../*"}, {Operation: "glob", Path: "/workspace", Pattern: "/*"},
		{Operation: "list-recursive", Path: "/workspace", Page: -1}, {Operation: "list-recursive", Path: "/workspace", PageSize: 1001},
		{Operation: "list-recursive", Path: "/workspace", MaxDepth: 65}, {Operation: "list-recursive", Path: "/workspace", MaxDepth: -1},
		{Operation: "read-lines", Path: "/workspace/a", StartLine: -1}, {Operation: "read-lines", Path: "/workspace/a", StartLine: 3, EndLine: 2},
		{Operation: "edit", Path: "/workspace/a"}, {Operation: "edit-lines", Path: "/workspace/a", StartLine: 0},
	} {
		if err := Validate(req); err == nil {
			t.Errorf("accepted invalid request: %+v", req)
		}
	}
	for _, name := range []string{"/workspace/a..b", "/tmp/a", "/workspace/./a", "/workspace"} {
		if err := Validate(Request{Operation: "read", Path: name}); err != nil {
			t.Errorf("rejected valid path %q: %v", name, err)
		}
	}
}

func TestSandboxFilesReadBinaryLinksAndSpecialTypes(t *testing.T) {
	dir, root := filesFixture(t)
	data := []byte{0, 255, '\n', 128, 'x'}
	fileWrite(t, dir, "a..b", data, 0640)
	for _, op := range []string{"read", "download"} {
		if got := fileOK(t, root, Request{Operation: op, Path: "/workspace/a..b"}).Content; !bytes.Equal(got, data) {
			t.Fatalf("binary %s corrupted: %v", op, got)
		}
	}
	fileFailure(t, runAt(Request{Operation: "read-lines", Path: "/workspace/a..b"}, root), "unsupported_type")
	if err := os.Symlink("a..b", filepath.Join(dir, "relative")); err != nil {
		t.Fatal(err)
	}
	if got := fileOK(t, root, Request{Operation: "read", Path: "/workspace/relative"}).Content; !bytes.Equal(got, data) {
		t.Fatal("relative symlink failed")
	}
	for _, tc := range []struct{ name, target string }{{"absolute", "/etc/passwd"}, {"escape", "../outside"}} {
		if err := os.Symlink(tc.target, filepath.Join(dir, tc.name)); err != nil {
			t.Fatal(err)
		}
		if got := runAt(Request{Operation: "read", Path: "/workspace/" + tc.name}, root); got.Error == nil {
			t.Fatal("escaped root", tc.name)
		}
	}
	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, req := range []Request{
		{Operation: "read", Path: "/workspace/fifo"}, {Operation: "edit", Path: "/workspace/fifo", OldStr: "x", NewStr: "y"},
		{Operation: "edit", Path: "/workspace/fifo/child", OldStr: "x", NewStr: "y"}, {Operation: "list", Path: "/workspace/fifo"},
		{Operation: "read", Path: "/workspace/fifo/child"},
	} {
		done := make(chan Result, 1)
		go func() { done <- runAt(req, root) }()
		select {
		case got := <-done:
			if got.Error == nil {
				t.Fatalf("special object accepted: %+v", req)
			}
		case <-time.After(time.Second):
			t.Fatalf("FIFO operation blocked before type rejection: %+v", req)
		}
	}
	fileFailure(t, runAt(Request{Operation: "read", Path: "/workspace/missing"}, root), "not_found")
	fileFailure(t, runAt(Request{Operation: "read", Path: "/workspace"}, root), "unsupported_type")
}

func TestSandboxFilesRejectInvalidUTF8Names(t *testing.T) {
	dir, root := filesFixture(t)
	err := os.WriteFile(filepath.Join(dir, string([]byte{'x', 255})), nil, 0600)
	if runtime.GOOS == "darwin" && errors.Is(err, unix.EILSEQ) {
		t.Skip("macOS filesystem rejects invalid UTF-8 names with EILSEQ before helper execution; Linux native fixture covers this case")
	}
	if err != nil {
		t.Fatal(err)
	}
	fileFailure(t, runAt(Request{Operation: "list", Path: "/workspace"}, root), "unsupported_type")
}

func TestSandboxFilesPinnedDirectoryRejectsSymlink(t *testing.T) {
	dir, root := filesFixture(t)
	if err := os.Mkdir(filepath.Join(dir, "real"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	parent, err := root.OpenFile(".", os.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	child, err := openChildDirectory(parent, "real")
	if err != nil {
		t.Fatal(err)
	}
	child.Close()
	if child, err := openChildDirectory(parent, "link"); err == nil {
		child.Close()
		t.Fatal("recursive pinned parent open followed a directory symlink")
	}
}

func TestSandboxFilesContentLimitAndEditPreservation(t *testing.T) {
	dir, root := filesFixture(t)
	data := bytes.Repeat([]byte("x"), MaxContentBytes)
	fileWrite(t, dir, "limit", data, 0640)
	if len(fileOK(t, root, Request{Operation: "read", Path: "/workspace/limit"}).Content) != MaxContentBytes {
		t.Fatal("exact byte limit failed")
	}
	fileFailure(t, runAt(Request{Operation: "edit", Path: "/workspace/limit", OldStr: "x", NewStr: "xx", ReplaceAll: true}, root), "limit_exceeded")
	got, err := os.ReadFile(filepath.Join(dir, "limit"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("failed edit changed target", err)
	}
	fileFailure(t, runAt(Request{Operation: "edit", Path: "/workspace/limit", OldStr: "absent", NewStr: "x"}, root), "conflict")
	fileWrite(t, dir, "large", append(data, 'x'), 0600)
	fileFailure(t, runAt(Request{Operation: "read", Path: "/workspace/large"}, root), "limit_exceeded")
	fileFailure(t, runAt(Request{Operation: "edit", Path: "/workspace/large", OldStr: "x", NewStr: ""}, root), "limit_exceeded")
	fileWrite(t, dir, "edit", []byte("old old\n"), 0640)
	original, err := os.Open(filepath.Join(dir, "edit"))
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	fileOK(t, root, Request{Operation: "edit", Path: "/workspace/edit", OldStr: "old", NewStr: "new"})
	previous := make([]byte, 8)
	n, err := original.Read(previous)
	if err != nil || string(previous[:n]) != "old old\n" {
		t.Fatal("edit overwrote original inode", string(previous[:n]), err)
	}
	got, err = os.ReadFile(filepath.Join(dir, "edit"))
	if err != nil || string(got) != "new old\n" {
		t.Fatal("first replacement", string(got), err)
	}
	fileOK(t, root, Request{Operation: "edit", Path: "/workspace/edit", OldStr: "old", NewStr: "all", ReplaceAll: true})
	info, err := os.Stat(filepath.Join(dir, "edit"))
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatal("permissions changed", info, err)
	}
	if err = os.Symlink("edit", filepath.Join(dir, "edit-link")); err != nil {
		t.Fatal(err)
	}
	fileFailure(t, runAt(Request{Operation: "edit", Path: "/workspace/edit-link", OldStr: "new", NewStr: "bad"}, root), "unsupported_type")
	got, err = os.ReadFile(filepath.Join(dir, "edit"))
	if err != nil || string(got) != "new all\n" {
		t.Fatal("symlink rejection changed target", string(got), err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("temporary files leaked: %v", entries)
	}
}

func TestSandboxFilesListsGlobPaginationAndDepth(t *testing.T) {
	dir, root := filesFixture(t)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	fileWrite(t, dir, "z\n\t.txt", []byte("z"), 0600)
	fileWrite(t, dir, "a.txt", nil, 0600)
	fileWrite(t, dir, "sub/inner.txt", []byte("inner"), 0600)
	if err := os.Symlink("sub", filepath.Join(dir, "link-dir")); err != nil {
		t.Fatal(err)
	}
	direct := fileOK(t, root, Request{Operation: "list", Path: "/workspace"})
	if len(direct.Files) != 4 || direct.Files[0].Name != "a.txt" {
		t.Fatalf("direct deterministic list: %+v", direct)
	}
	rec := fileOK(t, root, Request{Operation: "list-recursive", Path: "/workspace", Page: 2, PageSize: 2})
	if rec.TotalCount != 5 || rec.Page != 2 || rec.PageSize != 2 || len(rec.Files) != 2 || rec.Files[1].Path != "/workspace/sub/inner.txt" {
		t.Fatalf("recursive exact pagination: %+v", rec)
	}
	one := fileOK(t, root, Request{Operation: "list-recursive", Path: "/workspace", MaxDepth: 1})
	if one.TotalCount != 4 {
		t.Fatalf("max_depth=1 includes grandchildren: %+v", one)
	}
	glob := fileOK(t, root, Request{Operation: "glob", Path: "/workspace", Pattern: "sub/*.txt"})
	if glob.TotalCount != 1 || len(glob.Files) != 1 || glob.Files[0].Name != "inner.txt" {
		t.Fatalf("relative glob: %+v", glob)
	}
	empty := fileOK(t, root, Request{Operation: "glob", Path: "/workspace", Pattern: "*.txt", Page: int(^uint(0) >> 1), PageSize: 1000})
	if empty.TotalCount != 2 || len(empty.Files) != 0 {
		t.Fatalf("page overflow: %+v", empty)
	}
	deep := ""
	for i := 0; i < 65; i++ {
		deep = filepath.Join(deep, "d")
		if err := os.Mkdir(filepath.Join(dir, deep), 0700); err != nil {
			t.Fatal(err)
		}
	}
	fileFailure(t, runAt(Request{Operation: "list-recursive", Path: "/workspace"}, root), "limit_exceeded")
}

func TestSandboxFilesEntryLimit(t *testing.T) {
	dir, root := filesFixture(t)
	for i := 0; i < MaxEntries; i++ {
		fileWrite(t, dir, fmt.Sprintf("f%05d", i), nil, 0600)
	}
	got := fileOK(t, root, Request{Operation: "glob", Path: "/workspace", Pattern: "f0000?", PageSize: 1000})
	if got.TotalCount != 10 || len(got.Files) != 10 {
		t.Fatalf("exact10000 scan/count: %+v", got)
	}
	fileWrite(t, dir, "overflow", nil, 0600)
	fileFailure(t, runAt(Request{Operation: "glob", Path: "/workspace", Pattern: "no-match"}, root), "limit_exceeded")
}

func TestSandboxFilesLinesAndAtomicEdits(t *testing.T) {
	dir, root := filesFixture(t)
	fileWrite(t, dir, "lines", []byte("a\n\n c\nlast"), 0640)
	got := fileOK(t, root, Request{Operation: "read-lines", Path: "/workspace/lines"})
	if got.TotalLines != 4 || got.StartLine != 1 || got.EndLine != 4 || strings.Join(got.Lines, "|") != "a|| c|last" {
		t.Fatalf("line accounting: %+v", got)
	}
	got = fileOK(t, root, Request{Operation: "read-lines", Path: "/workspace/lines", StartLine: 9})
	if got.TotalLines != 4 || got.StartLine != 9 || got.EndLine != 4 || len(got.Lines) != 0 {
		t.Fatalf("EOF indexes: %+v", got)
	}
	fileOK(t, root, Request{Operation: "edit-lines", Path: "/workspace/lines", StartLine: 2, EndLine: 3, NewContent: "X"})
	data, err := os.ReadFile(filepath.Join(dir, "lines"))
	if err != nil || string(data) != "a\nXlast" {
		t.Fatal("implicit newline or wrong inclusive range", string(data), err)
	}
	fileOK(t, root, Request{Operation: "edit-lines", Path: "/workspace/lines", StartLine: 3, NewContent: "APPEND"})
	data, err = os.ReadFile(filepath.Join(dir, "lines"))
	if err != nil || string(data) != "a\nXlastAPPEND" {
		t.Fatal("EOF+1 append", string(data), err)
	}
	fileFailure(t, runAt(Request{Operation: "edit-lines", Path: "/workspace/lines", StartLine: 4, NewContent: "bad"}, root), "invalid_request")
	for _, tc := range []struct {
		content string
		count   int
	}{{"", 0}, {"\n", 1}, {"a\n", 1}, {"a\n\n", 2}} {
		fileWrite(t, dir, "count", []byte(tc.content), 0600)
		got = fileOK(t, root, Request{Operation: "read-lines", Path: "/workspace/count"})
		if got.TotalLines != tc.count || len(got.Lines) != tc.count {
			t.Fatalf("%q phantom/missing line: %+v", tc.content, got)
		}
	}
}
