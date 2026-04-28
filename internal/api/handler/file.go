package handler

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/internal/logger"
	"github.com/goairix/sandbox/internal/telemetry/trace"
	"github.com/goairix/sandbox/pkg/types"
)

// validateSandboxPath checks that the path does not contain ".." and starts with
// an allowed prefix (/workspace/ or /tmp/). This prevents path traversal attacks.
func validateSandboxPath(p string) error {
	cleaned := filepath.Clean(p)
	if strings.Contains(cleaned, "..") {
		return fmt.Errorf("path must not contain '..'")
	}
	if !(cleaned == "/workspace" || strings.HasPrefix(cleaned, "/workspace/")) &&
		!(cleaned == "/tmp" || strings.HasPrefix(cleaned, "/tmp/")) {
		return fmt.Errorf("path must start with /workspace/ or /tmp/")
	}
	return nil
}

func (h *Handler) UploadFile(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.UploadFile")
	defer span.End()

	id := c.Param("id")
	destPath := c.DefaultPostForm("path", "/workspace/")

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: "file is required",
		})
		return
	}
	defer file.Close()

	sb, err := h.manager.Get(spanCtx, id)
	if err != nil {
		c.JSON(http.StatusNotFound, types.ErrorResponse{
			Message: err.Error(),
		})
		return
	}
	_ = sb // used to confirm sandbox exists

	// Sanitize filename to prevent path traversal via filename
	safeFilename := filepath.Base(header.Filename)
	if safeFilename == "." || safeFilename == "/" {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: "invalid filename",
		})
		return
	}

	fullPath := destPath
	if fullPath == "" {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: "path must not be empty",
		})
		return
	}
	if fullPath[len(fullPath)-1] == '/' {
		fullPath += safeFilename
	}

	if err := validateSandboxPath(fullPath); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: err.Error(),
		})
		return
	}

	if err := h.manager.UploadFile(spanCtx, id, fullPath, file); err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, types.FileUploadResponse{
		Path: fullPath,
		Size: header.Size,
	})
}

func (h *Handler) DownloadFile(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.DownloadFile")
	defer span.End()

	id := c.Param("id")
	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: "path is required",
		})
		return
	}

	if err := validateSandboxPath(path); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: err.Error(),
		})
		return
	}

	reader, err := h.manager.DownloadFile(spanCtx, id, path)
	if err != nil {
		internalError(c, err)
		return
	}
	defer reader.Close()

	safeName := filepath.Base(path)
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", safeName))
	c.Header("Content-Type", "application/octet-stream")
	if _, err := io.Copy(c.Writer, reader); err != nil {
		logger.Warn(spanCtx, "error copying file to response",
			logger.AddField("sandbox_id", id),
			logger.AddField("path", path),
			logger.ErrorField(err),
		)
	}
}

func (h *Handler) ListFiles(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.ListFiles")
	defer span.End()

	id := c.Param("id")
	dir := c.DefaultQuery("path", "/workspace")

	if err := validateSandboxPath(dir); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: err.Error(),
		})
		return
	}

	files, err := h.manager.ListFiles(spanCtx, id, dir)
	if err != nil {
		internalError(c, err)
		return
	}

	var fileInfos []types.FileInfo
	for _, f := range files {
		fileInfos = append(fileInfos, types.FileInfo{
			Name:    f.Name,
			Path:    f.Path,
			Size:    f.Size,
			IsDir:   f.IsDir,
			ModTime: f.ModTime,
		})
	}

	c.JSON(http.StatusOK, types.FileListResponse{
		Files: fileInfos,
		Path:  dir,
	})
}

func (h *Handler) ListFilesRecursive(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.ListFilesRecursive")
	defer span.End()

	id := c.Param("id")

	var req types.ListFilesRecursiveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	if err := validateSandboxPath(req.Path); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	page := req.Page
	if page < 1 {
		page = 1
	}
	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = 100
	} else if pageSize > 1000 {
		pageSize = 1000
	}

	result, err := h.manager.ListFilesRecursive(spanCtx, id, req.Path, req.MaxDepth, page, pageSize)
	if err != nil {
		internalError(c, err)
		return
	}

	var fileInfos []types.FileInfo
	for _, f := range result.Files {
		fileInfos = append(fileInfos, types.FileInfo{
			Name:    f.Name,
			Path:    f.Path,
			Size:    f.Size,
			IsDir:   f.IsDir,
			ModTime: f.ModTime,
		})
	}

	c.JSON(http.StatusOK, types.ListFilesRecursiveResponse{
		Files:      fileInfos,
		Path:       req.Path,
		TotalCount: result.TotalCount,
		Page:       result.Page,
		PageSize:   result.PageSize,
	})
}

func (h *Handler) ReadFileLines(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.ReadFileLines")
	defer span.End()

	id := c.Param("id")

	var req types.ReadFileLinesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	if err := validateSandboxPath(req.Path); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	result, err := h.manager.ReadFileLines(spanCtx, id, req.Path, req.StartLine, req.EndLine)
	if err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, types.ReadFileLinesResponse{
		Lines:      result.Lines,
		StartLine:  result.StartLine,
		EndLine:    result.EndLine,
		TotalLines: result.TotalLines,
	})
}

func (h *Handler) EditFile(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.EditFile")
	defer span.End()

	id := c.Param("id")

	var req types.EditFileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	if err := validateSandboxPath(req.Path); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	if err := h.manager.EditFile(spanCtx, id, req.Path, req.OldStr, req.NewStr, req.ReplaceAll); err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, types.EditFileResponse{Message: "ok"})
}

func (h *Handler) EditFileLines(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.EditFileLines")
	defer span.End()

	id := c.Param("id")

	var req types.EditFileLinesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	if err := validateSandboxPath(req.Path); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	if err := h.manager.EditFileLines(spanCtx, id, req.Path, req.StartLine, req.EndLine, req.NewContent); err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, types.EditFileResponse{Message: "ok"})
}
