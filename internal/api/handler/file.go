package handler

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

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

	sb, err := h.manager.Get(c.Request.Context(), id)
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

	if err := h.manager.UploadFile(c.Request.Context(), id, fullPath, file); err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, types.FileUploadResponse{
		Path: fullPath,
		Size: header.Size,
	})
}

func (h *Handler) DownloadFile(c *gin.Context) {
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

	reader, err := h.manager.DownloadFile(c.Request.Context(), id, path)
	if err != nil {
		internalError(c, err)
		return
	}
	defer reader.Close()

	// Use filepath.Base to prevent Content-Disposition header injection
	safeName := filepath.Base(path)
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", safeName))
	c.Header("Content-Type", "application/octet-stream")
	if _, err := io.Copy(c.Writer, reader); err != nil {
		log.Printf("error copying file to response: %v", err)
	}
}

func (h *Handler) ListFiles(c *gin.Context) {
	id := c.Param("id")
	dir := c.DefaultQuery("path", "/workspace")

	if err := validateSandboxPath(dir); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: err.Error(),
		})
		return
	}

	files, err := h.manager.ListFiles(c.Request.Context(), id, dir)
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
