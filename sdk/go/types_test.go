package sandbox_test

import (
	"encoding/json"
	"strings"
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

func TestWorkspaceMountModeJSON(t *testing.T) {
	raw, err := json.Marshal(sandbox.CreateSandboxRequest{
		Mode: sandbox.ModeEphemeral, WorkspacePath: "jobs/a", WorkspaceMountMode: sandbox.WorkspaceMountFUSE,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) || !containsJSONField(raw, `"workspace_mount_mode":"fuse"`) {
		t.Fatalf("JSON = %s, want workspace_mount_mode=fuse", raw)
	}

	withoutMode, err := json.Marshal(sandbox.CreateSandboxRequest{Mode: sandbox.ModeEphemeral, WorkspacePath: "jobs/a"})
	if err != nil {
		t.Fatal(err)
	}
	if containsJSONField(withoutMode, `"workspace_mount_mode"`) {
		t.Fatalf("zero workspace mount mode must be omitted: %s", withoutMode)
	}
}

func containsJSONField(raw []byte, field string) bool {
	return strings.Contains(string(raw), field)
}
