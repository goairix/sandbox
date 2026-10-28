package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/pkg/types"
)

func fileV2PublicInfo(info runtime.FileInfo) fileV2SearchInfo {
	return fileV2SearchInfo{
		Name: info.Name, Path: info.Path, Size: info.Size,
		IsDir: info.IsDir, ModTime: info.ModTime, Mode: info.Mode,
		Nlink: info.Nlink, UID: info.UID, GID: info.GID,
	}
}

func (h *Handler) StatFileV2(c *gin.Context) {
	filePath := c.Query("path")
	if filePath == "" {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Code: "FILE_INVALID", Message: "path is required"})
		return
	}
	info, err := h.manager.StatFileV2(c.Request.Context(), c.Param("id"), filePath)
	if err != nil {
		fileV2Error(c, err)
		return
	}
	c.JSON(http.StatusOK, fileV2PublicInfo(info))
}

func (h *Handler) ListFilesV2(c *gin.Context) {
	limit := 0
	if raw := c.Query("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 1000 {
			c.JSON(http.StatusBadRequest, types.ErrorResponse{Code: "FILE_INVALID", Message: "invalid list limit"})
			return
		}
		limit = parsed
	}
	page, err := h.manager.ListFilesV2(c.Request.Context(), c.Param("id"), c.Query("path"), c.Query("cursor"), limit)
	if err != nil {
		fileV2Error(c, err)
		return
	}
	files := make([]fileV2SearchInfo, 0, len(page.Files))
	for _, info := range page.Files {
		files = append(files, fileV2PublicInfo(info))
	}
	c.JSON(http.StatusOK, gin.H{"files": files, "next_cursor": page.NextCursor})
}
