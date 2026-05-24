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
// appropriate HTTP status code. context.Canceled is silently ignored.
// runtime.ErrFileNotFound maps to 404; everything else maps to 500.
func internalError(c *gin.Context, err error) {
	if context.Cause(c.Request.Context()) != nil {
		return
	}
	if errors.Is(err, runtime.ErrFileNotFound) {
		c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
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
