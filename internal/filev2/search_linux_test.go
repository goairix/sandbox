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

func TestFilesV2Glob(t *testing.T) {
	if !matchGlob("**/*.go", "nested/b.go") || !matchGlob("**/*.go", "a.go") || matchGlob("**/*.go", "nested/c.txt") {
		t.Fatal("recursive glob matcher is missing or incorrect")
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.go", "nested/b.go", "nested/c.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.go"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.go"), filepath.Join(root, "link.go")); err != nil {
		t.Fatal(err)
	}
	reserved := fuseprotocol.ProbeObjectBasenamePrefix + strings.Repeat("a", 64)
	if err := os.WriteFile(filepath.Join(root, reserved), nil, 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"glob", root, "**/*.go"}, nil, &output); err != nil {
		t.Fatal(err)
	}
	var page struct {
		Files     []fileInfo `json:"files"`
		Truncated bool       `json:"truncated"`
	}
	if err := json.Unmarshal(output.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Truncated || len(page.Files) != 2 || page.Files[0].Path != filepath.Join(root, "a.go") || page.Files[1].Path != filepath.Join(root, "nested/b.go") {
		t.Fatalf("unsafe or incomplete glob result: %+v", page)
	}
	output.Reset()
	if err := Run(context.Background(), []string{"glob", root, "nested/[bc].*"}, nil, &output); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &page); err != nil || len(page.Files) != 2 {
		t.Fatalf("character class result: %+v, %v", page, err)
	}
	output.Reset()
	if err := Run(context.Background(), []string{"glob", root, "*", "hide-reserved"}, nil, &output); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	for _, info := range page.Files {
		if info.Name == reserved {
			t.Fatal("returned reserved FUSE object")
		}
	}
	for _, pattern := range []string{"", "[", "../*.go", "/tmp/*.go"} {
		output.Reset()
		if err := Run(context.Background(), []string{"glob", root, pattern}, nil, &output); err == nil || output.Len() != 0 {
			t.Fatalf("accepted invalid pattern %q: %q, %v", pattern, output.String(), err)
		}
	}
}

func TestFilesV2GlobTruncatesInsteadOfClaimingComplete(t *testing.T) {
	root := t.TempDir()
	for i := 0; i <= 1000; i++ {
		if err := os.WriteFile(filepath.Join(root, "file"+strconv.Itoa(i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"glob", root, "file*"}, nil, &output); err != nil {
		t.Fatal(err)
	}
	var page struct {
		Files     []fileInfo `json:"files"`
		Truncated bool       `json:"truncated"`
	}
	if err := json.Unmarshal(output.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if !page.Truncated || len(page.Files) != 1000 {
		t.Fatalf("failed to mark partial search: %+v", page)
	}
	for _, info := range page.Files {
		if !strings.HasPrefix(info.Name, "file") {
			t.Fatalf("unexpected file: %+v", info)
		}
	}
}
