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
	"github.com/goairix/sandbox/pkg/types"
	"github.com/google/uuid"
)

type Service interface {
	CreateBusiness(context.Context, provisioning.PlainColdBusinessInput) (provisioning.PlainColdBusinessResult, error)
	Lookup(context.Context, string) (*provisioning.PlainColdLookupResult, error)
	Exec(context.Context, string, string, p.ExecutionRequest, io.Writer, io.Writer) (provisioning.PlainExecResult, error)
	Ready(context.Context) error
}

type Gateway struct {
	service   Service
	settings  deployment.HTTPSettings
	mu        sync.Mutex
	closed    bool
	active    map[*gatewayCall]struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	tokens    float64
	lastToken time.Time
}

type gatewayCall struct {
	ctx    context.Context
	cancel context.CancelFunc
}

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
	return &Gateway{service: service, settings: owned, active: make(map[*gatewayCall]struct{}), tokens: float64(owned.RequestsPerSecond), lastToken: time.Now()}, nil
}

func (g *Gateway) enter(ctx context.Context) (*gatewayCall, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, 503
	}
	if len(g.active) >= g.settings.MaxActive {
		return nil, 429
	}
	call := &gatewayCall{}
	call.ctx, call.cancel = context.WithCancel(ctx)
	g.active[call] = struct{}{}
	g.wg.Add(1)
	return call, 0
}

func (g *Gateway) leave(call *gatewayCall) {
	call.cancel()
	g.mu.Lock()
	delete(g.active, call)
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
func (g *Gateway) Close() {
	g.closeOnce.Do(func() {
		g.stop()
		g.wg.Wait()
		g.settings.Zero()
	})
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
	call, status := g.enter(r.Context())
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
		supported := len(parts) == 2 && r.Method == http.MethodGet || r.Method == http.MethodPost && (len(parts) == 3 && parts[2] == "exec" || len(parts) == 4 && parts[2] == "exec" && parts[3] == "stream")
		if supported {
			id := parts[1]
			parsed, err := uuid.Parse(id)
			if err != nil || parsed == uuid.Nil || parsed.String() != id {
				writeError(w, 400, "invalid_request", "invalid sandbox id")
				return
			}
			if len(parts) == 2 {
				g.lookup(w, r, id)
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
	writeJSON(w, 201, types.CreateSandboxResponse{SandboxResponse: response, Reused: result.State == provisioning.PlainColdHistorical})
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

func sandboxResponse(current *provisioning.PlainColdLookupResult) (types.SandboxResponse, error) {
	if current == nil || current.Business == nil || current.Business.Resolved.TTL.Kind != "finite" {
		return types.SandboxResponse{}, provisioning.ErrPlainColdUnavailable
	}
	expires := current.ExpiresAt
	return types.SandboxResponse{ID: current.SandboxID, Mode: current.Business.Requested.Mode, State: "running", RuntimeID: current.Runtime.ID, CreatedAt: current.CreatedAt, Timeout: int(current.Business.Resolved.TTL.Seconds), ExpiresAt: &expires}, nil
}
