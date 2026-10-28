package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/internal/runtime"
)

type fileV2SearchInfo struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
}

// GlobFilesV2 returns a bounded path search. Truncated is part of the public
// contract: consumers must not treat a partial result as exhaustive.
func (h *Handler) GlobFilesV2(c *gin.Context) {
	var request runtime.FileV2GlobRequest
	if err := decodeFileV2JSON(c.Request.Body, 8192, &request); err != nil {
		fileV2Error(c, err)
		return
	}
	page, err := h.manager.GlobFilesV2(c.Request.Context(), c.Param("id"), request)
	if err != nil {
		fileV2Error(c, err)
		return
	}
	files := make([]fileV2SearchInfo, 0, len(page.Files))
	for _, file := range page.Files {
		files = append(files, fileV2PublicInfo(file))
	}
	c.JSON(http.StatusOK, gin.H{"files": files, "truncated": page.Truncated})
}
