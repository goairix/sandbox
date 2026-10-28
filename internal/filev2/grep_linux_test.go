//go:build linux

package filev2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	"golang.org/x/sys/unix"
)

func TestFilesV2Grep(t *testing.T) {
	binary := os.Getenv("RG_TEST_BINARY")
	if binary == "" {
		t.Skip("set RG_TEST_BINARY to the pinned ripgrep 14.1.1 ELF")
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "a.go"), []byte("before\nHello world\nafter\nfoo\nbar\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("hello second\n"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.go"), []byte("hello secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	reserved := fuseprotocol.ProbeObjectBasenamePrefix + strings.Repeat("a", 64)
	if err := os.WriteFile(filepath.Join(root, reserved), []byte("hello reserved\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, openedRoot, relative, absolute, err := parseRequest([]string{"grep", root})
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(openedRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	var output bytes.Buffer
	request := grepRequest{Pattern: "hello", Glob: "**/*.go", IgnoreCase: true, BeforeContext: 1, AfterContext: 1, Limit: 1000}
	if err := runGrepWithBinary(context.Background(), fd, relative, absolute, request, false, binary, &output); err != nil {
		t.Fatal(err)
	}
	var result fileGrepResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Truncated || len(result.Matches) != 1 || result.Matches[0].Path != filepath.Join(root, "nested", "a.go") ||
		result.Matches[0].LineNumber != 2 || result.Matches[0].Content != "Hello world\n" ||
		len(result.Matches[0].Before) != 1 || result.Matches[0].Before[0] != "before\n" ||
		len(result.Matches[0].After) != 1 || result.Matches[0].After[0] != "after\n" {
		t.Fatalf("unsafe or incomplete grep result: %+v", result)
	}
	output.Reset()
	request = grepRequest{Pattern: "foo\\nbar", Glob: "**/*.go", Multiline: true, Limit: 1000}
	if err := runGrepWithBinary(context.Background(), fd, relative, absolute, request, false, binary, &output); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || len(result.Matches) != 1 || result.Matches[0].LineNumber != 4 {
		t.Fatalf("multiline result: %+v, %v", result, err)
	}
	output.Reset()
	request = grepRequest{Pattern: "hello", IgnoreCase: true, Limit: 1}
	if err := runGrepWithBinary(context.Background(), fd, relative, absolute, request, true, binary, &output); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || !result.Truncated || len(result.Matches) != 1 || result.Matches[0].Path != filepath.Join(root, "b.txt") {
		t.Fatalf("limited/hidden result: %+v, %v", result, err)
	}
	output.Reset()
	request.Types = []string{"go"}
	request.Limit = 1000
	if err := runGrepWithBinary(context.Background(), fd, relative, absolute, request, true, binary, &output); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.Truncated || len(result.Matches) != 1 || result.Matches[0].Path != filepath.Join(root, "nested", "a.go") {
		t.Fatalf("file type result: %+v, %v", result, err)
	}
	output.Reset()
	request.Pattern = "["
	if err := runGrepWithBinary(context.Background(), fd, relative, absolute, request, false, binary, &output); !errors.Is(err, ErrTextInvalid) || output.Len() != 0 {
		t.Fatalf("invalid regex result: %q, %v", output.String(), err)
	}
}
