package handler

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/goairix/sandbox/internal/sandbox"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceLookupErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{sandbox.ErrWorkspaceLookupConflict, 409, "WORKSPACE_SANDBOX_CONFLICT"},
		{sandbox.ErrWorkspaceLookupAmbiguous, 409, "WORKSPACE_SANDBOX_AMBIGUOUS"},
		{sandbox.ErrWorkspaceLookupUnavailable, 503, "WORKSPACE_LOOKUP_UNAVAILABLE"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("GET", "/api/v1/sandboxes/by-workspace", nil)
			internalError(c, fmt.Errorf("lookup: %w", tc.err))
			require.Equal(t, tc.status, w.Code)
			require.Contains(t, w.Body.String(), tc.code)
		})
	}
}
