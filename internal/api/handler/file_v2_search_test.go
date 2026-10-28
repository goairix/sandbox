package handler_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFilesV2GlobHTTP(t *testing.T) {
	router, _, id := fileV2HTTPRouter(t)
	endpoint := "/sandboxes/" + id + "/files/glob"
	request := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(`{"path":"/workspace","pattern":"**/*.go","limit":1}`))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	require.Contains(t, response.Body.String(), `"path":"/workspace/a.go"`)
	require.Contains(t, response.Body.String(), `"truncated":true`)

	for _, body := range []string{
		`{"path":"/workspace","pattern":"../*.go"}`,
		`{"path":"/workspace","pattern":"["}`,
		`{"path":"/workspace","pattern":"*.go","limit":1001}`,
		`{"path":"/workspace","pattern":"*.go","other":true}`,
	} {
		response = httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body)))
		require.True(t, response.Code >= 400, "accepted %s: %s", body, response.Body.String())
	}
}
