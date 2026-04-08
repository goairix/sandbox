package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/pkg/types"
)

func (h *Handler) MountWorkspace(c *gin.Context) {
	id := c.Param("id")

	var req types.MountWorkspaceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	if err := h.manager.MountWorkspace(c.Request.Context(), id, req.RootPath); err != nil {
		if strings.Contains(err.Error(), "not found") {
			c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
			return
		}
		if strings.Contains(err.Error(), "already mounted") {
			c.JSON(http.StatusConflict, types.ErrorResponse{Message: err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{Message: err.Error()})
		return
	}

	info, _ := h.manager.GetWorkspaceInfo(c.Request.Context(), id)
	c.JSON(http.StatusOK, types.MountWorkspaceResponse{
		RootPath:  info.RootPath,
		MountedAt: info.MountedAt,
	})
}

func (h *Handler) UnmountWorkspace(c *gin.Context) {
	id := c.Param("id")

	if err := h.manager.UnmountWorkspace(c.Request.Context(), id); err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "no workspace mounted") {
			c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{Message: err.Error()})
		return
	}

	c.JSON(http.StatusOK, types.UnmountWorkspaceResponse{Message: "workspace unmounted"})
}

func (h *Handler) SyncWorkspace(c *gin.Context) {
	id := c.Param("id")

	var req types.SyncWorkspaceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	if err := h.manager.SyncWorkspace(c.Request.Context(), id, req.Direction); err != nil {
		if strings.Contains(err.Error(), "not found") {
			c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
			return
		}
		if strings.Contains(err.Error(), "no workspace mounted") {
			c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{Message: err.Error()})
		return
	}

	c.JSON(http.StatusOK, types.SyncWorkspaceResponse{
		Direction: req.Direction,
		Message:   "sync completed",
	})
}

func (h *Handler) GetWorkspaceInfo(c *gin.Context) {
	id := c.Param("id")

	info, err := h.manager.GetWorkspaceInfo(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
		return
	}

	if info == nil {
		c.JSON(http.StatusOK, types.WorkspaceInfoResponse{Mounted: false})
		return
	}

	c.JSON(http.StatusOK, types.WorkspaceInfoResponse{
		Mounted:   true,
		RootPath:  info.RootPath,
		MountedAt: info.MountedAt,
	})
}
