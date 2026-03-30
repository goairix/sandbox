package api

import (
	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/internal/api/handler"
	"github.com/goairix/sandbox/internal/api/middleware"
)

// SetupRouter configures all routes.
func SetupRouter(h *handler.Handler, apiKey string, rateLimit int) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())

	// Health check (no auth)
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	v1 := r.Group("/api/v1")
	v1.Use(middleware.Auth(apiKey))
	if rateLimit > 0 {
		v1.Use(middleware.RateLimit(rateLimit))
	}

	// Sandbox management
	v1.POST("/sandboxes", h.CreateSandbox)
	v1.GET("/sandboxes/:id", h.GetSandbox)
	v1.DELETE("/sandboxes/:id", h.DestroySandbox)

	// Execution within a sandbox
	v1.POST("/sandboxes/:id/exec", h.ExecSync)
	v1.POST("/sandboxes/:id/exec/stream", h.ExecStream)

	// File operations
	v1.POST("/sandboxes/:id/files/upload", h.UploadFile)
	v1.GET("/sandboxes/:id/files/download", h.DownloadFile)
	v1.GET("/sandboxes/:id/files/list", h.ListFiles)

	// One-shot execution
	v1.POST("/execute", h.ExecuteOneShot)
	v1.POST("/execute/stream", h.ExecuteOneShotStream)

	return r
}
