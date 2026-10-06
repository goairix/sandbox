package sandbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	sandbox "github.com/goairix/sandbox/sdk/go"
)

func TestClientGetSandboxByWorkspace(t *testing.T) {
	root := "团队/a +b&c?d#e"
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/sandboxes/by-workspace" || r.URL.Query().Get("workspace_path") != root || len(r.URL.Query()) != 1 {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing authentication")
		}
		_ = json.NewEncoder(w).Encode(sandbox.SandboxResponse{ID: "sandbox-old", WorkspacePath: root, WorkspaceMountMode: sandbox.WorkspaceMountFUSE})
	})
	got, err := client.GetSandboxByWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "sandbox-old" || got.WorkspacePath != root || got.WorkspaceMountMode != sandbox.WorkspaceMountFUSE {
		t.Fatalf("unexpected response: %+v", got)
	}
}

func TestClientGetSandboxByWorkspaceErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{{404, "SANDBOX_NOT_FOUND"}, {409, "WORKSPACE_SANDBOX_CONFLICT"}, {503, "WORKSPACE_LOOKUP_UNAVAILABLE"}} {
		t.Run(tc.code, func(t *testing.T) {
			_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"code": tc.code, "message": "lookup failed"})
			})
			_, err := client.GetSandboxByWorkspace(context.Background(), "team/a")
			var apiErr *sandbox.SandboxError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != tc.status || apiErr.Code != tc.code {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.status == 404 && !errors.Is(err, sandbox.ErrNotFound) {
				t.Fatalf("expected ErrNotFound, got %v", err)
			}
		})
	}
}
