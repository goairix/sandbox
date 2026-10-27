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
