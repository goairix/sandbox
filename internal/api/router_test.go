package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/api/handler"
	sandboxruntime "github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/sandbox"
	"github.com/goairix/sandbox/internal/telemetry/metrics"
)

type generatedZeroReader struct{ remaining int64 }

func (r *generatedZeroReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > r.remaining {
		n = r.remaining
	}
	clear(p[:n])
	r.remaining -= n
	return int(n), nil
}

type streamingUploadRuntime struct {
	sandboxruntime.Runtime
	bytesRead int64
	copyErr   error
}

func (r *streamingUploadRuntime) CreateSandbox(_ context.Context, spec sandboxruntime.SandboxSpec) (*sandboxruntime.SandboxInfo, error) {
	return &sandboxruntime.SandboxInfo{ID: spec.ID, RuntimeID: "runtime-a", RuntimeUID: "uid-a"}, nil
}

func (r *streamingUploadRuntime) RenameSandbox(context.Context, string, string) error { return nil }
func (r *streamingUploadRuntime) UpdateLabels(context.Context, string, map[string]*string) error {
	return nil
}
func (r *streamingUploadRuntime) UploadFile(_ context.Context, _, _ string, size int64, body io.Reader) error {
	n, err := io.Copy(io.Discard, body)
	r.bytesRead = n
	if err != nil {
		r.copyErr = err
		return err
	}
	if n != size {
		r.copyErr = fmt.Errorf("unexpected upload size: %d", n)
		return r.copyErr
	}
	return nil
}

var initRouterMetrics sync.Once

func streamingUploadRouter(t *testing.T, maxUploadBytes int64) (http.Handler, string, *streamingUploadRuntime) {
	t.Helper()
	initRouterMetrics.Do(func() { require.NoError(t, metrics.InitNoop()) })
	rt := &streamingUploadRuntime{}
	mgr := sandbox.NewManager(rt, nil, nil, sandbox.ManagerConfig{})
	sb, err := mgr.Create(context.Background(), sandbox.SandboxConfig{
		Mode: sandbox.ModePersistent, Network: sandbox.NetworkConfig{Enabled: true},
	})
	require.NoError(t, err)
	router := SetupRouter(handler.NewHandler(mgr, maxUploadBytes), "", 0, "test")
	return router, sb.ID, rt
}

func TestReadinessIsDependencyAwareWhileLivenessStaysHealthy(t *testing.T) {
	initRouterMetrics.Do(func() { require.NoError(t, metrics.InitNoop()) })
	rt := &streamingUploadRuntime{}
	mgr := sandbox.NewManager(rt, nil, nil, sandbox.ManagerConfig{})
	router := SetupRouter(handler.NewHandler(mgr, 1<<20), "", 0, "test", func(context.Context) error {
		return errors.New("redis unavailable")
	})

	ready := httptest.NewRecorder()
	router.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/ready", nil))
	assert.Equal(t, http.StatusServiceUnavailable, ready.Code)
	live := httptest.NewRecorder()
	router.ServeHTTP(live, httptest.NewRequest(http.MethodGet, "/health", nil))
	assert.Equal(t, http.StatusOK, live.Code)
}

func TestStreamingUploadCanExceedDefaultBodyLimitWithoutLinearMemoryGrowth(t *testing.T) {
	const payloadSize int64 = 1 << 30
	router, sandboxID, rt := streamingUploadRouter(t, payloadSize)
	boundary := "sandbox-stream-boundary"
	prefix := fmt.Sprintf("--%s\r\nContent-Disposition: form-data; name=\"file\"; filename=\"big.bin\"\r\nContent-Type: application/octet-stream\r\n\r\n", boundary)
	suffix := fmt.Sprintf("\r\n--%s--\r\n", boundary)
	body := io.MultiReader(strings.NewReader(prefix), &generatedZeroReader{remaining: payloadSize}, strings.NewReader(suffix))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/"+sandboxID+"/files/upload?path=/workspace/big.bin", body)
	req.ContentLength = int64(len(prefix)) + payloadSize + int64(len(suffix))
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.Header.Set("X-Sandbox-File-Size", fmt.Sprint(payloadSize))

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, payloadSize, rt.bytesRead)
	assert.NoError(t, rt.copyErr)
	var heapGrowth uint64
	if after.HeapInuse > before.HeapInuse {
		heapGrowth = after.HeapInuse - before.HeapInuse
	}
	assert.Less(t, heapGrowth, uint64(32<<20))
}

func TestNonUploadRouteRetainsDefault64MiBBodyLimit(t *testing.T) {
	router, sandboxID, _ := streamingUploadRouter(t, 1<<30)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/"+sandboxID+"/files/read", &generatedZeroReader{remaining: (64 << 20) + 1})
	req.ContentLength = (64 << 20) + 1
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}
