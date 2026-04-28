package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/internal/telemetry/trace"
	"github.com/goairix/sandbox/pkg/types"
)

func (h *Handler) MountWorkspace(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.workspace.MountWorkspace")
	defer span.End()

	id := c.Param("id")

	var req types.MountWorkspaceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	if err := h.manager.MountWorkspace(spanCtx, id, req.RootPath, req.Exclude); err != nil {
		if strings.Contains(err.Error(), "not found") {
			c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
			return
		}
		if strings.Contains(err.Error(), "already mounted") {
			c.JSON(http.StatusConflict, types.ErrorResponse{Message: err.Error()})
			return
		}
		internalError(c, err)
		return
	}

	info, _ := h.manager.GetWorkspaceInfo(spanCtx, id)
	c.JSON(http.StatusOK, types.MountWorkspaceResponse{
		RootPath:  info.RootPath,
		MountedAt: info.MountedAt,
	})
}

func (h *Handler) UnmountWorkspace(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.workspace.UnmountWorkspace")
	defer span.End()

	id := c.Param("id")

	if err := h.manager.UnmountWorkspace(spanCtx, id); err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "no workspace mounted") {
			c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
			return
		}
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, types.UnmountWorkspaceResponse{Message: "workspace unmounted"})
}

func (h *Handler) SyncWorkspace(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.workspace.SyncWorkspace")
	defer span.End()

	id := c.Param("id")

	var req types.SyncWorkspaceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	if err := h.manager.SyncWorkspace(spanCtx, id, req.Direction, req.Exclude); err != nil {
		if strings.Contains(err.Error(), "not found") {
			c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
			return
		}
		if strings.Contains(err.Error(), "no workspace mounted") {
			c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
			return
		}
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, types.SyncWorkspaceResponse{
		Direction: req.Direction,
		Message:   "sync completed",
	})
}

func (h *Handler) GetWorkspaceInfo(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.workspace.GetWorkspaceInfo")
	defer span.End()

	id := c.Param("id")

	info, err := h.manager.GetWorkspaceInfo(spanCtx, id)
	if err != nil {
		c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
		return
	}

	if info == nil {
		c.JSON(http.StatusOK, types.WorkspaceInfoResponse{Mounted: false})
		return
	}

	c.JSON(http.StatusOK, types.WorkspaceInfoResponse{
		Mounted:      true,
		RootPath:     info.RootPath,
		MountedAt:    info.MountedAt,
		LastSyncedAt: info.LastSyncedAt,
	})
}
