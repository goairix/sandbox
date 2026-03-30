package handler

import (
	"github.com/goairix/sandbox/internal/sandbox"
)

// Handler holds shared dependencies for all HTTP handlers.
type Handler struct {
	manager *sandbox.Manager
}

// NewHandler creates a new Handler.
func NewHandler(mgr *sandbox.Manager) *Handler {
	return &Handler{manager: mgr}
}
