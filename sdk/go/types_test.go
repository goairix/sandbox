package sandbox_test

import (
	"testing"

	sandbox "github.com/goairix/sandbox/sdk/go"
)

func TestModeConstants(t *testing.T) {
	if sandbox.ModeEphemeral != "ephemeral" {
		t.Errorf("ModeEphemeral = %q, want %q", sandbox.ModeEphemeral, "ephemeral")
	}
	if sandbox.ModePersistent != "persistent" {
		t.Errorf("ModePersistent = %q, want %q", sandbox.ModePersistent, "persistent")
	}
	// compile-time type identity check
	var _ sandbox.Mode = sandbox.ModeEphemeral
	var _ sandbox.Mode = sandbox.ModePersistent
}

func TestCreateSandboxRequestDefaults(t *testing.T) {
	req := sandbox.CreateSandboxRequest{}
	if req.Mode != "" {
		t.Errorf("zero Mode should be empty string, got %q", req.Mode)
	}
}
