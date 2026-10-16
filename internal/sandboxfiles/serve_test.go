package sandboxfiles

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSandboxFilesServeStrictAndBounds(t *testing.T) {
	for _, wire := range []string{`{"operation":"read","path":"/etc/passwd"}`, `{"operation":"read","operation":"list","path":"/workspace"}`, `{"Operation":"read","path":"/workspace"}`, `{"operation":"read","path":null}`, `{"operation":"read","path":"/workspace","host_root":"/"}`, `{"operation":"read","path":"/workspace"} {}`} {
		var out bytes.Buffer
		if err := Serve(strings.NewReader(wire), &out); err != nil {
			t.Fatal(err)
		}
		result, err := DecodeResult(out.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		fileFailure(t, result, "invalid_request")
	}
	var out bytes.Buffer
	if err := Serve(strings.NewReader(strings.Repeat(" ", MaxInputBytes+1)), &out); err != nil {
		t.Fatal(err)
	}
	result, err := DecodeResult(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	fileFailure(t, result, "limit_exceeded")
	// A legal file's line JSON can exceed stdout because control bytes escape.
	out.Reset()
	err = serve(strings.NewReader(`{"operation":"read-lines","path":"/workspace/a"}`), &out, func(Request) Result {
		return Result{Lines: []string{strings.Repeat("\x00", MaxContentBytes)}, StartLine: 1, EndLine: 1, TotalLines: 1}
	})
	if err != nil || out.Len() > MaxOutputBytes {
		t.Fatalf("unbounded encoded result: %d %v", out.Len(), err)
	}
	result, err = DecodeResult(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	fileFailure(t, result, "limit_exceeded")
	for _, wire := range []string{`{"error":{"code":"not_found"},"content":"YQ=="}`, `{"error":{"code":"made_up"}}`, `{"Content":"YQ=="}`, `{"content":"YQ==","content":"Yg=="}`, `{} {}`, `{"error":null}`, `{"content":"?"}`} {
		if _, err := DecodeResult([]byte(wire)); err == nil {
			t.Errorf("accepted invalid result %s", wire)
		}
	}
	wire, err := json.Marshal(Result{Content: []byte{0, 255, 128}})
	if err != nil {
		t.Fatal(err)
	}
	result, err = DecodeResult(wire)
	if err != nil || !bytes.Equal(result.Content, []byte{0, 255, 128}) {
		t.Fatal("result binary codec", err)
	}
	if err = Serve(strings.NewReader(`{}`), fileFailWriter{}); err == nil {
		t.Fatal("writer failure swallowed")
	}
}

type fileFailWriter struct{}

func (fileFailWriter) Write([]byte) (int, error) { return 0, errors.New("private-writer-error") }

func TestSandboxFilesSanitizedErrors(t *testing.T) {
	for code, want := range map[string]int{"invalid_request": 400, "not_found": 404, "conflict": 409, "limit_exceeded": 413, "unsupported_type": 422, "permission_denied": 422, "io_error": 500, "publish_uncertain": 500} {
		status, message := ErrorStatus(code)
		if status != want || message == "" || strings.Contains(message, "/workspace") {
			t.Fatalf("bad mapping %s=%d %q", code, status, message)
		}
		if code == "publish_uncertain" && !strings.Contains(message, "do not automatically retry") {
			t.Fatal("publication uncertainty lacks no-retry instruction")
		}
	}
}
