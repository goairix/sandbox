package handler_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFilesV2TextHTTP(t *testing.T) {
	router, rt, id := fileV2HTTPRouter(t)
	rt.content = []byte("α one\r\nβ two\n")
	base := "/sandboxes/" + id + "/files/text/"
	read := httptest.NewRequest(http.MethodPost, base+"read-lines", strings.NewReader(`{"path":"/workspace/a.txt","start_line":1,"line_count":0}`))
	read.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, read)
	require.Equal(t, http.StatusOK, response.Code)
	require.Contains(t, response.Body.String(), `"content"`)
	require.Contains(t, response.Body.String(), `α one`)

	edit := httptest.NewRequest(http.MethodPost, base+"edit", strings.NewReader(`{"path":"/workspace/a.txt","old_string":"one","new_string":"ONE"}`))
	edit.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, edit)
	require.Equal(t, http.StatusNoContent, response.Code)
	require.Equal(t, "α ONE\r\nβ two\n", string(rt.content))

	conflict := httptest.NewRequest(http.MethodPost, base+"edit", strings.NewReader(`{"path":"/workspace/a.txt","old_string":"missing","new_string":"x"}`))
	conflict.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, conflict)
	require.Equal(t, http.StatusConflict, response.Code)
	require.Contains(t, response.Body.String(), "FILE_CONFLICT")
}
