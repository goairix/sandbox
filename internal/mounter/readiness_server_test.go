package mounter

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/require"
)

func TestReadinessHTTPExposesOnlyFixedReadonlyBoolean(t *testing.T) {
	supervisor, _ := residentReadySupervisor(t)
	supervisor.mu.Lock()
	supervisor.config.MountInfo = func() (Mount, error) { panic("HTTP invoked a filesystem/management check") }
	supervisor.mu.Unlock()
	observer := sampledResidentObserver(supervisor)
	cache := observer.cache.Load()
	handler := newReadinessHTTPHandler(observer)

	for _, test := range []struct {
		method   string
		path     string
		body     string
		status   int
		response string
	}{
		{method: "GET", path: "/readyz", status: 200, response: "ready\n"},
		{method: "POST", path: "/readyz", status: 405, response: "not ready\n"},
		{method: "GET", path: "/authorize", status: 404, response: "not ready\n"},
		{method: "GET", path: "/flush", status: 404, response: "not ready\n"},
		{method: "GET", path: "/readyz?credentials=must-not-echo", status: 404, response: "not ready\n"},
		{method: "GET", path: "/readyz", body: "credentials=must-not-echo", status: 400, response: "not ready\n"},
	} {
		t.Run(test.method+test.path+test.body, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			request.Header.Set("Authorization", "must-not-echo")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			require.Equal(t, test.status, response.Code)
			require.Equal(t, test.response, response.Body.String())
		})
	}
	cache.usage.Store(&CacheUsage{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/readyz", nil))
	require.Equal(t, 503, response.Code)
	require.Equal(t, "not ready\n", response.Body.String())
}

func TestReadinessHTTPServesIdleMountAndJoinsConnections(t *testing.T) {
	supervisor, mount := residentReadySupervisor(t)
	var checks atomic.Int32
	supervisor.mu.Lock()
	supervisor.config.MountInfo = func() (Mount, error) {
		checks.Add(1)
		return mount, nil
	}
	supervisor.mu.Unlock()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := &ReadinessServer{Supervisor: supervisor}
	joined := make(chan error, 1)
	go func() { joined <- server.serve(ctx, listener, startTestCacheMonitor) }()
	client := &http.Client{Timeout: time.Second}
	url := "http://" + listener.Addr().String() + "/readyz"
	isReady := func() bool {
		response, err := client.Get(url)
		if err != nil {
			return false
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		return err == nil && response.StatusCode == 200 && string(body) == "ready\n"
	}
	require.Eventually(t, isReady, 3*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool { return checks.Load() >= 5 && isReady() }, 6*time.Second, 10*time.Millisecond)
	oversized, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	oversized.Header.Set("X-Oversized", strings.Repeat("x", 16<<10))
	response, err := client.Do(oversized)
	require.NoError(t, err)
	require.Equal(t, http.StatusRequestHeaderFieldsTooLarge, response.StatusCode)
	require.NoError(t, response.Body.Close())

	// A partial request must expire within the configured one-second read
	// budget; a slow client cannot permanently retain an admission slot.
	slow, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	defer slow.Close()
	_, err = io.WriteString(slow, "GET /readyz HTTP/1.1\r\nHost:")
	require.NoError(t, err)
	require.NoError(t, slow.SetReadDeadline(time.Now().Add(2*time.Second)))
	var one [1]byte
	_, err = slow.Read(one[:])
	require.Error(t, err)

	cancel()
	select {
	case err := <-joined:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("resident readiness server did not join")
	}
	_, err = net.DialTimeout("tcp", listener.Addr().String(), 100*time.Millisecond)
	require.Error(t, err)
}

func TestReadinessHTTPCancellationClosesListenerButRetainsBlockedObserver(t *testing.T) {
	supervisor, mount := residentReadySupervisor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	supervisor.mu.Lock()
	config := CacheMonitorConfig{Root: supervisor.bootstrap.CacheDir, LimitBytes: 1024}
	supervisor.mu.Unlock()
	cache, err := startTestCacheMonitor(ctx, config)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return cache.Snapshot().Known }, time.Second, time.Millisecond)
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	supervisor.mu.Lock()
	supervisor.config.MountInfo = func() (Mount, error) {
		close(entered)
		<-release
		return mount, nil
	}
	supervisor.mu.Unlock()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &ReadinessServer{Supervisor: supervisor}
	joined := make(chan error, 1)
	go func() {
		joined <- server.serve(ctx, listener, func(context.Context, CacheMonitorConfig) (*CacheMonitor, error) { return cache, nil })
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("mount syscall slot did not begin")
	}
	cancel()
	select {
	case <-joined:
		t.Fatal("HTTP server returned while its original observer was still blocked")
	case <-time.After(50 * time.Millisecond):
	}
	connection, err := net.DialTimeout("tcp", listener.Addr().String(), 100*time.Millisecond)
	if connection != nil {
		_ = connection.Close()
	}
	require.Error(t, err)
	unblock.Do(func() { close(release) })
	require.NoError(t, <-joined)
}

func TestReadinessConnectionAdmissionIsBounded(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	listener := newReadinessListener(base, 1)
	defer listener.Close()
	firstClient, err := net.Dial("tcp", base.Addr().String())
	require.NoError(t, err)
	defer firstClient.Close()
	first, err := listener.Accept()
	require.NoError(t, err)
	defer first.Close()
	secondClient, err := net.Dial("tcp", base.Addr().String())
	require.NoError(t, err)
	defer secondClient.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, _ := listener.Accept()
		accepted <- connection
	}()
	select {
	case <-accepted:
		t.Fatal("connection exceeded the single active admission slot")
	case <-time.After(30 * time.Millisecond):
	}
	require.NoError(t, first.Close())
	select {
	case second := <-accepted:
		require.NotNil(t, second)
		require.NoError(t, second.Close())
	case <-time.After(time.Second):
		t.Fatal("closed connection did not release its admission slot")
	}
}

func TestReadinessHTTPRetainsCacheBackendCloseFailure(t *testing.T) {
	supervisor, _ := residentReadySupervisor(t)
	want := errors.New("cache reader quiescence was not certified")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	started := make(chan struct{})
	joined := make(chan error, 1)
	server := &ReadinessServer{Supervisor: supervisor}
	go func() {
		joined <- server.serve(ctx, listener, func(parent context.Context, config CacheMonitorConfig) (*CacheMonitor, error) {
			watcher := &testCacheWatcher{events: make(chan fsnotify.Event), errors: make(chan error), close: func() error { return want }}
			monitor, err := startCacheMonitor(parent, config, func(uint) (cacheWatcher, error) { return watcher, nil })
			close(started)
			return monitor, err
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cache was not started")
	}
	cancel()
	require.ErrorIs(t, <-joined, want)
}

func TestReadinessHTTPCancellationDuringCacheStartupIsNormal(t *testing.T) {
	supervisor, _ := residentReadySupervisor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	entered := make(chan struct{})
	joined := make(chan error, 1)
	server := &ReadinessServer{Supervisor: supervisor}
	go func() {
		joined <- server.serve(ctx, listener, func(parent context.Context, _ CacheMonitorConfig) (*CacheMonitor, error) {
			close(entered)
			<-parent.Done()
			return nil, parent.Err()
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("cache startup did not begin")
	}
	cancel()
	require.NoError(t, <-joined)
}
