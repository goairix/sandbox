package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/internal/api/handler"
	"github.com/goairix/sandbox/pkg/types"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newFileTestRouter creates a minimal Gin router with the file handlers.
// It uses a nil manager — validation errors are returned before the manager is called.
func newFileTestRouter() *gin.Engine {
	h := handler.NewHandler(nil)
	r := gin.New()
	r.POST("/sandboxes/:id/files/list-recursive", h.ListFilesRecursive)
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
