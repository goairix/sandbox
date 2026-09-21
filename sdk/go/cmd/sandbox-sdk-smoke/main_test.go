package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sandbox "github.com/goairix/sandbox/sdk/go"
)

type smokeServer struct {
	mu        sync.Mutex
	created   []sandbox.CreateSandboxRequest
	deleted   []string
	unmounted bool
}

func (s *smokeServer) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/api/v1/sandboxes" && r.Method == http.MethodPost {
		var req sandbox.CreateSandboxRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.created = append(s.created, req)
		id := "ordinary"
		if req.WorkspaceMountMode == sandbox.WorkspaceMountFUSE {
			id = "fuse"
		}
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(sandbox.SandboxResponse{ID: id, Mode: req.Mode, State: "running", WorkspaceMountMode: req.WorkspaceMountMode})
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/v1/sandboxes/") {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/sandboxes/"), "/")
	if len(parts) == 1 && r.Method == http.MethodDelete {
		s.mu.Lock()
		s.deleted = append(s.deleted, parts[0])
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := parts[0]
	switch {
	case len(parts) == 2 && parts[1] == "exec" && r.Method == http.MethodPost:
		out := "ordinary-pass"
		if id == "fuse" {
			out = "fuse-pass"
		}
		_ = json.NewEncoder(w).Encode(sandbox.ExecResponse{ExitCode: 0, Stdout: out})
	case len(parts) == 3 && parts[1] == "workspace" && parts[2] == "sync" && r.Method == http.MethodPost:
		_ = json.NewEncoder(w).Encode(sandbox.SyncWorkspaceResponse{Direction: string(sandbox.SyncDirectionFromContainer), Message: "sync completed"})
	case len(parts) == 3 && parts[1] == "workspace" && parts[2] == "info" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(sandbox.WorkspaceInfoResponse{Mounted: !s.unmounted, MountType: "fuse", MountState: "ready", Flushed: true})
	default:
		http.NotFound(w, r)
	}
}

func TestRunSmokeExercisesOrdinaryAndFUSEWorkflows(t *testing.T) {
	state := &smokeServer{}
	srv := httptest.NewServer(http.HandlerFunc(state.handler))
	t.Cleanup(srv.Close)

	client := sandbox.NewClient(srv.URL, "test-key")
	if err := runSmoke(context.Background(), client, "sdk-test"); err != nil {
		t.Fatalf("runSmoke error: %v", err)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.created) != 2 || len(state.deleted) != 2 {
		t.Fatalf("created=%d deleted=%d, want two of each", len(state.created), len(state.deleted))
	}
	if state.created[0].WorkspaceMountMode != sandbox.WorkspaceMountSync || state.created[1].WorkspaceMountMode != sandbox.WorkspaceMountFUSE {
		t.Fatalf("workspace modes = %q, %q", state.created[0].WorkspaceMountMode, state.created[1].WorkspaceMountMode)
	}
}

func TestValidateFUSEWorkspaceRejectsUnmountedState(t *testing.T) {
	info := sandbox.WorkspaceInfoResponse{Mounted: false, MountType: "fuse", MountState: "ready", Flushed: true}
	if err := validateFUSEWorkspace(info); err == nil {
		t.Fatal("validateFUSEWorkspace accepted an unmounted workspace")
	}
}
