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
	manager *sandbox.Manager
}

// NewHandler creates a new Handler.
func NewHandler(mgr *sandbox.Manager) *Handler {
	return &Handler{manager: mgr}
}

// internalError records the error on the current span and responds with the
// appropriate HTTP status code and error code. context.Canceled is silently
// ignored. Known sentinel errors are mapped to specific HTTP status + code;
// everything else is a 500.
func internalError(c *gin.Context, err error) {
	if context.Cause(c.Request.Context()) != nil {
		return
	}

	switch {
	case errors.Is(err, runtime.ErrFileNotFound):
		c.JSON(http.StatusNotFound, types.ErrorResponse{
			Code:    "FILE_NOT_FOUND",
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
