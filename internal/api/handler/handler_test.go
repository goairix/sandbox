package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/sandbox"
	"github.com/goairix/sandbox/pkg/types"
)

func TestCreateSandboxRejectsMountModeWithoutWorkspace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", strings.NewReader(`{"mode":"ephemeral","workspace_mount_mode":"fuse"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	NewHandler(nil).CreateSandbox(c)

	assert.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "WORKSPACE_MOUNT_MODE_INVALID")
}

func TestCreateSandboxRejectsUnknownWorkspaceMountModeWithStableCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", strings.NewReader(`{"mode":"ephemeral","workspace_path":"jobs/a","workspace_mount_mode":"volume"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	NewHandler(nil).CreateSandbox(c)

	assert.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "WORKSPACE_MOUNT_MODE_INVALID")
}

func TestSandboxResponseIncludesResolvedWorkspaceMountMode(t *testing.T) {
	sb := &sandbox.Sandbox{
		Config:    sandbox.SandboxConfig{Mode: sandbox.ModePersistent, WorkspaceMountMode: sandbox.WorkspaceMountFUSE},
		Workspace: &sandbox.WorkspaceInfo{MountType: sandbox.WorkspaceMountFUSE},
	}
	assert.Equal(t, "fuse", sandboxToResponse(sb).WorkspaceMountMode)

	sb.Workspace = nil
	assert.Empty(t, sandboxToResponse(sb).WorkspaceMountMode)
}

func TestResourceLimitsFromRequestIncludesTmpDisk(t *testing.T) {
	got := resourceLimitsFromRequest(&types.ResourceLimits{
		Memory:  "256Mi",
		CPU:     "500m",
		Disk:    "1Gi",
		TmpDisk: "200Mi",
	})

	assert.Equal(t, sandbox.ResourceLimits{
		Memory:  "256Mi",
		CPU:     "500m",
		Disk:    "1Gi",
		TmpDisk: "200Mi",
	}, got)
}

func TestInternalErrorExecTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/id/exec", nil)

	internalError(c, fmt.Errorf("exec failed: %w", sandbox.ErrExecTimeout))

	assert.Equal(t, http.StatusRequestTimeout, recorder.Code)
	assert.JSONEq(t, `{"code":"EXEC_TIMEOUT","message":"exec failed: sandbox execution timed out"}`, recorder.Body.String())
}

func TestInternalErrorInvalidExecTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/id/exec", nil)

	internalError(c, fmt.Errorf("timeout=601: %w", sandbox.ErrInvalidExecTimeout))

	assert.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.JSONEq(t, `{"code":"INVALID_EXEC_TIMEOUT","message":"timeout=601: invalid execution timeout"}`, recorder.Body.String())
}

func TestInternalErrorFUSEWorkspaceConflicts(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{name: "immutable", err: sandbox.ErrFUSEWorkspaceImmutable, code: "FUSE_WORKSPACE_IMMUTABLE"},
		{name: "owned", err: sandbox.ErrWorkspaceOwned, code: "WORKSPACE_OWNED"},
		{name: "leased", err: sandbox.ErrWorkspaceLeased, code: "WORKSPACE_LEASED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/id/workspace/mount", nil)

			internalError(c, tc.err)

			assert.Equal(t, http.StatusConflict, recorder.Code)
			assert.Contains(t, recorder.Body.String(), tc.code)
		})
	}
}

func TestInternalErrorSandboxNotReadyIsServiceUnavailable(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/id/files/read", nil)

	require.NotPanics(t, func() { internalError(c, sandbox.ErrSandboxNotReady) })

	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "SANDBOX_NOT_READY")
}

func TestInternalErrorSandboxCleanupPendingIsServiceUnavailable(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/sandboxes/id", nil)

	internalError(c, sandbox.ErrSandboxCleanupPending)

	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "SANDBOX_CLEANUP_PENDING")
}

func TestHandleSkillNotReadyUsesServiceUnavailable(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/id/skills", nil)

	handled := handleSkillNotReady(c, fmt.Errorf("skill read: %w", sandbox.ErrSandboxNotReady))

	require.True(t, handled)
	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "SANDBOX_NOT_READY")
}

func TestStreamErrorData(t *testing.T) {
	t.Run("exec timeout", func(t *testing.T) {
		got := streamErrorData(sandbox.ErrExecTimeout.Error())
		assert.Equal(t, "EXEC_TIMEOUT", got.Error)
		assert.Equal(t, sandbox.ErrExecTimeout.Error(), got.Message)
	})

	t.Run("generic runtime error", func(t *testing.T) {
		got := streamErrorData("process exited unexpectedly")
		assert.Equal(t, "exec_error", got.Error)
		assert.Equal(t, "process exited unexpectedly", got.Message)
	})
}
