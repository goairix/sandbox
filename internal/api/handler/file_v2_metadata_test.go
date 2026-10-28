package handler_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFilesV2MetadataHTTP(t *testing.T) {
	router, _, id := fileV2HTTPRouter(t)
	base := "/sandboxes/" + id + "/files"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, base+"/stat?path=/workspace/a.txt", nil))
	require.Equal(t, http.StatusOK, response.Code)
	require.Contains(t, response.Body.String(), `"path":"/workspace/a.txt"`)
	require.Contains(t, response.Body.String(), `"mode":"-rw-r--r--"`)
	require.Contains(t, response.Body.String(), `"nlink":1`)

	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, base+"/list?path=/workspace&limit=1", nil))
	require.Equal(t, http.StatusOK, response.Code)
	require.Contains(t, response.Body.String(), `"next_cursor":"/workspace/a.txt"`)
	require.Contains(t, response.Body.String(), `"uid":1000`)

	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, base+"/list?path=/workspace&limit=1001", nil))
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Contains(t, response.Body.String(), "FILE_INVALID")
}
