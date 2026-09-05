package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/api/handler"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/sandbox"
	"github.com/goairix/sandbox/internal/telemetry/metrics"
	"github.com/goairix/sandbox/pkg/types"
)

var initFileHandlerMetrics sync.Once

type uploadRuntime struct {
	runtime.Runtime
	uploadSize int64
}

func (r *uploadRuntime) CreateSandbox(_ context.Context, spec runtime.SandboxSpec) (*runtime.SandboxInfo, error) {
	return &runtime.SandboxInfo{ID: spec.ID, RuntimeID: "runtime-a", RuntimeUID: "uid-a"}, nil
}

func (r *uploadRuntime) RenameSandbox(context.Context, string, string) error { return nil }

func (r *uploadRuntime) UpdateLabels(context.Context, string, map[string]*string) error { return nil }

func (r *uploadRuntime) UploadFile(_ context.Context, _, _ string, size int64, reader io.Reader) error {
	_, err := io.Copy(io.Discard, reader)
	if err == nil {
		r.uploadSize = size
	}
	return err
}

func fileRouterWithRuntime(t *testing.T) (*gin.Engine, *uploadRuntime, string) {
	return fileRouterWithRuntimeLimit(t, 2<<30)
}

func fileRouterWithRuntimeLimit(t *testing.T, maxUploadBytes int64) (*gin.Engine, *uploadRuntime, string) {
	t.Helper()
	initFileHandlerMetrics.Do(func() { require.NoError(t, metrics.InitNoop()) })
	rt := &uploadRuntime{}
	mgr := sandbox.NewManager(rt, nil, nil, sandbox.ManagerConfig{})
	sb, err := mgr.Create(context.Background(), sandbox.SandboxConfig{
		Mode:    sandbox.ModePersistent,
		Network: sandbox.NetworkConfig{Enabled: true},
	})
	require.NoError(t, err)
	h := handler.NewHandler(mgr, maxUploadBytes)
	r := gin.New()
	r.POST("/sandboxes/:id/files/upload", h.UploadFile)
	return r, rt, sb.ID
}

func multipartBody(t *testing.T, field, filename string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile(field, filename)
	require.NoError(t, err)
	_, err = part.Write(content)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return &body, w.FormDataContentType()
}

func TestUploadFilePropagatesMultipartSize(t *testing.T) {
	router, rt, sandboxID := fileRouterWithRuntime(t)
	body, contentType := multipartBody(t, "file", "a.txt", []byte("hello"))
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/"+sandboxID+"/files/upload?path=/workspace/a.txt", body)
	req.Header.Set("Content-Type", contentType)
	// The transport declaration, rather than multipart.FileHeader.Size, is the
	// contract passed through to the runtime's exact-size stream verifier.
	req.Header.Set("X-Sandbox-File-Size", "7")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, int64(7), rt.uploadSize)
}

func TestUploadFileKeepsLegacySmallRequestAndTrailingPathFieldCompatible(t *testing.T) {
	router, rt, sandboxID := fileRouterWithRuntime(t)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "a.txt")
	require.NoError(t, err)
	_, err = part.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, w.WriteField("path", "/workspace/nested/a.txt"))
	require.NoError(t, w.Close())
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/"+sandboxID+"/files/upload", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, int64(5), rt.uploadSize)
	require.Contains(t, recorder.Body.String(), "/workspace/nested/a.txt")
}

func TestUploadFileRejectsDeclaredSizeAboveConfiguredLimit(t *testing.T) {
	router, rt, sandboxID := fileRouterWithRuntimeLimit(t, 4)
	body, contentType := multipartBody(t, "file", "a.txt", []byte("hello"))
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/"+sandboxID+"/files/upload?path=/workspace/a.txt", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Sandbox-File-Size", "5")
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, recorder.Code)
	require.Zero(t, rt.uploadSize)
}

func TestUploadFileDeclaredSizeRequiresPathBeforeStreaming(t *testing.T) {
	router, rt, sandboxID := fileRouterWithRuntime(t)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "a.txt")
	require.NoError(t, err)
	_, err = part.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, w.WriteField("path", "/workspace/nested/a.txt"))
	require.NoError(t, w.Close())
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/"+sandboxID+"/files/upload", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-Sandbox-File-Size", "5")
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Zero(t, rt.uploadSize)
}

func TestUploadFileDeclaredSizeRejectsMultipartPathEvenBeforeFile(t *testing.T) {
	router, rt, sandboxID := fileRouterWithRuntime(t)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	require.NoError(t, w.WriteField("path", "/workspace/body.txt"))
	part, err := w.CreateFormFile("file", "a.txt")
	require.NoError(t, err)
	_, err = part.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/"+sandboxID+"/files/upload?path=/workspace/query.txt", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-Sandbox-File-Size", "5")
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Zero(t, rt.uploadSize)
}

func TestUploadFileDeclaredSizeRejectsTrailingMultipartPathBeforePublish(t *testing.T) {
	router, rt, sandboxID := fileRouterWithRuntime(t)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "a.txt")
	require.NoError(t, err)
	_, err = part.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, w.WriteField("path", "/workspace/body.txt"))
	require.NoError(t, w.Close())
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/"+sandboxID+"/files/upload?path=/workspace/query.txt", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-Sandbox-File-Size", "5")
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Zero(t, rt.uploadSize, "runtime must not report a successful publish")
}

func init() {
	gin.SetMode(gin.TestMode)
}

// newFileTestRouter creates a minimal Gin router with the file handlers.
// It uses a nil manager — validation errors are returned before the manager is called.
func newFileTestRouter() *gin.Engine {
	h := handler.NewHandler(nil)
	r := gin.New()
	r.POST("/sandboxes/:id/files/list-recursive", h.ListFilesRecursive)
	r.POST("/sandboxes/:id/files/glob", h.GlobFiles)
	r.POST("/sandboxes/:id/files/read-lines", h.ReadFileLines)
	r.POST("/sandboxes/:id/files/edit", h.EditFile)
	r.POST("/sandboxes/:id/files/edit-lines", h.EditFileLines)
	return r
}

func doPost(t *testing.T, r *gin.Engine, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestListFilesRecursive_MissingPath(t *testing.T) {
	r := newFileTestRouter()
	w := doPost(t, r, "/sandboxes/sb-1/files/list-recursive", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReadFileLines_MissingPath(t *testing.T) {
	r := newFileTestRouter()
	w := doPost(t, r, "/sandboxes/sb-1/files/read-lines", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReadFileLines_InvalidPath(t *testing.T) {
	r := newFileTestRouter()
	w := doPost(t, r, "/sandboxes/sb-1/files/read-lines", map[string]any{
		"path": "../../etc/passwd",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp types.ErrorResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Message == "" {
		t.Error("expected non-empty error message")
	}
}

func TestEditFile_MissingPath(t *testing.T) {
	r := newFileTestRouter()
	w := doPost(t, r, "/sandboxes/sb-1/files/edit", map[string]any{
		"old_str": "foo",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestEditFile_MissingOldStr(t *testing.T) {
	r := newFileTestRouter()
	w := doPost(t, r, "/sandboxes/sb-1/files/edit", map[string]any{
		"path": "/workspace/file.txt",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestEditFileLines_MissingPath(t *testing.T) {
	r := newFileTestRouter()
	w := doPost(t, r, "/sandboxes/sb-1/files/edit-lines", map[string]any{
		"start_line": 1,
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestEditFileLines_InvalidStartLine(t *testing.T) {
	r := newFileTestRouter()
	w := doPost(t, r, "/sandboxes/sb-1/files/edit-lines", map[string]any{
		"path":       "/workspace/file.txt",
		"start_line": 0,
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGlobFiles_MissingPath(t *testing.T) {
	r := newFileTestRouter()
	w := doPost(t, r, "/sandboxes/sb-1/files/glob", map[string]any{
		"pattern": "*.txt",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGlobFiles_MissingPattern(t *testing.T) {
	r := newFileTestRouter()
	w := doPost(t, r, "/sandboxes/sb-1/files/glob", map[string]any{
		"path": "/workspace",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGlobFiles_InvalidPath(t *testing.T) {
	r := newFileTestRouter()
	w := doPost(t, r, "/sandboxes/sb-1/files/glob", map[string]any{
		"path":    "../../etc",
		"pattern": "*.txt",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}
