package etcdgateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/goairix/sandbox/internal/provisioning"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/sandboxfiles"
	"github.com/goairix/sandbox/internal/transferspool"
)

func downloadDeadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(10 * time.Second)
	if total, ok := ctx.Deadline(); ok && total.Before(deadline) {
		deadline = total
	}
	return deadline
}

func (g *Gateway) download(w http.ResponseWriter, r *http.Request, id, requestID, filename string) {
	settings := g.settings.FileTransfer
	request := sandboxfiles.DownloadRequest{Path: filename, MaxBytes: settings.MaxFileBytes}
	stdin, err := json.Marshal(request)
	if err != nil || sandboxfiles.ValidateDownload(request) != nil || len(stdin) > sandboxfiles.MaxDownloadInputBytes {
		writeError(w, 400, "invalid_request", "invalid download request")
		return
	}
	resolved := p.ExecutionRequest{Argv: []string{"/usr/local/bin/sandbox-launcher", "file-download"}, Env: g.settings.Env, UID: g.settings.UID, GID: g.settings.GID, WorkDir: "/workspace", TimeoutSeconds: uint32(settings.TimeoutSeconds), Stdin: stdin, RequiresNetwork: false}
	descriptor, err := p.NewExecutionDescriptor(resolved)
	if err != nil {
		writeError(w, 400, "invalid_request", "invalid download execution descriptor")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(settings.TimeoutSeconds)*time.Second)
	defer cancel()
	controller := http.NewResponseController(w)
	// Capability must be real before disk admission or the original execution.
	// Clear the initial deadline while staging: no response bytes exist yet.
	if err := controller.SetWriteDeadline(downloadDeadline(ctx)); err != nil {
		writeError(w, 503, "download_unavailable", "socket write deadline unavailable")
		return
	}
	if err := controller.SetWriteDeadline(time.Time{}); err != nil {
		writeError(w, 503, "download_unavailable", "socket write deadline unavailable")
		return
	}
	job, err := g.transfers.Reserve(ctx)
	if err != nil {
		status := 503
		if errors.Is(err, transferspool.ErrCapacity) {
			status = 429
		}
		w.Header().Set("Retry-After", "1")
		writeError(w, status, "download_unavailable", "download capacity unavailable")
		return
	}
	// The synchronous original Exec return physically joins writers/Wait/IO.
	// This owner releases the job only after delivery also physically returns.
	defer func() { _ = job.Close() }()
	budget := &fileOutputBudget{cancel: cancel}
	stderr := &fileOutput{budget: budget, limit: sandboxfiles.MaxStderrBytes}
	result, execErr := g.service.Exec(job.Context(), id, requestID, descriptor.Request(), job, stderr)
	if budget.overflow {
		writeJSON(w, 502, executionUnknown(result, execErr))
		return
	}
	if exit, terminal := terminalExit(result, execErr); terminal {
		if exit == 1 {
			failure, decodeErr := sandboxfiles.DecodeDownloadFailure(stderr.buffer.Bytes())
			if decodeErr == nil {
				status, message := sandboxfiles.ErrorStatus(failure.Code)
				writeError(w, status, failure.Code, message)
				return
			}
		}
		if exit != 0 {
			writeError(w, 502, "file_helper_failed", "file helper failed; do not automatically retry")
			return
		}
		size, sealErr := job.Seal()
		if sealErr != nil {
			writeError(w, 502, "file_result_invalid", "download result unavailable; do not automatically retry")
			return
		}
		deliverDownload(job.Context(), w, controller, job, filename, size)
		return
	}
	if result.State == provisioning.PlainExecRejected && execErr != nil {
		writeServiceError(w, execErr)
		return
	}
	writeJSON(w, 502, executionUnknown(result, execErr))
}

func deliverDownload(ctx context.Context, w http.ResponseWriter, controller *http.ResponseController, job *transferspool.Job, filename string, size int64) {
	if ctx.Err() != nil {
		writeError(w, 503, "download_unavailable", "download delivery unavailable")
		return
	}
	if err := controller.SetWriteDeadline(downloadDeadline(ctx)); err != nil {
		writeError(w, 503, "download_unavailable", "download delivery unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(filename)}))
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	var buffer [32 << 10]byte
	remaining := size
	for remaining > 0 {
		if ctx.Err() != nil {
			return
		}
		next := int64(len(buffer))
		if remaining < next {
			next = remaining
		}
		n, err := io.ReadFull(job, buffer[:int(next)])
		if err != nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		if err := controller.SetWriteDeadline(downloadDeadline(ctx)); err != nil {
			return
		}
		written, err := w.Write(buffer[:n])
		if err != nil || written != n {
			return
		}
		remaining -= int64(n)
	}
}
