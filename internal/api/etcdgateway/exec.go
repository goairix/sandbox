package etcdgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/goairix/sandbox/internal/execcode"
	"github.com/goairix/sandbox/internal/provisioning"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/goairix/sandbox/pkg/types"
	"github.com/google/uuid"
)

func (g *Gateway) exec(w http.ResponseWriter, r *http.Request, id string, stream bool) {
	for name := range r.Header {
		if strings.EqualFold(name, "Idempotency-Key") {
			writeError(w, 400, "invalid_request", "Idempotency-Key is not supported for execution")
			return
		}
	}
	requestID, valid := validHeaderValue(r.Header, "X-Request-ID", 128, true)
	if !valid {
		writeError(w, 400, "invalid_request", "invalid X-Request-ID")
		return
	}
	if requestID == "" {
		requestID = uuid.NewString()
	}
	w.Header().Set("X-Request-ID", requestID)
	var req types.ExecRequest
	if !readRequest(w, r, &req) {
		return
	}
	command, err := execcode.BuildCommand(req.Language, req.Code)
	if err != nil || req.Code == "" || req.LineBuffered || req.Timeout < 0 || req.Timeout > g.settings.MaxExecTimeoutSeconds || req.RequiresNetwork && !g.settings.NetworkAllowed {
		writeError(w, 400, "unsupported_profile", "unsupported execution profile")
		return
	}
	timeout := req.Timeout
	if timeout == 0 {
		timeout = g.settings.ExecTimeoutSeconds
	}
	env := make(map[string]string, len(g.settings.Env)+len(req.Env))
	for name, value := range g.settings.Env {
		env[name] = value
	}
	for name, value := range req.Env {
		env[name] = value
	}
	resolved := p.ExecutionRequest{Argv: []string{"/bin/sh", "-c", command}, Env: env, UID: g.settings.UID, GID: g.settings.GID, WorkDir: "/workspace", TimeoutSeconds: uint32(timeout), Stdin: []byte(req.Stdin), RequiresNetwork: req.RequiresNetwork}
	if _, err := p.NewExecutionDescriptor(resolved); err != nil {
		writeError(w, 400, "invalid_request", "invalid execution descriptor")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(timeout)*time.Second+30*time.Second)
	defer cancel()
	if stream {
		g.execStream(w, ctx, cancel, id, requestID, resolved)
	} else {
		g.execSync(w, ctx, cancel, id, requestID, resolved)
	}
}

type outputBudget struct {
	mu       sync.Mutex
	used     int
	overflow bool
	cancel   context.CancelFunc
}

type boundedOutput struct {
	budget *outputBudget
	buffer bytes.Buffer
}

func (w *boundedOutput) Write(data []byte) (int, error) {
	w.budget.mu.Lock()
	defer w.budget.mu.Unlock()
	if w.budget.overflow || len(data) > (1<<20)-w.budget.used {
		w.budget.overflow = true
		w.budget.cancel()
		return 0, errors.New("execution output limit")
	}
	w.budget.used += len(data)
	return w.buffer.Write(data)
}

func terminalExit(result provisioning.PlainExecResult, err error) (int, bool) {
	if err != nil || result.State != provisioning.PlainExecLocalTerminal || result.Receipt.State() != "local_terminal" || !result.Receipt.DrainConfirmed() {
		return 0, false
	}
	status := result.Receipt.RootWaitStatus()
	if status <= 0xffff && status&0xff == 0 {
		return int(status >> 8), true
	}
	if status&0xffffff00 == 0 && status&0x7f > 0 && status&0x7f < 0x7f {
		return 128 + int(status&0x7f), true
	}
	return 0, false
}

type unknownResponse struct {
	Reason    string                     `json:"reason"`
	Code      string                     `json:"code"`
	Message   string                     `json:"message"`
	Operation *state.OperationReference  `json:"operation,omitempty"`
	Effect    *state.ExecEffectReference `json:"effect,omitempty"`
}

func executionUnknown(result provisioning.PlainExecResult, err error) unknownResponse {
	reason := "outcome_unknown"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		reason = "deadline_exceeded"
	case errors.Is(err, state.ErrGuardExpired):
		reason = "guard_expired"
	case errors.Is(err, state.ErrConflict):
		reason = "conflict"
	case errors.Is(err, context.Canceled):
		reason = "canceled"
	}
	response := unknownResponse{Code: "exec_unknown", Message: "execution outcome unknown; do not automatically retry", Reason: reason}
	if result.Operation.OperationID != "" {
		operation := result.Operation
		response.Operation = &operation
	}
	if result.Effect.CommandID != "" {
		effect := result.Effect
		response.Effect = &effect
	}
	return response
}

func (g *Gateway) execSync(w http.ResponseWriter, ctx context.Context, cancel context.CancelFunc, id, requestID string, request p.ExecutionRequest) {
	budget := &outputBudget{cancel: cancel}
	stdout, stderr := &boundedOutput{budget: budget}, &boundedOutput{budget: budget}
	start := time.Now()
	result, err := g.service.Exec(ctx, id, requestID, request, stdout, stderr)
	if exit, ok := terminalExit(result, err); ok && !budget.overflow {
		writeJSON(w, 200, struct {
			types.ExecResponse
			Reason string `json:"reason"`
		}{types.ExecResponse{ExitCode: exit, Stdout: stdout.buffer.String(), Stderr: stderr.buffer.String(), Duration: time.Since(start).Seconds()}, result.Receipt.Reason()})
		return
	}
	if result.State == provisioning.PlainExecRejected && err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, 502, executionUnknown(result, err))
}

type streamChunk struct {
	event string
	data  []byte
}

type streamOutput struct {
	ctx   context.Context
	queue chan<- streamChunk
	event string
}

func (w streamOutput) Write(data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		if err := w.ctx.Err(); err != nil {
			return written, err
		}
		n := len(data)
		if n > 32<<10 {
			n = 32 << 10
		}
		chunk := streamChunk{event: w.event, data: bytes.Clone(data[:n])}
		select {
		case <-w.ctx.Done():
			return written, w.ctx.Err()
		case w.queue <- chunk:
			written += n
			data = data[n:]
		}
	}
	return written, nil
}

type streamAnswer struct {
	result provisioning.PlainExecResult
	err    error
}

func sendEvent(w http.ResponseWriter, rc *http.ResponseController, event string, value any) error {
	if err := rc.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	wire, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, wire); err != nil {
		return err
	}
	return rc.Flush()
}

func (g *Gateway) execStream(w http.ResponseWriter, ctx context.Context, cancel context.CancelFunc, id, requestID string, request p.ExecutionRequest) {
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		writeError(w, 503, "stream_unavailable", "socket write deadline unavailable")
		return
	}
	// Flush support is required before issuing Exec. A successful first Flush
	// commits SSE headers, after which every error uses the SSE error shape.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	if err := rc.Flush(); err != nil {
		return
	}
	queue := make(chan streamChunk, 16)
	joined := make(chan streamAnswer, 1)
	go func() {
		result, err := g.service.Exec(ctx, id, requestID, request, streamOutput{ctx, queue, "stdout"}, streamOutput{ctx, queue, "stderr"})
		close(queue)
		joined <- streamAnswer{result, err}
	}()
	defer func() {
		cancel()
		// Reading the original result, rather than observing cancellation, joins
		// the original service Wait/renewal/IO owner and all queue writers.
		<-joined
	}()
	start := time.Now()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := sendEvent(w, rc, "ping", types.SSEPingData{Timestamp: time.Now().Unix()}); err != nil {
				return
			}
		case chunk, ok := <-queue:
			if !ok {
				answer := <-joined
				// Put the consumed result back for the single deferred owner join.
				joined <- answer
				if exit, terminal := terminalExit(answer.result, answer.err); terminal {
					if err := sendEvent(w, rc, "done", struct {
						types.SSEDoneData
						Reason string `json:"reason"`
					}{types.SSEDoneData{ExitCode: exit, Elapsed: time.Since(start).Seconds()}, answer.result.Receipt.Reason()}); err != nil {
						return
					}
				} else if answer.result.State == provisioning.PlainExecRejected {
					failure := classifyServiceError(answer.err)
					if err := sendEvent(w, rc, "error", struct {
						Error      string `json:"error"`
						Code       string `json:"code"`
						State      string `json:"state"`
						Reason     string `json:"reason"`
						Message    string `json:"message"`
						RetryAfter int    `json:"retry_after,omitempty"`
					}{failure.code, failure.code, "rejected", failure.code, failure.message, failure.retryAfter}); err != nil {
						return
					}
				} else {
					failure := executionUnknown(answer.result, answer.err)
					if err := sendEvent(w, rc, "error", struct {
						Error string `json:"error"`
						unknownResponse
					}{"exec_unknown", failure}); err != nil {
						return
					}
				}
				return
			}
			if err := sendEvent(w, rc, chunk.event, types.SSEStdoutData{Content: string(chunk.data)}); err != nil {
				return
			}
		}
	}
}
