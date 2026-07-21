package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/goairix/sandbox/internal/sandbox"
)

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
