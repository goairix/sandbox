package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/sandboxfiles"
)

func TestFileOperationBoundedApplicationError(t *testing.T) {
	var output bytes.Buffer
	if err := fileOperation(strings.NewReader(`{"operation":"read","path":"/etc/passwd"}`), &output); err != nil {
		t.Fatal(err)
	}
	result, err := sandboxfiles.DecodeResult(output.Bytes())
	if err != nil || result.Error == nil || result.Error.Code != "invalid_request" {
		t.Fatalf("fixed helper did not emit sanitized result: %s %v", output.String(), err)
	}
	if err := run([]string{"file-operation", "/arbitrary/root"}); err == nil {
		t.Fatal("helper accepted extra authority arguments")
	}
}
