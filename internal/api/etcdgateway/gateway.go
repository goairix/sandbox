// Package etcdgateway exposes the bounded, fixed-profile etcd service over v1 HTTP.
package etcdgateway

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/goairix/sandbox/internal/provisioning"
	"github.com/goairix/sandbox/internal/provisioning/deployment"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/sandboxsnapshot"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/goairix/sandbox/internal/transferspool"
	"github.com/goairix/sandbox/pkg/types"
	"github.com/google/uuid"
)

type Service interface {
	CreateBusiness(context.Context, provisioning.PlainColdBusinessInput) (provisioning.PlainColdBusinessResult, error)
	Lookup(context.Context, string) (*provisioning.PlainColdLookupResult, error)
	Exec(context.Context, string, string, p.ExecutionRequest, io.Writer, io.Writer) (provisioning.PlainExecResult, error)
	// Destroy admits asynchronous cleanup for one exact original target. Its
	// result diagnoses metadata admission, never physical cleanup completion.
	Destroy(context.Context, string, string) (provisioning.PlainColdDestroyResult, error)
	Ready(context.Context) error
}

// ttlService is optional until a service supports the complete trusted runtime
// credential renewal and metadata commit. A nil error promises that both have
// completed; updating display metadata alone must never implement this contract.
type ttlService interface {
	UpdateTTL(context.Context, string, string, int) (types.UpdateTTLResponse, error)
}

type Gateway struct {
	service   Service
	settings  deployment.HTTPSettings
	mu        sync.Mutex
	closed    bool
	active    map[*gatewayCall]struct{}
	lanes     [3]int
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
	transfers *transferspool.Store
	tokens    float64
	lastToken time.Time
}

type gatewayCall struct {
	ctx    context.Context
	cancel context.CancelFunc
	lane   admissionLane
}

type admissionLane uint8

const (
	businessLane admissionLane = iota
	healthLane
	readyLane
)

func New(service Service, settings *deployment.HTTPSettings) (*Gateway, error) {
	if service == nil || (reflect.ValueOf(service).Kind() == reflect.Pointer && reflect.ValueOf(service).IsNil()) || settings == nil || len(settings.APIKey) == 0 || len(settings.APIKey) > 4096 || settings.Env == nil || settings.ExecTimeoutSeconds < 1 || settings.ExecTimeoutSeconds > settings.MaxExecTimeoutSeconds || settings.MaxExecTimeoutSeconds > 3600 || settings.MaxActive < 1 || settings.MaxActive > 32 || settings.RequestsPerSecond < 1 || settings.RequestsPerSecond > 10000 {
		return nil, errors.New("invalid HTTP gateway settings")
	}
	for _, c := range settings.APIKey {
		if c < 33 || c > 126 {
			return nil, errors.New("invalid HTTP gateway key")
		}
	}
	if _, err := p.NewExecutionDescriptor(p.ExecutionRequest{Argv: []string{"/bin/sh"}, Env: settings.Env, UID: settings.UID, GID: settings.GID, WorkDir: "/workspace", TimeoutSeconds: uint32(settings.ExecTimeoutSeconds)}); err != nil {
		return nil, errors.New("invalid HTTP execution profile")
	}
	owned := *settings
	owned.APIKey = bytes.Clone(settings.APIKey)
	owned.Env = make(map[string]string, len(settings.Env))
	for name, value := range settings.Env {
		owned.Env[name] = value
	}
	var transfers *transferspool.Store
	if settings.FileTransfer != nil {
		transfer := *settings.FileTransfer
		transfer.Directory = strings.Clone(transfer.Directory)
		owned.FileTransfer = &transfer
		options := transferspool.Options{Directory: transfer.Directory, MaxFileBytes: transfer.MaxFileBytes, MaxTotalBytes: transfer.MaxTotalBytes, MaxActive: transfer.MaxActive}
		if options.Validate() != nil || transfer.TimeoutSeconds < 1 || transfer.TimeoutSeconds > settings.MaxExecTimeoutSeconds {
			owned.Zero()
			return nil, errors.New("invalid HTTP file transfer settings")
		}
		var err error
		transfers, err = transferspool.Open(options)
		if err != nil {
			owned.Zero()
			return nil, errors.New("HTTP file transfer directory unavailable")
		}
	}
	return &Gateway{service: service, settings: owned, transfers: transfers, active: make(map[*gatewayCall]struct{}), tokens: float64(owned.RequestsPerSecond), lastToken: time.Now()}, nil
}

func (g *Gateway) enter(ctx context.Context, lane admissionLane) (*gatewayCall, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, 503
	}
	limit, fullStatus := g.settings.MaxActive, 429
	switch lane {
	case healthLane:
		limit, fullStatus = 4, 503
	case readyLane:
		limit, fullStatus = 2, 503
	}
	if g.lanes[lane] >= limit {
		return nil, fullStatus
	}
	call := &gatewayCall{lane: lane}
	call.ctx, call.cancel = context.WithCancel(ctx)
	g.active[call] = struct{}{}
	g.lanes[lane]++
	g.wg.Add(1)
	return call, 0
}

func (g *Gateway) leave(call *gatewayCall) {
	call.cancel()
	g.mu.Lock()
	delete(g.active, call)
	g.lanes[call.lane]--
	g.mu.Unlock()
	g.wg.Done()
}

func (g *Gateway) stop() {
	g.mu.Lock()
	g.closed = true
	for call := range g.active {
		call.cancel()
	}
	g.mu.Unlock()
}

// Close cancels and physically joins admitted handlers, including their original
// Exec writers. Serve additionally owns socket closure to release HTTP writes.
func (g *Gateway) Close() error {
	g.closeOnce.Do(func() {
		g.stop()
		g.wg.Wait()
		if g.transfers != nil {
			g.closeErr = g.transfers.Close()
		}
		g.settings.Zero()
	})
	return g.closeErr
}

func (g *Gateway) allow() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	g.tokens += now.Sub(g.lastToken).Seconds() * float64(g.settings.RequestsPerSecond)
	if limit := float64(g.settings.RequestsPerSecond); g.tokens > limit {
		g.tokens = limit
	}
	g.lastToken = now
	if g.tokens < 1 {
		return false
	}
	g.tokens--
	return true
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	lane := businessLane
	if r.Method == http.MethodGet {
		switch r.URL.Path {
		case "/health":
			lane = healthLane
		case "/ready":
			lane = readyLane
		}
	}
	call, status := g.enter(r.Context(), lane)
	if call == nil {
		w.Header().Set("Retry-After", "1")
		writeError(w, status, "unavailable", "HTTP admission unavailable")
		return
	}
	defer g.leave(call)
	r = r.WithContext(call.ctx)
	if r.Method == http.MethodGet && r.URL.Path == "/health" {
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/ready" {
		if g.transfers != nil && g.transfers.Err() != nil {
			writeJSON(w, 503, map[string]string{"status": "unavailable"})
			return
		}
		ctx, cancel := context.WithTimeout(call.ctx, 2*time.Second)
		defer cancel()
		if err := g.service.Ready(ctx); err != nil {
			writeJSON(w, 503, map[string]string{"status": "unavailable"})
		} else {
			writeJSON(w, 200, map[string]string{"status": "ok"})
		}
		return
	}
	if r.URL.Path != "/api/v1" && !strings.HasPrefix(r.URL.Path, "/api/v1/") {
		writeError(w, 404, "not_found", "route not found")
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(r.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare([]byte(token), g.settings.APIKey) != 1 {
		writeError(w, 401, "unauthorized", "invalid API key")
		return
	}
	if !g.allow() {
		w.Header().Set("Retry-After", "1")
		writeError(w, 429, "rate_limit_exceeded", "too many requests")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
	if len(parts) == 1 && parts[0] == "sandboxes" && r.Method == http.MethodPost {
		g.create(w, r)
		return
	}
	if len(parts) >= 2 && parts[0] == "sandboxes" && parts[1] != "by-workspace" {
		file := len(parts) == 4 && parts[2] == "files" && fileRoute(r.Method, parts[3])
		ttl := len(parts) == 3 && parts[2] == "ttl" && r.Method == http.MethodPut
		supported := file || ttl || len(parts) == 2 && (r.Method == http.MethodGet || r.Method == http.MethodDelete) || r.Method == http.MethodPost && (len(parts) == 3 && parts[2] == "exec" || len(parts) == 4 && parts[2] == "exec" && parts[3] == "stream")
		if supported {
			id := parts[1]
			parsed, err := uuid.Parse(id)
			if err != nil || parsed == uuid.Nil || parsed.String() != id {
				writeError(w, 400, "invalid_request", "invalid sandbox id")
				return
			}
			if ttl {
				g.updateTTL(w, r, id)
			} else if len(parts) == 2 && r.Method == http.MethodDelete {
				g.destroy(w, r, id)
			} else if len(parts) == 2 {
				g.lookup(w, r, id)
			} else if file {
				g.files(w, r, id, parts[3])
			} else {
				g.exec(w, r, id, len(parts) == 4)
			}
			return
		}
	}
	writeError(w, 501, "unsupported", "route is not implemented in etcd mode")
}

func validHeaderValue(header http.Header, name string, max int, optional bool) (string, bool) {
	var values []string
	present := false
	for field, entries := range header {
		if strings.EqualFold(field, name) {
			present = true
			values = append(values, entries...)
		}
	}
	if !present {
		return "", optional
	}
	if len(values) != 1 || len(values[0]) == 0 || len(values[0]) > max {
		return "", false
	}
	for _, c := range []byte(values[0]) {
		if c < 33 || c > 126 {
			return "", false
		}
	}
	return values[0], true
}

func validCreate(req types.CreateSandboxRequest, networkAllowed bool) bool {
	if req.Mode != "ephemeral" && req.Mode != "persistent" || req.Timeout < 0 || req.Timeout > 365*24*60*60 || req.WorkspacePath != "" || req.WorkspaceMountMode != "" || len(req.WorkspaceSyncExclude) != 0 || len(req.Dependencies) != 0 {
		return false
	}
	network := req.Network
	if network == nil && networkAllowed || network != nil && (network.Enabled != networkAllowed || len(network.Whitelist) != 0 || network.BlockPrivate) {
		return false
	}
	// Validate requested scalars with the original business parser. These probe
	// defaults only fill omitted values for validation; the original Service
	// alone resolves/persists operator defaults, including same-key replay.
	_, err := sandboxsnapshot.Build(req, sandboxsnapshot.Defaults{TimeoutSeconds: 1, Resources: types.ResourceLimits{CPU: "1", Memory: "1Mi", Disk: "1Mi", TmpDisk: "1Mi"}})
	return err == nil
}

func (g *Gateway) create(w http.ResponseWriter, r *http.Request) {
	key, valid := validHeaderValue(r.Header, "Idempotency-Key", 256, true)
	if !valid {
		writeError(w, 400, "invalid_request", "invalid Idempotency-Key")
		return
	}
	var req types.CreateSandboxRequest
	if !readRequest(w, r, &req) {
		return
	}
	if !validCreate(req, g.settings.NetworkAllowed) {
		writeError(w, 400, "unsupported_profile", "unsupported creation profile")
		return
	}
	result, err := g.service.CreateBusiness(r.Context(), provisioning.PlainColdBusinessInput{IdempotencyKey: key, Request: req})
	if _, valid := validHeaderValue(http.Header{"Idempotency-Key": []string{result.IdempotencyKey}}, "Idempotency-Key", 256, false); valid {
		w.Header().Set("Idempotency-Key", result.IdempotencyKey)
	}
	if errors.Is(err, state.ErrIdempotencyConflict) || errors.Is(err, provisioning.ErrPlainColdProfile) || errors.Is(err, state.ErrPreparationCapacity) {
		writeServiceError(w, err)
		return
	}
	if result.State == provisioning.PlainColdCapacity {
		writeServiceError(w, state.ErrPreparationCapacity)
		return
	}
	if result.State == provisioning.PlainColdPending && errors.Is(err, state.ErrConflict) && result.RequestID == "" && result.IntentID == "" && result.SandboxID == "" {
		writeServiceError(w, state.ErrConflict)
		return
	}
	if err != nil || result.State != provisioning.PlainColdCompleted && result.State != provisioning.PlainColdHistorical {
		w.Header().Set("Retry-After", "1")
		writeJSON(w, 202, struct {
			Code      string                      `json:"code"`
			State     provisioning.PlainColdState `json:"state"`
			SandboxID string                      `json:"sandbox_id,omitempty"`
			RequestID string                      `json:"request_id,omitempty"`
			IntentID  string                      `json:"intent_id,omitempty"`
		}{"create_pending", result.State, result.SandboxID, result.RequestID, result.IntentID})
		return
	}
	current, err := g.service.Lookup(r.Context(), result.SandboxID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	response, err := sandboxResponse(current)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, 201, types.CreateSandboxResponse{SandboxResponse: response, Reused: result.Reused})
}

func (g *Gateway) lookup(w http.ResponseWriter, r *http.Request, id string) {
	current, err := g.service.Lookup(r.Context(), id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	response, err := sandboxResponse(current)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, 200, response)
}

// updateTTL retains the original authenticated business-call lifetime. The
// capability owns the trusted clock, original attempt and runtime installation;
// the HTTP boundary cannot extend expiry itself or retry an uncertain outcome.
func (g *Gateway) updateTTL(w http.ResponseWriter, r *http.Request, id string) {
	service, supported := g.service.(ttlService)
	if !supported {
		writeError(w, 501, "unsupported", "TTL renewal is not implemented in etcd mode")
		return
	}
	for name := range r.Header {
		if strings.EqualFold(name, "Idempotency-Key") {
			writeError(w, 400, "invalid_request", "Idempotency-Key is not supported for TTL renewal")
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

	var request types.UpdateTTLRequest
	if !readRequest(w, r, &request) {
		return
	}
	// Seconds retain the old positive timeout DTO. The finite upper bound is
	// the same 365-day limit used by plain business creation and its snapshot.
	if request.Timeout < 1 || request.Timeout > 365*24*60*60 {
		writeError(w, 400, "invalid_request", "invalid TTL timeout")
		return
	}

	result, err := service.UpdateTTL(r.Context(), id, requestID, request.Timeout)
	if errors.Is(err, state.ErrOutcomeUnknown) {
		writeError(w, 503, "outcome_unknown", "TTL renewal outcome is unknown")
		return
	}
	if err != nil {
		writeServiceError(w, err)
		return
	}
	// Invalid success output cannot be repaired by a new Lookup or renewal:
	// either would lose the original attempt's outcome and TTL baseline.
	if result.Timeout != request.Timeout || result.ExpiresAt.IsZero() || result.ExpiresAt.Location() != time.UTC || result.ExpiresAt.Year() < 1 || result.ExpiresAt.Year() > 9999 {
		writeError(w, 503, "outcome_unknown", "TTL renewal returned an invalid result")
		return
	}
	writeJSON(w, 200, result)
}

func sandboxResponse(current *provisioning.PlainColdLookupResult) (types.SandboxResponse, error) {
	if current == nil || current.Business == nil || current.Business.Resolved.TTL.Kind != "finite" {
		return types.SandboxResponse{}, provisioning.ErrPlainColdUnavailable
	}
	expires := current.ExpiresAt
	timeout := int(current.Business.Resolved.TTL.Seconds)
	if current.TimeoutSeconds > 0 {
		timeout = current.TimeoutSeconds
	}
	return types.SandboxResponse{ID: current.SandboxID, Mode: current.Business.Requested.Mode, State: "running", RuntimeID: current.Runtime.ID, CreatedAt: current.CreatedAt, Timeout: timeout, ExpiresAt: &expires}, nil
}
