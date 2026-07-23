package sandbox_test

import (
	"encoding/json"
	"testing"

	sandbox "github.com/goairix/sandbox/sdk/go"
)

func TestResourceLimitsMarshalTmpDisk(t *testing.T) {
	data, err := json.Marshal(sandbox.ResourceLimits{TmpDisk: "200Mi"})
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	if got, want := string(data), `{"tmp_disk":"200Mi"}`; got != want {
		t.Fatalf("JSON = %s, want %s", got, want)
	}
}

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
