package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/api/handler"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/sandbox"
	"github.com/goairix/sandbox/internal/telemetry/metrics"
)

type fileV2HTTPRuntime struct {
	uploadRuntime
	content []byte
}

func (r *fileV2HTTPRuntime) RunFileOp(_ context.Context, _ string, args []string, input io.Reader) (io.ReadCloser, error) {
	switch args[0] {
	case "stat":
		if args[1] == "/workspace/missing" {
			return nil, runtime.ErrFileNotFound
		}
		body, err := json.Marshal(map[string]any{"name": "a.txt", "path": args[1], "size": len(r.content), "is_dir": false, "mod_time": time.Unix(0, 0).UTC()})
		return io.NopCloser(bytes.NewReader(body)), err
	case "read":
		start, _ := strconv.Atoi(args[2])
		length, _ := strconv.Atoi(args[3])
		if length < 0 {
			length = len(r.content) - start
		}
		return io.NopCloser(bytes.NewReader(r.content[start : start+length])), nil
	case "write":
		body, err := io.ReadAll(input)
		if err != nil {
			return nil, err
		}
		size, _ := strconv.Atoi(args[2])
		if len(body) != size {
			return nil, runtime.ErrInvalidUploadSize
		}
		r.content = body
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	return nil, nil
}

func fileV2HTTPRouter(t *testing.T) (*gin.Engine, *fileV2HTTPRuntime, string) {
	t.Helper()
	initFileHandlerMetrics.Do(func() { require.NoError(t, metrics.InitNoop()) })
	rt := &fileV2HTTPRuntime{content: []byte("abcdef")}
	mgr := sandbox.NewManager(rt, nil, nil, sandbox.ManagerConfig{})
	sb, err := mgr.Create(context.Background(), sandbox.SandboxConfig{Mode: sandbox.ModePersistent})
	require.NoError(t, err)
	h := handler.NewHandler(mgr, 16)
	router := gin.New()
	router.GET("/sandboxes/:id/files/content", h.ReadFileContentV2)
	router.PUT("/sandboxes/:id/files/content", h.WriteFileContentV2)
	return router, rt, sb.ID
}

func TestFilesV2Content(t *testing.T) {
	router, rt, id := fileV2HTTPRouter(t)
	url := "/sandboxes/" + id + "/files/content?path=/workspace/a.txt"
	request := httptest.NewRequest(http.MethodGet, url, nil)
	request.Header.Set("Range", "bytes=2-4")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusPartialContent, response.Code)
	require.Equal(t, "cde", response.Body.String())
	require.Equal(t, "bytes 2-4/6", response.Header().Get("Content-Range"))
	request = httptest.NewRequest(http.MethodGet, url, nil)
	request.Header.Set("Range", "bytes=-2")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusPartialContent, response.Code)
	require.Equal(t, "ef", response.Body.String())

	request = httptest.NewRequest(http.MethodGet, url, nil)
	request.Header.Set("Range", "bytes=6-")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, response.Code)
	require.Equal(t, "bytes */6", response.Header().Get("Content-Range"))

	request = httptest.NewRequest(http.MethodPut, url+"&create_parents=true", strings.NewReader("xyz"))
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusNoContent, response.Code)
	require.Equal(t, "xyz", string(rt.content))
	badLength := httptest.NewRequest(http.MethodPut, url, strings.NewReader("x"))
	badLength.ContentLength = 2
	response = httptest.NewRecorder()
	router.ServeHTTP(response, badLength)
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Contains(t, response.Body.String(), "FILE_INVALID")

	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, url, nil))
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "xyz", response.Body.String())

	rt.content = nil
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, url, nil))
	require.Equal(t, http.StatusOK, response.Code)
	require.Empty(t, response.Body.String())

	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/sandboxes/"+id+"/files/content?path=/workspace/missing", nil))
	require.Equal(t, http.StatusNotFound, response.Code)
	require.Contains(t, response.Body.String(), "FILE_NOT_FOUND")
}
