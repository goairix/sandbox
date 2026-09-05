package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"

	"github.com/goairix/sandbox/internal/logger"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/sandbox"
	telemetry "github.com/goairix/sandbox/internal/telemetry/trace"
	"github.com/goairix/sandbox/pkg/types"
)

// Handler holds shared dependencies for all HTTP handlers.
type Handler struct {
	manager        *sandbox.Manager
	maxUploadBytes int64
}

const defaultMaxUploadBytes int64 = 2 << 30

// NewHandler creates a new Handler.
func NewHandler(mgr *sandbox.Manager, maxUploadBytes ...int64) *Handler {
	limit := defaultMaxUploadBytes
	if len(maxUploadBytes) > 0 && maxUploadBytes[0] > 0 {
		limit = maxUploadBytes[0]
	}
	return &Handler{manager: mgr, maxUploadBytes: limit}
}

// MaxUploadBytes is the configured payload limit for the direct streaming
// upload route. It excludes bounded multipart framing overhead.
func (h *Handler) MaxUploadBytes() int64 { return h.maxUploadBytes }

// internalError records the error on the current span and responds with the
// appropriate HTTP status code and error code. context.Canceled is silently
// ignored. Known sentinel errors are mapped to specific HTTP status + code;
// everything else is a 500.
func internalError(c *gin.Context, err error) {
	if context.Cause(c.Request.Context()) != nil {
		return
	}

	switch {
	case errors.Is(err, sandbox.ErrSandboxNotReady):
		c.JSON(http.StatusServiceUnavailable, types.ErrorResponse{
			Code:    "SANDBOX_NOT_READY",
			Message: err.Error(),
		})
		return
	case errors.Is(err, sandbox.ErrInvalidExecTimeout):
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Code:    "INVALID_EXEC_TIMEOUT",
			Message: err.Error(),
		})
		return
	case errors.Is(err, sandbox.ErrExecTimeout):
		c.JSON(http.StatusRequestTimeout, types.ErrorResponse{
			Code:    "EXEC_TIMEOUT",
			Message: err.Error(),
		})
		return
	case errors.Is(err, runtime.ErrFileNotFound):
		c.JSON(http.StatusNotFound, types.ErrorResponse{
			Code:    "FILE_NOT_FOUND",
			Message: err.Error(),
		})
		return
	case errors.Is(err, runtime.ErrInvalidUploadSize):
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Code:    "INVALID_UPLOAD_SIZE",
			Message: err.Error(),
		})
		return
	case errors.Is(err, sandbox.ErrSandboxNotFound):
		c.JSON(http.StatusNotFound, types.ErrorResponse{
			Code:    "SANDBOX_NOT_FOUND",
			Message: err.Error(),
		})
		return
	case errors.Is(err, sandbox.ErrNoWorkspaceMounted):
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Code:    "NO_WORKSPACE_MOUNTED",
			Message: err.Error(),
		})
		return
	case errors.Is(err, sandbox.ErrWorkspaceAlreadyMounted):
		c.JSON(http.StatusConflict, types.ErrorResponse{
			Code:    "WORKSPACE_ALREADY_MOUNTED",
			Message: err.Error(),
		})
		return
	case errors.Is(err, sandbox.ErrFUSEWorkspaceImmutable):
		c.JSON(http.StatusConflict, types.ErrorResponse{
			Code:    "FUSE_WORKSPACE_IMMUTABLE",
			Message: err.Error(),
		})
		return
	case errors.Is(err, sandbox.ErrWorkspaceOwned):
		c.JSON(http.StatusConflict, types.ErrorResponse{
			Code:    "WORKSPACE_OWNED",
			Message: err.Error(),
		})
		return
	case errors.Is(err, sandbox.ErrWorkspaceLeased):
		c.JSON(http.StatusConflict, types.ErrorResponse{
			Code:    "WORKSPACE_LEASED",
			Message: err.Error(),
		})
		return
	case errors.Is(err, sandbox.ErrReservedFUSEWorkspacePath):
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Code:    "RESERVED_WORKSPACE_PATH",
			Message: err.Error(),
		})
		return
	case errors.Is(err, sandbox.ErrUploadNotFound):
		c.JSON(http.StatusNotFound, types.ErrorResponse{
			Code:    "UPLOAD_NOT_FOUND",
			Message: err.Error(),
		})
		return
	case errors.Is(err, sandbox.ErrUnexpectedChunkIndex):
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Code:    "UNEXPECTED_CHUNK_INDEX",
			Message: err.Error(),
		})
		return
	case errors.Is(err, sandbox.ErrIncompleteUpload):
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Code:    "INCOMPLETE_UPLOAD",
			Message: err.Error(),
		})
		return
	}

	span := trace.SpanFromContext(c.Request.Context())
	telemetry.Error(err, span)
	logger.Error(c.Request.Context(), "internal error",
		logger.AddField("method", c.Request.Method),
		logger.AddField("path", c.Request.URL.Path),
		logger.ErrorField(err),
	)
	c.JSON(http.StatusInternalServerError, types.ErrorResponse{Message: err.Error()})
}

func streamErrorData(message string) types.SSEErrorData {
	code := "exec_error"
	if message == sandbox.ErrExecTimeout.Error() {
		code = "EXEC_TIMEOUT"
	}
	return types.SSEErrorData{Error: code, Message: message}
}
