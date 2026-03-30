package handler

import (
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/pkg/types"
)

func (h *Handler) UploadFile(c *gin.Context) {
	id := c.Param("id")
	destPath := c.DefaultPostForm("path", "/workspace/")

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Error:   "invalid_request",
			Message: "file is required",
		})
		return
	}
	defer file.Close()

	sb, err := h.manager.Get(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, types.ErrorResponse{
			Error:   "not_found",
			Message: err.Error(),
		})
		return
	}
	_ = sb // used to confirm sandbox exists

	fullPath := destPath
	if fullPath[len(fullPath)-1] == '/' {
		fullPath += header.Filename
	}

	if err := h.manager.UploadFile(c.Request.Context(), id, fullPath, file); err != nil {
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{
			Error:   "upload_failed",
			Message: err.Error(),
		})
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
			Error:   "invalid_request",
			Message: "path is required",
		})
		return
	}

	reader, err := h.manager.DownloadFile(c.Request.Context(), id, path)
	if err != nil {
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{
			Error:   "download_failed",
			Message: err.Error(),
		})
		return
	}
	defer reader.Close()

	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path))
	c.Header("Content-Type", "application/octet-stream")
	io.Copy(c.Writer, reader)
}

func (h *Handler) ListFiles(c *gin.Context) {
	id := c.Param("id")
	dir := c.DefaultQuery("path", "/workspace")

	files, err := h.manager.ListFiles(c.Request.Context(), id, dir)
	if err != nil {
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{
			Error:   "list_failed",
			Message: err.Error(),
		})
		return
	}

	var fileInfos []types.FileInfo
	for _, f := range files {
		fileInfos = append(fileInfos, types.FileInfo{
			Name:  f.Name,
			Path:  f.Path,
			Size:  f.Size,
			IsDir: f.IsDir,
		})
	}

	c.JSON(http.StatusOK, types.FileListResponse{
		Files: fileInfos,
		Path:  dir,
	})
}
