package handler_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/goairix/sandbox/internal/api/handler"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/sandbox"
	"github.com/goairix/sandbox/internal/telemetry/metrics"
	"github.com/stretchr/testify/require"
)

type downloadRuntime struct {
	uploadRuntime
	reader io.ReadCloser
	err    error
}

func (r *downloadRuntime) DownloadFile(context.Context, string, string) (io.ReadCloser, error) {
	return r.reader, r.err
}

type failedDownloadReader struct{ err error }

func (r failedDownloadReader) Read([]byte) (int, error) { return 0, r.err }
func (r failedDownloadReader) Close() error             { return nil }

func downloadTestRouter(t *testing.T, rt runtime.Runtime) (*gin.Engine, string) {
	t.Helper()
	initFileHandlerMetrics.Do(func() { require.NoError(t, metrics.InitNoop()) })
	mgr := sandbox.NewManager(rt, nil, nil, sandbox.ManagerConfig{})
	sb, err := mgr.Create(context.Background(), sandbox.SandboxConfig{Mode: sandbox.ModePersistent})
	require.NoError(t, err)
	router := gin.New()
	router.GET("/sandboxes/:id/files/download", handler.NewHandler(mgr).DownloadFile)
	return router, sb.ID
}

func TestDownloadFileHandlesMissingBeforeHeadersAndEmptyFiles(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		err       error
		streamErr error
		status    int
		code      string
	}{
		{name: "runtime missing", err: fmt.Errorf("download: %w", runtime.ErrFileNotFound), status: http.StatusNotFound, code: "FILE_NOT_FOUND"},
		{name: "stream missing", streamErr: fmt.Errorf("download: %w", runtime.ErrFileNotFound), status: http.StatusNotFound, code: "FILE_NOT_FOUND"},
		{name: "permission", err: errors.New("permission denied"), status: http.StatusInternalServerError},
		{name: "transport", streamErr: errors.New("transport failed"), status: http.StatusInternalServerError},
		{name: "missing pod", err: runtime.ErrNotFound, status: http.StatusInternalServerError},
		{name: "empty", status: http.StatusOK},
		{name: "content", body: "hello 世界\n", status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var archive bytes.Buffer
			tw := tar.NewWriter(&archive)
			require.NoError(t, tw.WriteHeader(&tar.Header{Name: "workspace/a.txt", Mode: 0o644, Size: int64(len(tc.body))}))
			_, err := tw.Write([]byte(tc.body))
			require.NoError(t, err)
			require.NoError(t, tw.Close())
			rt := &downloadRuntime{reader: io.NopCloser(bytes.NewReader(archive.Bytes())), err: tc.err}
			if tc.streamErr != nil {
				rt.reader = failedDownloadReader{err: tc.streamErr}
			}
			router, id := downloadTestRouter(t, rt)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/sandboxes/"+id+"/files/download?path=/workspace/a.txt", nil))
			require.Equal(t, tc.status, recorder.Code)
			if tc.status == http.StatusOK {
				require.Equal(t, tc.body, recorder.Body.String())
			} else {
				require.Empty(t, recorder.Header().Get("Content-Disposition"), "error response must not have attachment headers")
				if tc.code != "" {
					require.Contains(t, recorder.Body.String(), tc.code)
				} else {
					require.NotContains(t, recorder.Body.String(), "FILE_NOT_FOUND")
				}
			}
		})
	}
}
