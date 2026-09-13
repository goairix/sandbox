package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"

	"github.com/goairix/sandbox/internal/sandbox"
	"github.com/goairix/sandbox/internal/telemetry/trace"
	"github.com/goairix/sandbox/pkg/types"
)

// isValidLanguage checks whether the language string is a known value.
func isValidLanguage(lang string) bool {
	switch sandbox.Language(lang) {
	case sandbox.LangPython, sandbox.LangNodeJS, sandbox.LangBash:
		return true
	}
	return false
}

// isValidMode checks whether the mode string is a known value.
func isValidMode(mode string) bool {
	switch sandbox.Mode(mode) {
	case sandbox.ModeEphemeral, sandbox.ModePersistent:
		return true
	}
	return false
}

func resourceLimitsFromRequest(req *types.ResourceLimits) sandbox.ResourceLimits {
	if req == nil {
		return sandbox.ResourceLimits{}
	}
	return sandbox.ResourceLimits{
		Memory:  req.Memory,
		CPU:     req.CPU,
		Disk:    req.Disk,
		TmpDisk: req.TmpDisk,
	}
}

func (h *Handler) CreateSandbox(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.sandbox.CreateSandbox")
	defer span.End()

	var req types.CreateSandboxRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		var validationErrors validator.ValidationErrors
		if errors.As(err, &validationErrors) {
			for _, fieldError := range validationErrors {
				if fieldError.StructField() == "WorkspaceMountMode" {
					internalError(c, fmt.Errorf("workspace_mount_mode must be sync or fuse: %w", sandbox.ErrInvalidWorkspaceMountMode))
					return
				}
			}
		}
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: err.Error(),
		})
		return
	}

	if !isValidMode(req.Mode) {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: "invalid mode, must be one of: ephemeral, persistent",
		})
		return
	}
	if req.WorkspaceMountMode != "" && req.WorkspacePath == "" {
		internalError(c, fmt.Errorf("workspace_mount_mode requires workspace_path: %w", sandbox.ErrInvalidWorkspaceMountMode))
		return
	}

	cfg := sandbox.SandboxConfig{
		Mode:    sandbox.Mode(req.Mode),
		Timeout: req.Timeout,
	}

	if req.Resources != nil {
		cfg.Resources = resourceLimitsFromRequest(req.Resources)
	}

	if req.Network != nil {
		cfg.Network = sandbox.NetworkConfig{
			Enabled:      req.Network.Enabled,
			Whitelist:    req.Network.Whitelist,
			BlockPrivate: req.Network.BlockPrivate,
		}
	}

	for _, dep := range req.Dependencies {
		cfg.Dependencies = append(cfg.Dependencies, sandbox.Dependency{
			Name:    dep.Name,
			Version: dep.Version,
			Manager: dep.Manager,
		})
	}

	cfg.WorkspacePath = req.WorkspacePath
	cfg.WorkspaceMountMode = sandbox.WorkspaceMountMode(req.WorkspaceMountMode)
	cfg.WorkspaceSyncExclude = req.WorkspaceSyncExclude

	sb, err := h.manager.Create(spanCtx, cfg)
	if err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusCreated, sandboxToResponse(sb))
}

func (h *Handler) GetSandbox(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.sandbox.GetSandbox")
	defer span.End()

	id := c.Param("id")

	sb, err := h.manager.Get(spanCtx, id)
	if err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, sandboxToResponse(&sb))
}

func (h *Handler) DestroySandbox(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.sandbox.DestroySandbox")
	defer span.End()

	id := c.Param("id")

	if err := h.manager.Destroy(spanCtx, id); err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "sandbox destroyed"})
}

func (h *Handler) UpdateNetwork(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.sandbox.UpdateNetwork")
	defer span.End()

	id := c.Param("id")

	var req types.UpdateNetworkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: err.Error(),
		})
		return
	}

	if err := h.manager.UpdateNetwork(spanCtx, id, req.Enabled, req.Whitelist, req.BlockPrivate); err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, types.UpdateNetworkResponse{
		Enabled:      req.Enabled,
		Whitelist:    req.Whitelist,
		BlockPrivate: req.BlockPrivate,
	})
}

// UpdateTTL dynamically updates the TTL for a running sandbox.
func (h *Handler) UpdateTTL(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.sandbox.UpdateTTL")
	defer span.End()

	id := c.Param("id")

	var req types.UpdateTTLRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: err.Error(),
		})
		return
	}

	sb, err := h.manager.UpdateTTL(spanCtx, id, req.Timeout)
	if err != nil {
		internalError(c, err)
		return
	}

	expiresAt := sb.CreatedAt.Add(sb.Timeout)
	c.JSON(http.StatusOK, types.UpdateTTLResponse{
		Timeout:   int(sb.Timeout.Seconds()),
		ExpiresAt: expiresAt,
	})
}

// sandboxToResponse converts a Sandbox to a SandboxResponse.
func sandboxToResponse(sb *sandbox.Sandbox) types.SandboxResponse {
	resp := types.SandboxResponse{
		ID:        sb.ID,
		Mode:      string(sb.Config.Mode),
		State:     string(sb.State),
		RuntimeID: sb.RuntimeID,
		CreatedAt: sb.CreatedAt,
		Timeout:   int(sb.Timeout.Seconds()),
	}
	if sb.Timeout > 0 {
		expiresAt := sb.CreatedAt.Add(sb.Timeout)
		resp.ExpiresAt = &expiresAt
	}
	if sb.Workspace != nil {
		mountMode := sb.Config.WorkspaceMountMode
		if mountMode == "" {
			mountMode = sandbox.WorkspaceMountSync
			if sb.Workspace.MountType == sandbox.WorkspaceMountFUSE {
				mountMode = sandbox.WorkspaceMountFUSE
			}
		}
		resp.WorkspaceMountMode = string(mountMode)
	}
	return resp
}

// buildCommand wraps raw code with the appropriate interpreter command based on language.
func buildCommand(lang sandbox.Language, code string) (string, error) {
	switch lang {
	case sandbox.LangPython:
		return "python3 -c " + quoteShellArgument(code), nil
	case sandbox.LangNodeJS:
		return "node -e " + quoteShellArgument(code), nil
	case sandbox.LangBash:
		return code, nil
	default:
		return "", fmt.Errorf("unsupported language: %s", lang)
	}
}

func quoteShellArgument(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
