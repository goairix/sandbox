package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/sandbox"
	"github.com/goairix/sandbox/pkg/types"
)

func (h *Handler) ExecuteOneShot(c *gin.Context) {
	var req types.ExecuteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: err.Error(),
		})
		return
	}

	if !isValidLanguage(req.Language) {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: "invalid language, must be one of: python, nodejs, bash",
		})
		return
	}

	ctx := c.Request.Context()

	// Create ephemeral sandbox
	cfg := sandbox.SandboxConfig{
		Mode:    sandbox.ModeEphemeral,
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

	sb, err := h.manager.Create(ctx, cfg)
	if err != nil {
		internalError(c, err)
		return
	}
	// Use context.WithoutCancel so Destroy completes even if client disconnects
	defer h.manager.Destroy(context.WithoutCancel(ctx), sb.ID)

	// Build command from language and code
	command, err := buildCommand(sandbox.Language(req.Language), req.Code)
	if err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: err.Error(),
		})
		return
	}

	// Execute
	result, err := h.manager.Exec(ctx, sb.ID, runtime.ExecRequest{
		Command: command,
		Stdin:   req.Stdin,
		Timeout: req.Timeout,
		Env:     req.Env,
	})
	if err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, types.ExecResponse{
		ExitCode: result.ExitCode,
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
		Duration: result.Duration.Seconds(),
	})
}

func (h *Handler) ExecuteOneShotStream(c *gin.Context) {
	var req types.ExecuteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: err.Error(),
		})
		return
	}

	if !isValidLanguage(req.Language) {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: "invalid language, must be one of: python, nodejs, bash",
		})
		return
	}

	ctx := c.Request.Context()

	cfg := sandbox.SandboxConfig{
		Mode:    sandbox.ModeEphemeral,
		Timeout: req.Timeout,
	}
	if req.Resources != nil {
		cfg.Resources = sandbox.ResourceLimits{
			Memory: req.Resources.Memory,
			CPU:    req.Resources.CPU,
			Disk:   req.Resources.Disk,
		}
	}

	sb, err := h.manager.Create(ctx, cfg)
	if err != nil {
		internalError(c, err)
		return
	}
	// Use context.WithoutCancel so Destroy completes even if client disconnects
	defer h.manager.Destroy(context.WithoutCancel(ctx), sb.ID)

	// Build command from language and code
	command2, err := buildCommand(sandbox.Language(req.Language), req.Code)
	if err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: err.Error(),
		})
		return
	}

	ch, err := h.manager.ExecStream(ctx, sb.ID, runtime.ExecRequest{
		Command: command2,
		Stdin:   req.Stdin,
		Timeout: req.Timeout,
		Env:     req.Env,
	})
	if err != nil {
		internalError(c, err)
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	start := time.Now()
	flusher, _ := c.Writer.(http.Flusher)
	rc := http.NewResponseController(c.Writer)

	// Heartbeat ticker to prevent timeout during silent periods
	heartbeatInterval := 30 * time.Second
	writeDeadline := 2 * heartbeatInterval
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	// Extend write deadline before first wait
	_ = rc.SetWriteDeadline(time.Now().Add(writeDeadline))

	for {
		select {
		case <-c.Request.Context().Done():
			// Client disconnected
			return
		case <-ticker.C:
			// Extend write deadline before sending heartbeat
			_ = rc.SetWriteDeadline(time.Now().Add(writeDeadline))
			// Send heartbeat ping event
			pingData := types.SSEPingData{Timestamp: time.Now().Unix()}
			jsonData, _ := json.Marshal(pingData)
			fmt.Fprintf(c.Writer, "event: ping\ndata: %s\n\n", jsonData)
			if flusher != nil {
				flusher.Flush()
			}
		case event, ok := <-ch:
			if !ok {
				// Channel closed
				return
			}

			var eventType string
			var data any

			switch event.Type {
			case runtime.StreamStdout:
				eventType = "stdout"
				data = types.SSEStdoutData{Content: event.Content}
			case runtime.StreamStderr:
				eventType = "stderr"
				data = types.SSEStderrData{Content: event.Content}
			case runtime.StreamDone:
				eventType = "done"
				exitCode, err := strconv.Atoi(event.Content)
				if err != nil {
					log.Printf("failed to parse exit code %q: %v", event.Content, err)
					exitCode = -1
				}
				data = types.SSEDoneData{
					ExitCode: exitCode,
					Elapsed:  time.Since(start).Seconds(),
				}
			case runtime.StreamError:
				eventType = "error"
				data = types.SSEErrorData{
					Error:   "exec_error",
					Message: event.Content,
				}
			default:
				// Unknown event type, skip
				log.Printf("unknown stream event type: %v", event.Type)
				continue
			}

			// Extend write deadline before sending event
			_ = rc.SetWriteDeadline(time.Now().Add(writeDeadline))

			jsonData, err := json.Marshal(data)
			if err != nil {
				log.Printf("failed to marshal SSE data: %v", err)
				errData := types.SSEErrorData{Error: "marshal_error", Message: "failed to serialize event"}
				jsonData, _ = json.Marshal(errData)
				eventType = "error"
			}
			fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", eventType, jsonData)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}
