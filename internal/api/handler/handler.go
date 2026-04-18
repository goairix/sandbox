package handler

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/internal/sandbox"
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

// internalError logs the error and responds with 500.
func internalError(c *gin.Context, err error) {
	log.Printf("ERROR [%s %s]: %v", c.Request.Method, c.Request.URL.Path, err)
	c.JSON(http.StatusInternalServerError, types.ErrorResponse{Message: err.Error()})
}
