package handler

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/internal/sandbox"
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

func (h *Handler) CreateSandbox(c *gin.Context) {
	var req types.CreateSandboxRequest
	if err := c.ShouldBindJSON(&req); err != nil {
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

	cfg := sandbox.SandboxConfig{
		Mode:    sandbox.Mode(req.Mode),
		Timeout: req.Timeout,
	}

	if req.Resources != nil {
		cfg.Resources = sandbox.ResourceLimits{
			Memory: req.Resources.Memory,
			CPU:    req.Resources.CPU,
			Disk:   req.Resources.Disk,
		}
	}

	if req.Network != nil {
		cfg.Network = sandbox.NetworkConfig{
			Enabled:   req.Network.Enabled,
			Whitelist: req.Network.Whitelist,
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
	cfg.WorkspaceSyncExclude = req.WorkspaceSyncExclude

	sb, err := h.manager.Create(c.Request.Context(), cfg)
	if err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusCreated, types.SandboxResponse{
		ID:        sb.ID,
		Mode:      string(sb.Config.Mode),
		State:     string(sb.State),
		RuntimeID: sb.RuntimeID,
		CreatedAt: sb.CreatedAt,
	})
}

func (h *Handler) GetSandbox(c *gin.Context) {
	id := c.Param("id")

	sb, err := h.manager.Get(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, types.ErrorResponse{
			Message: err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, types.SandboxResponse{
		ID:        sb.ID,
		Mode:      string(sb.Config.Mode),
		State:     string(sb.State),
		RuntimeID: sb.RuntimeID,
		CreatedAt: sb.CreatedAt,
	})
}

func (h *Handler) DestroySandbox(c *gin.Context) {
	id := c.Param("id")

	if err := h.manager.Destroy(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusNotFound, types.ErrorResponse{
			Message: err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "sandbox destroyed"})
}

func (h *Handler) UpdateNetwork(c *gin.Context) {
	id := c.Param("id")

	var req types.UpdateNetworkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: err.Error(),
		})
		return
	}

	if err := h.manager.UpdateNetwork(c.Request.Context(), id, req.Enabled, req.Whitelist); err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, types.UpdateNetworkResponse{
		Enabled:   req.Enabled,
		Whitelist: req.Whitelist,
	})
}

// buildCommand wraps raw code with the appropriate interpreter command based on language.
func buildCommand(lang sandbox.Language, code string) (string, error) {
	switch lang {
	case sandbox.LangPython:
		return fmt.Sprintf("python3 <<'SANDBOX_EOF'\n%s\nSANDBOX_EOF", code), nil
	case sandbox.LangNodeJS:
		return fmt.Sprintf("node <<'SANDBOX_EOF'\n%s\nSANDBOX_EOF", code), nil
	case sandbox.LangBash:
		return code, nil
	default:
		return "", fmt.Errorf("unsupported language: %s", lang)
	}
}
