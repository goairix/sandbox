//go:build linux

package filev2

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

func TestRunFileOperations(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sample.txt")
	if err := Run(context.Background(), []string{"write", path, "5"}, strings.NewReader("hello"), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var content bytes.Buffer
	if err := Run(context.Background(), []string{"read", path}, strings.NewReader(""), &content); err != nil {
		t.Fatal(err)
	}
	if content.String() != "hello" {
		t.Fatalf("read %q", content.String())
	}
	var info struct {
		Path  string `json:"path"`
		Size  int64  `json:"size"`
		IsDir bool   `json:"is_dir"`
	}
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"stat", path}, nil, &output); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Path != path || info.Size != 5 || info.IsDir {
		t.Fatalf("unexpected stat: %+v", info)
	}
	output.Reset()
	if err := Run(context.Background(), []string{"list", root}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(output.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != path {
		t.Fatalf("unexpected listing: %+v", entries)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "hello" {
		t.Fatalf("disk content %q, %v", got, err)
	}
}

func TestRunReadRange(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "range.txt")
	if err := os.WriteFile(filePath, []byte("abcdef"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		start, length string
		want          string
		fail          bool
	}{
		{start: "2", length: "3", want: "cde"},
		{start: "6", length: "0", want: ""},
		{start: "0", length: "-1", want: "abcdef"},
		{start: "7", length: "0", fail: true},
		{start: "5", length: "2", fail: true},
		{start: "-1", length: "1", fail: true},
	} {
		var output bytes.Buffer
		err := Run(context.Background(), []string{"read", filePath, tc.start, tc.length}, nil, &output)
		if tc.fail {
			if err == nil || output.Len() != 0 {
				t.Fatalf("range %s:%s returned %q, %v", tc.start, tc.length, output.String(), err)
			}
			continue
		}
		if err != nil || output.String() != tc.want {
			t.Fatalf("range %s:%s returned %q, %v; want %q", tc.start, tc.length, output.String(), err, tc.want)
		}
	}
}

func TestRunWriteCreateParents(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "one", "two", "file.txt")
	if err := Run(context.Background(), []string{"write", target, "3", "create-parents"}, strings.NewReader("abc"), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "abc" {
		t.Fatalf("written file = %q, %v", content, err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	err = Run(context.Background(), []string{"write", filepath.Join(root, "escape", "bad"), "1", "create-parents"}, strings.NewReader("x"), &bytes.Buffer{})
	if err == nil {
		t.Fatal("followed symlink while creating parents")
	}
	if _, err := os.Stat(filepath.Join(outside, "bad")); !os.IsNotExist(err) {
		t.Fatalf("created file outside root: %v", err)
	}
}

func TestRunTextOperations(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "text.txt")
	if err := os.WriteFile(filePath, []byte("α one\r\nβ two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"read-lines", filePath, "2", "0"}, nil, &output); err != nil {
		t.Fatal(err)
	}
	var readResult struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(output.Bytes(), &readResult); err != nil || readResult.Content != "β two\n" {
		t.Fatalf("read-lines = %+v, %v", readResult, err)
	}
	output.Reset()
	if err := Run(context.Background(), []string{"edit", filePath}, strings.NewReader(`{"old_string":"one","new_string":"ONE","replace_all":false}`), &output); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filePath)
	if err != nil || string(content) != "α ONE\r\nβ two\n" {
		t.Fatalf("edited file = %q, %v", content, err)
	}
	output.Reset()
	if err := Run(context.Background(), []string{"edit", filePath}, strings.NewReader(`{"old_string":"not found","new_string":"x"}`), &output); err == nil {
		t.Fatal("accepted missing edit match")
	}
	if content, err = os.ReadFile(filePath); err != nil || string(content) != "α ONE\r\nβ two\n" {
		t.Fatalf("failed edit changed file = %q, %v", content, err)
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return 1, nil }

func TestRunReadRejectsShortOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bytes")
	if err := os.WriteFile(path, []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), []string{"read", path}, nil, shortWriter{}); err == nil {
		t.Fatal("short writer silently dropped file content")
	}
}

func TestRunListFailsClosedWhenUnpagedDirectoryExceedsLimit(t *testing.T) {
	root := t.TempDir()
	for i := 0; i <= 1000; i++ {
		if err := os.WriteFile(filepath.Join(root, strconv.Itoa(i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"list", root}, strings.NewReader(""), &output); err == nil {
		t.Fatal("unpaged listing exceeded 1000 entries")
	}
	if output.Len() != 0 {
		t.Fatalf("returned partial unpaged listing: %d bytes", output.Len())
	}
}

func TestRunPagedList(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	cursor := ""
	for _, want := range [][]string{{"a", "b"}, {"c", "d"}, {"e"}} {
		var output bytes.Buffer
		if err := Run(context.Background(), []string{"list", root, cursor, "2"}, nil, &output); err != nil {
			t.Fatal(err)
		}
		var page struct {
			Files []struct {
				Name string `json:"name"`
			} `json:"files"`
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(output.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Files) != len(want) {
			t.Fatalf("got %d entries, want %v", len(page.Files), want)
		}
		for i, name := range want {
			if page.Files[i].Name != name {
				t.Fatalf("entry %d = %q, want %q", i, page.Files[i].Name, name)
			}
		}
		cursor = page.NextCursor
	}
	if cursor != "" {
		t.Fatalf("last page has cursor %q", cursor)
	}
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"list", root, "/tmp/other/file", "2"}, nil, &output); err == nil {
		t.Fatal("accepted cursor for another directory")
	}
}

func TestRunPagedListHidesReservedBeforeCursor(t *testing.T) {
	root := t.TempDir()
	reserved := fuseprotocol.ProbeObjectBasenamePrefix + strings.Repeat("a", 64)
	for _, name := range []string{"!.txt", reserved, "a.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"list", root, "", "1", "hide-reserved"}, nil, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), reserved) {
		t.Fatal("reserved basename leaked into page or cursor")
	}
	var page struct {
		Files      []fileInfo `json:"files"`
		NextCursor string     `json:"next_cursor"`
	}
	if err := json.Unmarshal(output.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Files) != 1 || page.Files[0].Name != "!.txt" || page.NextCursor != filepath.Join(root, "!.txt") {
		t.Fatalf("unexpected first page: %+v", page)
	}
	output.Reset()
	if err := Run(context.Background(), []string{"list", root, page.NextCursor, "1", "hide-reserved"}, nil, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), reserved) {
		t.Fatal("reserved basename leaked into following page")
	}
}

func TestRunRejectsInvalidControlAndPath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file")
	for _, args := range [][]string{
		{"not-an-operation", filepath.Join(root, "file")},
		{"stat", "/etc/passwd"},
		{"stat", filepath.Join(root, "../escape")},
		{"write", filepath.Join(root, "file"), "not-a-size"},
		{"read"},
	} {
		if err := Run(context.Background(), args, strings.NewReader(""), &bytes.Buffer{}); err == nil {
			t.Errorf("accepted invalid operation: %q", args)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "file")); !os.IsNotExist(err) {
		t.Fatalf("invalid controls created file: %v", err)
	}
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), []string{"write", path, "0"}, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted missing write body")
	}
	if err := Run(context.Background(), []string{"stat", path}, strings.NewReader("not-json"), &bytes.Buffer{}); err == nil {
		t.Fatal("accepted malformed metadata control")
	}
	for _, body := range []string{"ab", "too long"} {
		if err := Run(context.Background(), []string{"write", path, "3"}, strings.NewReader(body), &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted mismatched write body %q", body)
		}
	}
}
