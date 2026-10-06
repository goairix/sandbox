package handler_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/goairix/fs/driver/local"
	"github.com/goairix/sandbox/internal/api/handler"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/sandbox"
	"github.com/goairix/sandbox/internal/storage"
	"github.com/goairix/sandbox/internal/telemetry/metrics"
	"github.com/goairix/sandbox/pkg/types"
	"github.com/stretchr/testify/require"
)

type workspaceRequestRuntime struct {
	runtime.Runtime
	info    *runtime.SandboxInfo
	created int
}

func (r *workspaceRequestRuntime) CreateSandbox(_ context.Context, spec runtime.SandboxSpec) (*runtime.SandboxInfo, error) {
	r.created++
	r.info = &runtime.SandboxInfo{ID: spec.ID, RuntimeID: "container-" + spec.ID, RuntimeUID: "container-" + spec.ID, State: "running", WorkspaceHostPath: spec.Mounts[0].HostPath}
	return r.info, nil
}
func (r *workspaceRequestRuntime) RenameSandbox(context.Context, string, string) error { return nil }
func (r *workspaceRequestRuntime) GetSandbox(context.Context, string) (*runtime.SandboxInfo, error) {
	return r.info, nil
}
func (r *workspaceRequestRuntime) RemoveSandbox(context.Context, string) error { return nil }
func TestCreateSandboxHTTPReusesWorkspace(t *testing.T) {
	initFileHandlerMetrics.Do(func() { require.NoError(t, metrics.InitNoop()) })
	rt := &workspaceRequestRuntime{}
	root := t.TempDir()
	filesystem, err := local.New(local.Config{RootPath: root})
	require.NoError(t, err)
	m := sandbox.NewManager(rt, filesystem, &storage.FileSystemMeta{Provider: storage.ProviderLocal, LocalPath: root}, sandbox.ManagerConfig{RuntimeType: "docker"})
	t.Cleanup(func() { require.NoError(t, m.Stop(context.Background())) })
	h := handler.NewHandler(m)
	router := gin.New()
	router.POST("/api/v1/sandboxes", h.CreateSandbox)
	var id string
	for range 2 {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/v1/sandboxes", strings.NewReader(`{"mode":"persistent","workspace_path":"team/a"}`))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		require.Equal(t, 201, w.Code, w.Body.String())
		var response types.SandboxResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		require.NotEmpty(t, response.ID)
		require.Equal(t, "team/a", response.WorkspacePath)
		if id == "" {
			id = response.ID
		} else {
			require.Equal(t, id, response.ID)
		}
	}
	require.Equal(t, 1, rt.created)
}
