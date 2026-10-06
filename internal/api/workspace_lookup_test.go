package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/goairix/fs/driver/local"
	"github.com/goairix/sandbox/internal/api/handler"
	sandboxruntime "github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/sandbox"
	"github.com/goairix/sandbox/internal/storage"
	"github.com/goairix/sandbox/internal/telemetry/metrics"
	"github.com/goairix/sandbox/pkg/types"
	"github.com/stretchr/testify/require"
)

type lookupRuntime struct {
	sandboxruntime.Runtime
	workspaceHostPath string
}

func (r lookupRuntime) CreateSandbox(_ context.Context, spec sandboxruntime.SandboxSpec) (*sandboxruntime.SandboxInfo, error) {
	return &sandboxruntime.SandboxInfo{ID: spec.ID, RuntimeID: "container-a", RuntimeUID: "container-a", State: "running"}, nil
}
func (r lookupRuntime) GetSandbox(context.Context, string) (*sandboxruntime.SandboxInfo, error) {
	return &sandboxruntime.SandboxInfo{RuntimeID: "container-a", RuntimeUID: "container-a", State: "running", WorkspaceHostPath: r.workspaceHostPath}, nil
}
func (r lookupRuntime) UpdateLabels(context.Context, string, map[string]*string) error { return nil }

func (r lookupRuntime) RenameSandbox(context.Context, string, string) error { return nil }

func TestWorkspaceLookupRouteAndAuth(t *testing.T) {
	initRouterMetrics.Do(func() { require.NoError(t, metrics.InitNoop()) })
	localRoot := t.TempDir()
	fsys, err := local.New(local.Config{RootPath: localRoot})
	require.NoError(t, err)
	mgr := sandbox.NewManager(lookupRuntime{workspaceHostPath: localRoot + "/team/a"}, fsys, &storage.FileSystemMeta{Provider: storage.ProviderLocal, LocalPath: localRoot}, sandbox.ManagerConfig{RuntimeType: "docker"})
	t.Cleanup(func() { require.NoError(t, mgr.Stop(context.Background())) })
	sb, err := mgr.Create(context.Background(), sandbox.SandboxConfig{Mode: sandbox.ModePersistent, WorkspacePath: "team/a", Timeout: -1})
	require.NoError(t, err)
	router := SetupRouter(handler.NewHandler(mgr), "key", 0, "test")
	for _, tc := range []struct {
		name, root, key, code string
		status                int
	}{
		{"found", "team/a", "key", "", 200},
		{"not_found", "team/b", "key", "SANDBOX_NOT_FOUND", 404},
		{"invalid", "../team/a", "key", "WORKSPACE_PATH_INVALID", 400},
		{"empty", "", "key", "WORKSPACE_PATH_INVALID", 400},
		{"unauthorized", "team/a", "other", "", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/by-workspace?workspace_path="+url.QueryEscape(tc.root), nil)
			req.Header.Set("Authorization", "Bearer "+tc.key)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.status == 200 {
				var resp types.SandboxResponse
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				require.Equal(t, sb.ID, resp.ID)
				require.Equal(t, "team/a", resp.WorkspacePath)
				require.Equal(t, "sync", resp.WorkspaceMountMode)
				require.Nil(t, resp.ExpiresAt)
			} else if tc.code != "" {
				var resp types.ErrorResponse
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				require.Equal(t, tc.code, resp.Code)
			}
		})
	}
	// Static lookup routing must not replace the existing ID endpoint.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/"+sb.ID, nil)
	req.Header.Set("Authorization", "Bearer key")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}
