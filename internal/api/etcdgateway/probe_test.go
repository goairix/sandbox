package etcdgateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/provisioning"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

func TestGatewayProbeIndependentBusinessAndReadyLanes(t *testing.T) {
	service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}}
	readyStarted := make(chan struct{}, 2)
	businessStarted := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	var readyCount atomic.Int32
	service.ready = func(ctx context.Context) error {
		readyCount.Add(1)
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 2*time.Second || time.Until(deadline) < time.Second {
			t.Error("ready did not receive the original bounded 2-second context")
		}
		readyStarted <- struct{}{}
		<-ctx.Done()
		<-release
		return ctx.Err()
	}
	service.execCall = func(ctx context.Context, _, _ string, _ p.ExecutionRequest, _, _ io.Writer) (provisioning.PlainExecResult, error) {
		close(businessStarted)
		<-ctx.Done()
		<-release
		return provisioning.PlainExecResult{State: provisioning.PlainExecUnknown}, ctx.Err()
	}
	gateway, client, origin := fileGatewaySocket(t, service, 1)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	answers := make(chan int, 3)
	go func() {
		status, _, _ := fileHTTPRequest(t, client, origin, "POST", gatewayExecPath, `{"language":"bash","code":"wait"}`, nil)
		answers <- status
	}()
	select {
	case <-businessStarted:
	case <-time.After(time.Second):
		t.Fatal("business did not start")
	}
	status, _, _ := fileHTTPRequest(t, client, origin, "POST", gatewayExecPath, `{"language":"bash","code":"wait"}`, nil)
	if status != 429 || service.execs.Load() != 1 {
		t.Fatal("business lane limit changed", status)
	}
	for i := 0; i < 2; i++ {
		go func() {
			status, _, _ := fileHTTPRequest(t, client, origin, "GET", "/ready", "", nil)
			answers <- status
		}()
		select {
		case <-readyStarted:
		case status := <-answers:
			t.Fatalf("ready borrowed full business capacity: %d", status)
		case <-time.After(time.Second):
			t.Fatal("independent ready did not start")
		}
	}
	status, _, _ = fileHTTPRequest(t, client, origin, "GET", "/ready", "", nil)
	if status != 503 || readyCount.Load() != 2 {
		t.Fatal("ready lane limit changed", status, readyCount.Load())
	}
	status, _, wire := fileHTTPRequest(t, client, origin, "GET", "/health", "", nil)
	if status != 200 || !strings.Contains(string(wire), `"status":"ok"`) || readyCount.Load() != 2 || service.execs.Load() != 1 {
		t.Fatal("health borrowed ready/business or called service", status, string(wire))
	}
	closed := make(chan struct{})
	go func() { gateway.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned before original business and ready owners joined")
	case <-time.After(30 * time.Millisecond):
	}
	for _, route := range []string{"/health", "/ready", gatewayFilesPath + "list"} {
		status, _, _ = fileHTTPRequest(t, client, origin, "GET", route, "", nil)
		if status != 503 {
			t.Fatal("closed gateway admitted lane", route, status)
		}
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not join released original owners")
	}
	for i := 0; i < 3; i++ {
		select {
		case <-answers:
		case <-time.After(time.Second):
			t.Fatal("original socket handler did not finish")
		}
	}
}

type probeBlockedWriter struct {
	header  http.Header
	started chan<- struct{}
	release <-chan struct{}
}

func (w *probeBlockedWriter) Header() http.Header { return w.header }
func (*probeBlockedWriter) WriteHeader(int)       {}
func (w *probeBlockedWriter) Write(data []byte) (int, error) {
	w.started <- struct{}{}
	<-w.release
	return len(data), nil
}

func TestGatewayProbeFiniteHealthLaneAndCloseJoin(t *testing.T) {
	service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}, ready: func(context.Context) error { t.Error("health called Ready"); return nil }}
	gateway, err := New(service, gatewaySettings())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); gateway.Close() })
	done := make(chan struct{}, 4)
	for i := 0; i < 4; i++ {
		go func() {
			gateway.ServeHTTP(&probeBlockedWriter{header: make(http.Header), started: started, release: release}, httptest.NewRequest("GET", "/health", nil))
			done <- struct{}{}
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("health lane capacity below four")
		}
	}
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, httptest.NewRequest("GET", "/health", nil))
	if response.Code != 503 {
		t.Fatal("health lane admitted fifth owner", response.Code)
	}
	closed := make(chan struct{})
	go func() { gateway.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close skipped admitted health writers")
	case <-time.After(30 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close failed to join health writers")
	}
	for i := 0; i < 4; i++ {
		<-done
	}
}

func TestGatewayProbeReadyActualUnavailable(t *testing.T) {
	service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}, ready: func(context.Context) error { return errors.New("private-ready-sentinel") }}
	_, client, origin := fileGatewaySocket(t, service, 1)
	status, _, wire := fileHTTPRequest(t, client, origin, "GET", "/ready", "", nil)
	if status != 503 || !strings.Contains(string(wire), `"status":"unavailable"`) || strings.Contains(string(wire), "private-ready-sentinel") {
		t.Fatal("ready fabricated availability or leaked cause", status, string(wire))
	}
	status, _, _ = fileHTTPRequest(t, client, origin, "GET", "/health", "", nil)
	if status != 200 {
		t.Fatal("health depended on ready", status)
	}
}
