package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/internal/api/handler"
	"github.com/goairix/sandbox/internal/api/middleware"
)

// BodySizeLimit returns a middleware that limits the size of request bodies.
func BodySizeLimit(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		}
		c.Next()
	}
}

// SetupRouter configures all routes.
func SetupRouter(h *handler.Handler, apiKey string, rateLimit int) *gin.Engine {
	r := gin.New()
	r.Use(gin.Logger())
	r.Use(gin.Recovery())

	// Limit multipart memory to 32MB
	r.MaxMultipartMemory = 32 << 20

	// Limit request body size to 64MB
	r.Use(BodySizeLimit(64 << 20))

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
	v1.PUT("/sandboxes/:id/network", h.UpdateNetwork)

	// Execution within a sandbox
	v1.POST("/sandboxes/:id/exec", h.ExecSync)
	v1.POST("/sandboxes/:id/exec/stream", h.ExecStream)

	// File operations
	v1.POST("/sandboxes/:id/files/upload", h.UploadFile)
	v1.GET("/sandboxes/:id/files/download", h.DownloadFile)
	v1.GET("/sandboxes/:id/files/list", h.ListFiles)

	// Workspace operations
	v1.POST("/sandboxes/:id/workspace/mount", h.MountWorkspace)
	v1.POST("/sandboxes/:id/workspace/unmount", h.UnmountWorkspace)
	v1.POST("/sandboxes/:id/workspace/sync", h.SyncWorkspace)
	v1.GET("/sandboxes/:id/workspace/info", h.GetWorkspaceInfo)

	// One-shot execution
	v1.POST("/execute", h.ExecuteOneShot)
	v1.POST("/execute/stream", h.ExecuteOneShotStream)

	return r
}
