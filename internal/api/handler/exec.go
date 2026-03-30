package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/pkg/types"
)

func (h *Handler) ExecSync(c *gin.Context) {
	id := c.Param("id")

	var req types.ExecRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Error:   "invalid_request",
			Message: err.Error(),
		})
		return
	}

	execReq := runtime.ExecRequest{
		Command: req.Command,
		Stdin:   req.Stdin,
		Timeout: req.Timeout,
		Env:     req.Env,
	}

	result, err := h.manager.Exec(c.Request.Context(), id, execReq)
	if err != nil {
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{
			Error:   "exec_failed",
			Message: err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, types.ExecResponse{
		ExitCode: result.ExitCode,
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
		Duration: result.Duration.Seconds(),
	})
}

func (h *Handler) ExecStream(c *gin.Context) {
	id := c.Param("id")

	var req types.ExecRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Error:   "invalid_request",
			Message: err.Error(),
		})
		return
	}

	execReq := runtime.ExecRequest{
		Command: req.Command,
		Stdin:   req.Stdin,
		Timeout: req.Timeout,
		Env:     req.Env,
	}

	ch, err := h.manager.ExecStream(c.Request.Context(), id, execReq)
	if err != nil {
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{
			Error:   "exec_failed",
			Message: err.Error(),
		})
		return
	}

	// SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	start := time.Now()
	flusher, _ := c.Writer.(http.Flusher)

	for event := range ch {
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
			exitCode, _ := strconv.Atoi(event.Content)
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
		}

		jsonData, _ := json.Marshal(data)
		fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", eventType, jsonData)
		if flusher != nil {
			flusher.Flush()
		}
	}
}
