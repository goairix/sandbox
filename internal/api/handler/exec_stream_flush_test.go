package handler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/goairix/sandbox/internal/api/handler"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/sandbox"
	"github.com/goairix/sandbox/internal/telemetry/metrics"
	"github.com/stretchr/testify/require"
)

type silentStreamRuntime struct{ uploadRuntime }

func (r *silentStreamRuntime) ExecStream(ctx context.Context, _ string, _ runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
	ch := make(chan runtime.StreamEvent)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func (r *silentStreamRuntime) GetSandbox(context.Context, string) (*runtime.SandboxInfo, error) {
	return &runtime.SandboxInfo{RuntimeID: "runtime-a", RuntimeUID: "uid-a", State: "running"}, nil
}

func (r *silentStreamRuntime) RemoveSandbox(context.Context, string) error { return nil }

func TestExecStreamFlushesHeadersBeforeSilentCommandOutput(t *testing.T) {
	initFileHandlerMetrics.Do(func() { require.NoError(t, metrics.InitNoop()) })
	rt := &silentStreamRuntime{}
	mgr := sandbox.NewManager(rt, nil, nil, sandbox.ManagerConfig{})
	sb, err := mgr.Create(context.Background(), sandbox.SandboxConfig{Mode: sandbox.ModePersistent})
	require.NoError(t, err)
	router := gin.New()
	router.POST("/sandboxes/:id/exec/stream", handler.NewHandler(mgr).ExecStream)
	server := httptest.NewServer(router)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/sandboxes/"+sb.ID+"/exec/stream", strings.NewReader(`{"language":"bash","code":"sleep 30"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	response, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "text/event-stream", response.Header.Get("Content-Type"))
	require.Less(t, time.Since(start), time.Second, "stream must open before first output or heartbeat")
	cancel()
}
