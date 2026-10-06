package handler_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
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
	r.info = &runtime.SandboxInfo{ID: spec.ID, RuntimeID: "container-" + spec.ID, RuntimeUID: "container-" + spec.ID, State: "running"}
	if len(spec.Mounts) > 0 {
		r.info.WorkspaceHostPath = spec.Mounts[0].HostPath
	}
	return r.info, nil
}
func (r *workspaceRequestRuntime) RenameSandbox(context.Context, string, string) error { return nil }
func (r *workspaceRequestRuntime) UpdateLabels(context.Context, string, map[string]*string) error {
	return nil
}
func (r *workspaceRequestRuntime) GetSandbox(context.Context, string) (*runtime.SandboxInfo, error) {
	return r.info, nil
}
func (r *workspaceRequestRuntime) RemoveSandbox(context.Context, string) error { return nil }
func workspaceRequestRouter(t *testing.T) (*gin.Engine, *workspaceRequestRuntime) {
	t.Helper()
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
	router.GET("/api/v1/sandboxes/:id", h.GetSandbox)
	return router, rt
}

func TestCreateSandboxHTTPReusesWorkspace(t *testing.T) {
	router, rt := workspaceRequestRouter(t)
	var id string
	for i := range 2 {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/v1/sandboxes", strings.NewReader(`{"mode":"persistent","workspace_path":"team/a"}`))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		require.Equal(t, 201, w.Code, w.Body.String())
		var response types.SandboxResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		var fields map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fields))
		reused, present := fields["reused"]
		require.True(t, present, "create response must include reused even when false")
		require.Equal(t, i > 0, reused)
		require.NotEmpty(t, response.ID)
		require.Equal(t, "team/a", response.WorkspacePath)
		if id == "" {
			id = response.ID
		} else {
			require.Equal(t, id, response.ID)
		}
	}
	require.Equal(t, 1, rt.created)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/sandboxes/"+id, nil))
	require.Equal(t, 200, w.Code, w.Body.String())
	var fields map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fields))
	require.NotContains(t, fields, "reused", "reuse describes a create request, not stored sandbox state")
}

func TestCreateSandboxHTTPConcurrentReuseOutcome(t *testing.T) {
	router, rt := workspaceRequestRouter(t)
	const count = 12
	responses := make([]*httptest.ResponseRecorder, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/api/v1/sandboxes", strings.NewReader(`{"mode":"persistent","workspace_path":"team/a"}`))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(w, req)
			responses[i] = w
		}()
	}
	wg.Wait()
	created := 0
	var id string
	for _, w := range responses {
		require.Equal(t, 201, w.Code, w.Body.String())
		var fields map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fields))
		reused, present := fields["reused"].(bool)
		require.True(t, present, "create response must include a boolean reused")
		if !reused {
			created++
		}
		if id == "" {
			id = fields["id"].(string)
		}
		require.Equal(t, id, fields["id"])
	}
	require.Equal(t, 1, created)
	require.Equal(t, 1, rt.created)
}

func TestCreateSandboxHTTPWithoutWorkspaceIsCreated(t *testing.T) {
	router, rt := workspaceRequestRouter(t)
	for range 2 {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/v1/sandboxes", strings.NewReader(`{"mode":"ephemeral"}`))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		require.Equal(t, 201, w.Code, w.Body.String())
		var fields map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fields))
		reused, present := fields["reused"]
		require.True(t, present)
		require.Equal(t, false, reused)
	}
	require.Equal(t, 2, rt.created)
}
