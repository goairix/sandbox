package sandbox_test

import (
	"errors"
	"testing"

	sandbox "github.com/goairix/sandbox/sdk/go"
)

func TestSandboxErrorIs(t *testing.T) {
	err := &sandbox.SandboxError{StatusCode: 404, Code: "SANDBOX_NOT_FOUND", Message: "not found"}
	if !errors.Is(err, sandbox.ErrNotFound) {
		t.Error("errors.Is should match ErrNotFound by StatusCode+Code")
	}
}

func TestSandboxErrorIsNegative(t *testing.T) {
	err := &sandbox.SandboxError{StatusCode: 500, Code: "INTERNAL", Message: "oops"}
	if errors.Is(err, sandbox.ErrNotFound) {
		t.Error("500 error should not match ErrNotFound")
	}
}

func TestSandboxErrorIsEmptyCodeSentinel(t *testing.T) {
	// ErrUnauthorized has no Code — any 401 should match regardless of Code.
	err := &sandbox.SandboxError{StatusCode: 401, Code: "SOME_OTHER_CODE", Message: "bad key"}
	if !errors.Is(err, sandbox.ErrUnauthorized) {
		t.Error("any 401 error should match ErrUnauthorized (empty-Code sentinel)")
	}
}

func TestSandboxErrorAs(t *testing.T) {
	err := &sandbox.SandboxError{StatusCode: 401, Code: "UNAUTHORIZED", Message: "bad key"}
	var se *sandbox.SandboxError
	if !errors.As(err, &se) {
		t.Fatal("errors.As should unwrap to *SandboxError")
	}
	if se.Message != "bad key" {
		t.Errorf("Message = %q, want %q", se.Message, "bad key")
	}
}

func TestSandboxErrorError(t *testing.T) {
	err := &sandbox.SandboxError{StatusCode: 429, Code: "RATE_LIMITED", Message: "slow down"}
	if err.Error() == "" {
		t.Error("Error() should return non-empty string")
	}
}
