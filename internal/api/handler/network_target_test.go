package handler

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInvalidNetworkTargetReturnsBadRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	internalError(c, fmt.Errorf("network: %w", runtime.ErrInvalidNetworkTarget))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "NETWORK_TARGET_INVALID")
}
