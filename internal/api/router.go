package api

import (
	"math"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/internal/api/handler"
	"github.com/goairix/sandbox/internal/api/middleware"
)

// BodySizeLimit returns a middleware that limits the size of request bodies.
func BodySizeLimit(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.ContentLength > maxBytes {
			c.AbortWithStatus(http.StatusRequestEntityTooLarge)
			return
		}
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		}
		c.Next()
	}
}

func routeAwareBodySizeLimit(defaultMaxBytes, uploadMaxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := defaultMaxBytes
		if directUploadRoute(c.Request.Method, c.Request.URL.Path) {
			// Permit bounded multipart framing in addition to the configured file
			// payload. The handler separately enforces the exact declared size.
			if uploadMaxBytes > math.MaxInt64-(1<<20) {
				limit = math.MaxInt64
			} else {
				limit = uploadMaxBytes + (1 << 20)
			}
		}
		BodySizeLimit(limit)(c)
	}
}

func directUploadRoute(method, requestPath string) bool {
	if method != http.MethodPost || strings.HasSuffix(requestPath, "/") {
		return false
	}
	parts := strings.Split(strings.Trim(requestPath, "/"), "/")
	return len(parts) == 6 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "sandboxes" &&
		parts[3] != "" && parts[4] == "files" && parts[5] == "upload"
}

// SetupRouter configures all routes.
func SetupRouter(h *handler.Handler, apiKey string, rateLimit int, serviceName string) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())

	// Health check — registered before OTel middleware so it is never traced
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	r.Use(middleware.OTel(serviceName))

	// Limit multipart memory to 32MB
	r.MaxMultipartMemory = 32 << 20

	// Preserve the 64 MiB cap everywhere except the exact direct streaming
	// upload route, whose payload cap comes from security.max_upload_bytes.
	r.Use(routeAwareBodySizeLimit(64<<20, h.MaxUploadBytes()))

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
	v1.PUT("/sandboxes/:id/ttl", h.UpdateTTL)

	// Execution within a sandbox
	v1.POST("/sandboxes/:id/exec", h.ExecSync)
	v1.POST("/sandboxes/:id/exec/stream", h.ExecStream)

	// File operations
	v1.POST("/sandboxes/:id/files/upload", h.UploadFile)
	v1.GET("/sandboxes/:id/files/download", h.DownloadFile)
	v1.POST("/sandboxes/:id/files/read", h.ReadFile)
	v1.GET("/sandboxes/:id/files/list", h.ListFiles)
	v1.POST("/sandboxes/:id/files/list-recursive", h.ListFilesRecursive)
	v1.POST("/sandboxes/:id/files/glob", h.GlobFiles)
	v1.POST("/sandboxes/:id/files/read-lines", h.ReadFileLines)
	v1.POST("/sandboxes/:id/files/edit", h.EditFile)
	v1.POST("/sandboxes/:id/files/edit-lines", h.EditFileLines)

	// Multipart upload
	v1.POST("/sandboxes/:id/files/upload/init", h.InitMultipartUpload)
	v1.POST("/sandboxes/:id/files/upload/chunk", h.UploadChunk)
	v1.GET("/sandboxes/:id/files/upload/status", h.GetMultipartStatus)
	v1.POST("/sandboxes/:id/files/upload/complete", h.CompleteMultipartUpload)
	v1.DELETE("/sandboxes/:id/files/upload/cancel", h.CancelMultipartUpload)

	// Workspace operations
	v1.POST("/sandboxes/:id/workspace/mount", h.MountWorkspace)
	v1.POST("/sandboxes/:id/workspace/unmount", h.UnmountWorkspace)
	v1.POST("/sandboxes/:id/workspace/sync", h.SyncWorkspace)
	v1.GET("/sandboxes/:id/workspace/info", h.GetWorkspaceInfo)

	// Agent skill discovery
	v1.GET("/sandboxes/:id/skills", h.ListSkills)
	v1.GET("/sandboxes/:id/skills/:name", h.GetSkill)
	v1.GET("/sandboxes/:id/skills/:name/files/*filepath", h.GetSkillFile)

	// One-shot execution
	v1.POST("/execute", h.ExecuteOneShot)
	v1.POST("/execute/stream", h.ExecuteOneShotStream)

	return r
}
