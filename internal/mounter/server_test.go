package mounter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

// This is a transport component fixture: the original injectable mount/cache
// model supplies the sample. It neither attests a native PID1 nor registers an
// Observer slot; the receiving Observer must independently authenticate both.
// The private loop is also called by Serve after its fixed root-peer check;
// these ordinary host tests do not replace that check with a callback.
func TestReadinessChannelPullsOriginalResidentSampleAndJoins(t *testing.T) {
	supervisor, observer := residentV2ControlSample(t)
	sample := observer.sample.Load()
	usage := observer.cache.Load().Snapshot()
	supervisor.mu.Lock()
	supervisor.config.MountInfo = func() (Mount, error) { panic("channel started a new mount probe") }
	supervisor.mu.Unlock()

	socketDir, err := os.MkdirTemp("/tmp", "wm-health-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "roothealth.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	require.NoError(t, listener.SetDeadline(time.Now().Add(time.Second)))
	ctx, cancel := context.WithCancel(context.Background())
	joined := make(chan error, 1)
	channel := &ReadinessChannel{
		Supervisor:     supervisor,
		SocketPath:     socket,
		PodName:        "pod-a",
		NodeUID:        "22222222-2222-4222-8222-222222222222",
		NodeBootID:     "33333333-3333-4333-8333-333333333333",
		ContractDigest: strings.Repeat("a", 64),
	}
	go func() {
		conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
		if err != nil {
			joined <- err
			return
		}
		defer conn.Close()
		joined <- channel.serveConnection(ctx, conn)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-joined:
			require.True(t, err == nil || errors.Is(err, context.Canceled), "original channel join: %v", err)
		case <-time.After(time.Second):
			t.Error("channel returned without joining its original Unix IO")
		}
	})
	conn, err := listener.AcceptUnix()
	require.NoError(t, err)
	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
	})
	require.NoError(t, conn.SetDeadline(time.Now().Add(time.Second)))

	hello, err := readFrame(conn)
	require.NoError(t, err)
	var registration fuseprotocol.SocketRequest
	require.NoError(t, fuseprotocol.DecodeExact(hello, &registration))
	require.Equal(t, fuseprotocol.Version, registration.Version)
	require.Equal(t, "register-readiness", registration.Command)
	require.JSONEq(t, `{"version":2,"runtime_uid":"uid-a","pod_name":"pod-a","node_uid":"22222222-2222-4222-8222-222222222222","node_boot_id":"33333333-3333-4333-8333-333333333333","contract_digest":"`+channel.ContractDigest+`"}`, string(registration.Input))
	require.NoError(t, writeResponse(conn, []byte(`{"version":2,"accepted":true}`), ""))

	var previous fuseprotocol.MounterReadiness
	for i := 0; i < 2; i++ {
		request, err := json.Marshal(fuseprotocol.SocketRequest{Version: fuseprotocol.Version, Command: "health-ready", Input: json.RawMessage(`{"version":2}`)})
		require.NoError(t, err)
		anchor := time.Now()
		require.NoError(t, writeFrame(conn, request))
		wire, err := readFrame(conn)
		require.NoError(t, err)
		var response fuseprotocol.SocketResponse
		require.NoError(t, fuseprotocol.DecodeExact(wire, &response))
		require.True(t, response.OK)
		require.Empty(t, response.ErrorCode)
		status, err := fuseprotocol.DecodeMounterReadiness(response.Output)
		require.NoError(t, err)
		require.Equal(t, sample.sequence, status.SampleSequence)
		require.Equal(t, sample.observedAt.UTC(), status.SampledAt)
		require.Equal(t, usage.ObservedAt.UTC(), status.CacheObservedAt)
		require.True(t, time.Now().Before(anchor.Add(time.Duration(status.ValidForNanoseconds))))
		if i > 0 {
			require.Less(t, status.ValidForNanoseconds, previous.ValidForNanoseconds, "polling must not refresh the source sample")
		}
		previous = status
	}

	// A now-unknown original accounting observation cannot be hidden by the
	// positive response already sent over this same persistent connection.
	observer.cache.Load().usage.Store(&CacheUsage{})
	request, err := json.Marshal(fuseprotocol.SocketRequest{Version: fuseprotocol.Version, Command: "health-ready", Input: json.RawMessage(`{"version":2}`)})
	require.NoError(t, err)
	require.NoError(t, writeFrame(conn, request))
	wire, err := readFrame(conn)
	require.NoError(t, err)
	var response fuseprotocol.SocketResponse
	require.NoError(t, fuseprotocol.DecodeExact(wire, &response))
	require.False(t, response.OK)
	require.Equal(t, fuseprotocol.MounterErrorRejected, response.ErrorCode)
	require.JSONEq(t, `{}`, string(response.Output), "the original strict response envelope represents no output with an empty object")
}

func TestReadinessChannelRejectsManagementFrames(t *testing.T) {
	for _, command := range []string{"authorize", "flush", "shutdown", "health-prepared", "health-ready"} {
		t.Run(command, func(t *testing.T) {
			supervisor, _ := residentV2ControlSample(t)
			before := supervisor.State()
			runner := supervisor.runner.(*fakeRunner)
			runner.mu.Lock()
			starts, runs := runner.starts, len(runner.runs)
			runner.mu.Unlock()
			socketDir, err := os.MkdirTemp("/tmp", "wm-health-")
			require.NoError(t, err)
			t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
			socket := filepath.Join(socketDir, "roothealth.sock")
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
			require.NoError(t, err)
			defer listener.Close()
			require.NoError(t, listener.SetDeadline(time.Now().Add(time.Second)))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			joined := make(chan error, 1)
			channel := &ReadinessChannel{Supervisor: supervisor, SocketPath: socket, PodName: "pod-a", NodeUID: "22222222-2222-4222-8222-222222222222", NodeBootID: "33333333-3333-4333-8333-333333333333", ContractDigest: strings.Repeat("a", 64)}
			go func() {
				conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
				if err != nil {
					joined <- err
					return
				}
				defer conn.Close()
				joined <- channel.serveConnection(ctx, conn)
			}()
			defer func() {
				cancel()
				select {
				case <-joined:
				case <-time.After(time.Second):
					t.Error("rejected frame retained original channel IO")
				}
			}()
			conn, err := listener.AcceptUnix()
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, conn.SetDeadline(time.Now().Add(time.Second)))
			_, err = readFrame(conn)
			require.NoError(t, err)
			require.NoError(t, writeResponse(conn, []byte(`{"version":2,"accepted":true}`), ""))
			input := json.RawMessage(`{}`)
			if command == "flush" || command == "shutdown" {
				input, err = json.Marshal(fuseprotocol.ControlRequest{Version: fuseprotocol.Version, RuntimeUID: "uid-a", Generation: validAuthorization().LeaseGeneration})
				require.NoError(t, err)
			}
			request, err := json.Marshal(fuseprotocol.SocketRequest{Version: fuseprotocol.Version, Command: command, Input: input})
			require.NoError(t, err)
			require.NoError(t, writeFrame(conn, request))
			wire, err := readFrame(conn)
			require.NoError(t, err)
			var response fuseprotocol.SocketResponse
			require.NoError(t, fuseprotocol.DecodeExact(wire, &response))
			require.False(t, response.OK)
			require.Equal(t, fuseprotocol.MounterErrorRejected, response.ErrorCode)
			require.JSONEq(t, `{}`, string(response.Output))
			require.Equal(t, before, supervisor.State(), "the read-only channel must never enter the management dispatcher")
			runner.mu.Lock()
			afterStarts, afterRuns := runner.starts, len(runner.runs)
			runner.mu.Unlock()
			require.Equal(t, starts, afterStarts)
			require.Equal(t, runs, afterRuns, "a structurally valid flush request must still never execute")
		})
	}
}

type shortWriter struct{ bytes.Buffer }

func (w *shortWriter) Write(value []byte) (int, error) {
	if len(value) > 1 {
		value = value[:1]
	}
	return w.Buffer.Write(value)
}

func TestServerClientAuthorizeContractAndRejectReplay(t *testing.T) {
	runner := &fakeRunner{}
	supervisor, _ := newTestSupervisor(t, runner)
	socketDir, err := os.MkdirTemp("/tmp", "wm-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	require.NoError(t, os.Chmod(socketDir, 0o700))
	socket := filepath.Join(socketDir, "control.sock")
	server := &Server{Supervisor: supervisor, SocketPath: socket, ExpectedRuntimeUID: "uid-a", VerifyPeer: func(*net.UnixConn) error { return nil }, IOTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	require.Eventually(t, func() bool {
		select {
		case err := <-done:
			require.NoError(t, err)
			return false
		default:
			_, err := os.Stat(socket)
			return err == nil
		}
	}, time.Second, time.Millisecond)

	raw, err := json.Marshal(validAuthorization())
	require.NoError(t, err)
	client := Client{SocketPath: socket, IOTimeout: time.Second}
	output, err := client.Do(context.Background(), "authorize", raw)
	require.NoError(t, err)
	var ack fuseprotocol.ControlAck
	require.NoError(t, fuseprotocol.DecodeExact(output, &ack))
	assert.True(t, ack.Accepted)
	_, err = client.Do(context.Background(), "authorize", raw)
	require.Error(t, err)
	assert.Equal(t, 1, runner.starts)

	cancel()
	assert.NoError(t, <-done)
}

func TestServerRejectsNonRootPeerBeforeDispatch(t *testing.T) {
	runner := &fakeRunner{}
	supervisor, _ := newTestSupervisor(t, runner)
	socketDir, err := os.MkdirTemp("/tmp", "wm-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	require.NoError(t, os.Chmod(socketDir, 0o700))
	socket := filepath.Join(socketDir, "control.sock")
	server := &Server{Supervisor: supervisor, SocketPath: socket, ExpectedRuntimeUID: "uid-a", VerifyPeer: func(*net.UnixConn) error { return assert.AnError }, IOTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	require.Eventually(t, func() bool {
		select {
		case err := <-done:
			require.NoError(t, err)
			return false
		default:
			_, err := os.Stat(socket)
			return err == nil
		}
	}, time.Second, time.Millisecond)
	client := Client{SocketPath: socket, IOTimeout: time.Second}
	_, err = client.Do(context.Background(), "authorize", []byte(`{}`))
	require.Error(t, err)
	assert.Equal(t, 0, runner.starts)
	cancel()
	assert.NoError(t, <-done)
}

func TestReadFrameRejectsOversizedPayloadBeforeAllocation(t *testing.T) {
	server, client := net.Pipe()
	defer func() { require.NoError(t, server.Close()) }()
	defer func() { require.NoError(t, client.Close()) }()
	go func() {
		_, _ = client.Write([]byte{0, 1, 0, 1})
	}()
	_, err := readFrame(server)
	require.Error(t, err)
}

func TestWriteFrameCompletesShortWrites(t *testing.T) {
	writer := &shortWriter{}
	require.NoError(t, writeFrame(writer, []byte(`{"ok":true}`)))
	raw, err := readFrame(bytes.NewReader(writer.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, []byte(`{"ok":true}`), raw)
}

func TestOperationMayOutliveFramingDeadline(t *testing.T) {
	runner := &fakeRunner{runDelay: 40 * time.Millisecond}
	supervisor, bootstrap := newTestSupervisor(t, runner)
	require.NoError(t, supervisor.Authorize(context.Background(), validAuthorization()))
	supervisor.config.MountInfo = func() (Mount, error) {
		return Mount{ID: 42, MountPoint: bootstrap.MountPath, FilesystemType: "fuse.s3fs"}, nil
	}
	socketDir, err := os.MkdirTemp("/tmp", "wm-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	require.NoError(t, os.Chmod(socketDir, 0o700))
	socket := filepath.Join(socketDir, "control.sock")
	server := &Server{Supervisor: supervisor, SocketPath: socket, ExpectedRuntimeUID: "uid-a", VerifyPeer: func(*net.UnixConn) error { return nil }, IOTimeout: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	require.Eventually(t, func() bool { _, err := os.Stat(socket); return err == nil }, time.Second, time.Millisecond)
	client := Client{SocketPath: socket, IOTimeout: 5 * time.Millisecond}
	output, err := client.Do(context.Background(), "health-ready", []byte(`{}`))
	require.NoError(t, err)
	var status fuseprotocol.MounterStatus
	require.NoError(t, fuseprotocol.DecodeExact(output, &status))
	assert.Equal(t, "ready", status.State)
	cancel()
	require.NoError(t, <-done)
}

// This exercises the existing dispatcher with its actual resident observer.
// The injected accounting model deliberately differs from the empty directory:
// a request that walks the directory again returns zero instead of this sample.
func TestServerReadyConsumesResidentSample(t *testing.T) {
	supervisor, observer := residentControlObserver(t)
	usage := observer.cache.Load().Snapshot()
	usage.Bytes = 37
	observer.cache.Load().usage.Store(&usage)
	server := &Server{Supervisor: supervisor}
	output, err := server.dispatch(context.Background(), "health-ready", []byte(`{}`))
	require.NoError(t, err)
	var status fuseprotocol.MounterStatus
	require.NoError(t, fuseprotocol.DecodeExact(output, &status))
	require.Equal(t, "ready", status.State)
	require.Equal(t, "uid-a", status.RuntimeUID)
	require.Equal(t, validAuthorization().LeaseGeneration, status.Generation)
	require.Equal(t, int64(37), status.CacheBytes, "the request must use the resident incremental accounting sample")
	require.Equal(t, usage.LimitBytes, status.CacheLimitBytes)
}

// Use the original injectable mount/cache fixture without a periodic writer,
// so a request cannot race a new sample into either fixed timestamp assertion.
// This component fixture does not claim an OS-backed mounter identity.
func residentV2ControlSample(t *testing.T) (*Supervisor, *readinessObserver) {
	t.Helper()
	supervisor, _ := residentReadySupervisor(t)
	observer := sampledResidentObserver(supervisor)
	usage := observer.cache.Load().Snapshot()
	usage.ObservedAt = time.Now().Add(-time.Second)
	observer.cache.Load().usage.Store(&usage)
	_, err := observer.refresh()
	require.NoError(t, err)
	require.NotNil(t, observer.sample.Load(), "the actual refresh must publish this sample")
	supervisor.mu.Lock()
	supervisor.readiness = observer
	supervisor.mu.Unlock()
	return supervisor, observer
}

func TestServerReadinessV2PreservesSampleTimesAndLegacyBytes(t *testing.T) {
	supervisor, observer := residentV2ControlSample(t)
	sample := observer.sample.Load()
	usage := observer.cache.Load().Snapshot()
	supervisor.mu.Lock()
	mountID := supervisor.mountID
	auth := supervisor.auth.Sanitized()
	supervisor.config.MountInfo = func() (Mount, error) { panic("read-only request started another probe") }
	supervisor.mu.Unlock()
	server := &Server{Supervisor: supervisor}
	legacy, err := server.dispatch(context.Background(), "health-ready", []byte(`{}`))
	require.NoError(t, err)
	expectedLegacy := fmt.Sprintf(`{"version":1,"state":"ready","runtime_uid":"uid-a","pool_key":"%s","mount_type":"fuse","generation":1,"restart_detected":false,"cache_bytes":0,"cache_limit_bytes":%d,"cache_exceeded":false}`, auth.PoolKey, usage.LimitBytes)
	require.Equal(t, expectedLegacy, string(legacy), "Version1 bytes must remain unchanged")
	var previous fuseprotocol.MounterReadiness
	for i := 0; i < 2; i++ {
		before := time.Now()
		wire, err := server.dispatch(context.Background(), "health-ready", []byte(`{"version":2}`))
		require.NoError(t, err)
		after := time.Now()
		status, err := fuseprotocol.DecodeMounterReadiness(wire)
		require.NoError(t, err)
		require.Equal(t, auth.RuntimeUID, status.RuntimeUID)
		require.Equal(t, auth.PoolKey, status.PoolKey)
		require.Equal(t, auth.LeaseGeneration, status.Generation)
		require.Equal(t, auth.MountAttempt, status.MountAttempt)
		require.Equal(t, mountID, status.MountID)
		require.Positive(t, status.SampleSequence)
		require.Equal(t, sample.observedAt.UTC(), status.SampledAt)
		require.Equal(t, usage.ObservedAt.UTC(), status.CacheObservedAt)
		require.Positive(t, status.ValidForNanoseconds)
		require.LessOrEqual(t, status.ValidForNanoseconds, int64(readinessMaximumAge))
		beforeRemaining := min(readinessMaximumAge-before.Sub(sample.observedAt), readinessMaximumAge-before.Sub(usage.ObservedAt))
		afterRemaining := min(readinessMaximumAge-after.Sub(sample.observedAt), readinessMaximumAge-after.Sub(usage.ObservedAt))
		require.LessOrEqual(t, status.ValidForNanoseconds, int64(beforeRemaining), "the older cache sample must shorten the mount sample's validity")
		require.GreaterOrEqual(t, status.ValidForNanoseconds, int64(afterRemaining))
		if i > 0 {
			require.Less(t, status.ValidForNanoseconds, previous.ValidForNanoseconds, "polling cannot renew an unchanged sample's remaining duration")
			fixed := status
			fixed.ValidForNanoseconds = previous.ValidForNanoseconds
			require.Equal(t, previous, fixed, "polling must preserve sequence, identity and both diagnostic UTC timestamps")
		}
		previous = status
	}

	t.Run("sample-shorter", func(t *testing.T) {
		supervisor, observer := residentV2ControlSample(t)
		sample := *observer.sample.Load()
		sample.observedAt = time.Now().Add(-2 * time.Second)
		observer.sample.Store(&sample)
		usage := observer.cache.Load().Snapshot()
		before := time.Now()
		wire, err := (&Server{Supervisor: supervisor}).dispatch(context.Background(), "health-ready", []byte(`{"version":2}`))
		require.NoError(t, err)
		after := time.Now()
		status, err := fuseprotocol.DecodeMounterReadiness(wire)
		require.NoError(t, err)
		require.LessOrEqual(t, status.ValidForNanoseconds, int64(min(readinessMaximumAge-before.Sub(sample.observedAt), readinessMaximumAge-before.Sub(usage.ObservedAt))))
		require.GreaterOrEqual(t, status.ValidForNanoseconds, int64(min(readinessMaximumAge-after.Sub(sample.observedAt), readinessMaximumAge-after.Sub(usage.ObservedAt))))
		require.Positive(t, status.ValidForNanoseconds, "the older mount sample must shorten the cache sample's validity")
	})

	for _, fault := range []string{"refresh-failure", "sequence-exhaustion"} {
		t.Run(fault, func(t *testing.T) {
			supervisor, observer := residentV2ControlSample(t)
			supervisor.mu.Lock()
			previousSequence := supervisor.readinessSequence
			if fault == "sequence-exhaustion" {
				previousSequence = ^uint64(0)
				supervisor.readinessSequence = previousSequence
			} else {
				supervisor.config.MountInfo = func() (Mount, error) {
					return Mount{}, fmt.Errorf("controlled mount observation failure")
				}
			}
			supervisor.mu.Unlock()
			_, err := observer.refresh()
			if fault == "sequence-exhaustion" {
				require.Error(t, err)
			} else {
				require.NoError(t, err, "an unavailable mount remains an unknown observation")
			}
			require.Nil(t, observer.sample.Load(), "neither failed observation nor exhausted sequence may publish readiness")
			supervisor.mu.Lock()
			currentSequence := supervisor.readinessSequence
			supervisor.mu.Unlock()
			require.Equal(t, previousSequence, currentSequence, "failed refresh must not advance or wrap the original supervisor sequence")
			_, err = (&Server{Supervisor: supervisor}).dispatch(context.Background(), "health-ready", []byte(`{"version":2}`))
			require.Error(t, err)
		})
	}

	for _, fault := range []string{"aged", "future", "unknown", "mount-attempt"} {
		t.Run(fault, func(t *testing.T) {
			supervisor, observer := residentV2ControlSample(t)
			sample := *observer.sample.Load()
			switch fault {
			case "aged":
				sample.observedAt = time.Now().Add(-4 * time.Second)
				observer.sample.Store(&sample)
			case "future":
				sample.observedAt = time.Now().Add(time.Second)
				observer.sample.Store(&sample)
			case "unknown":
				observer.sample.Store(nil)
			case "mount-attempt":
				supervisor.mu.Lock()
				supervisor.auth.MountAttempt++
				supervisor.mu.Unlock()
			}
			_, err := (&Server{Supervisor: supervisor}).dispatch(context.Background(), "health-ready", []byte(`{"version":2}`))
			require.Error(t, err, "a request must not replace unavailable or differently bound evidence")
		})
	}
}

func TestServerReadinessV2SequenceSurvivesObserverReplacement(t *testing.T) {
	supervisor, _ := residentReadySupervisor(t)
	server := &Server{Supervisor: supervisor}
	var last uint64
	for owner := 0; owner < 2; owner++ {
		observer, cancel, joined := runTestReadinessObserver(supervisor)
		physicallyJoined := false
		t.Cleanup(func() {
			cancel()
			if !physicallyJoined {
				require.NoError(t, <-joined)
			}
		})
		waitResidentReady(t, observer)
		for observation := 0; observation < 2; observation++ {
			var status fuseprotocol.MounterReadiness
			require.Eventually(t, func() bool {
				wire, err := server.dispatch(context.Background(), "health-ready", []byte(`{"version":2}`))
				if err != nil {
					return false
				}
				status, err = fuseprotocol.DecodeMounterReadiness(wire)
				return err == nil && status.SampleSequence > last
			}, 3*time.Second, 5*time.Millisecond, "only another successful original refresh may advance the sequence")
			require.Greater(t, status.SampleSequence, last)
			last = status.SampleSequence
		}
		cancel()
		result := <-joined
		physicallyJoined = true
		require.NoError(t, result)
		supervisor.mu.Lock()
		retired := supervisor.readiness == nil
		supervisor.mu.Unlock()
		require.True(t, retired, "replacement starts only after the original observer/cache actually joined")
	}
}

// Canonical local state can change while an earlier observation is retained.
// A ready-shaped reply from a fresh filesystem check must not replace missing,
// old or differently bound resident evidence in this read-only request path.
func TestServerReadyRefusesChangedResidentSample(t *testing.T) {
	for _, fault := range []string{"sample-age", "cache-age", "generation", "runtime-uid", "mount-id", "process", "unknown-cache"} {
		t.Run(fault, func(t *testing.T) {
			supervisor, observer := residentControlObserver(t)
			switch fault {
			case "sample-age":
				sample := *observer.sample.Load()
				sample.observedAt = time.Now().Add(-4 * time.Second)
				observer.sample.Store(&sample)
			case "cache-age":
				usage := observer.cache.Load().Snapshot()
				usage.ObservedAt = time.Now().Add(-4 * time.Second)
				observer.cache.Load().usage.Store(&usage)
			case "unknown-cache":
				observer.cache.Load().usage.Store(&CacheUsage{})
			default:
				supervisor.mu.Lock()
				switch fault {
				case "generation":
					supervisor.auth.LeaseGeneration++
				case "runtime-uid":
					supervisor.bootstrap.RuntimeUID = "successor-uid"
				case "mount-id":
					supervisor.mountID++
				case "process":
					supervisor.processDone = make(chan struct{})
				}
				supervisor.mu.Unlock()
			}
			_, err := (&Server{Supervisor: supervisor}).dispatch(context.Background(), "health-ready", []byte(`{}`))
			require.Error(t, err, "a request cannot manufacture a replacement positive observation")
		})
	}
}

// The ordinary injectable supervisor/observer is the model boundary. Accounting
// has an explicit joined lifetime, so no periodic producer can overwrite a
// deliberately stale or unknown observation during these request assertions.
func residentControlObserver(t *testing.T) (*Supervisor, *readinessObserver) {
	t.Helper()
	supervisor, _ := residentReadySupervisor(t)
	ctx, cancel := context.WithCancel(context.Background())
	observer := newReadinessObserver(ctx, supervisor, func(parent context.Context, config CacheMonitorConfig) (*CacheMonitor, error) {
		cacheContext, stop := context.WithCancel(parent)
		cache := &CacheMonitor{ctx: cacheContext, cancel: stop, done: make(chan struct{})}
		cache.usage.Store(&CacheUsage{Known: true, LimitBytes: config.LimitBytes, ObservedAt: time.Now()})
		go func() {
			defer close(cache.done)
			<-cacheContext.Done()
		}()
		return cache, nil
	})
	joined := make(chan error, 1)
	go func() { joined <- observer.run() }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-joined)
	})
	waitResidentReady(t, observer)
	return supervisor, observer
}

func TestServerClientPreservesSafeReadyDiagnostic(t *testing.T) {
	runner := &fakeRunner{process: &fakeProcess{exit: make(chan error, 1)}}
	supervisor, _ := newTestSupervisor(t, runner)
	require.NoError(t, supervisor.Authorize(context.Background(), validAuthorization()))
	runner.process.exit <- &DiagnosticError{Code: "endpoint-tls", ExitCode: 60}
	require.Eventually(t, func() bool { return supervisor.State() == StateUnhealthy }, time.Second, time.Millisecond)

	socketDir, err := os.MkdirTemp("/tmp", "wm-diagnostic-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	require.NoError(t, os.Chmod(socketDir, 0o700))
	socket := filepath.Join(socketDir, "control.sock")
	server := &Server{Supervisor: supervisor, SocketPath: socket, ExpectedRuntimeUID: "uid-a", VerifyPeer: func(*net.UnixConn) error { return nil }, IOTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	require.Eventually(t, func() bool { _, err := os.Stat(socket); return err == nil }, time.Second, time.Millisecond)

	_, err = (Client{SocketPath: socket, IOTimeout: time.Second}).Do(context.Background(), "health-ready", []byte("{}"))
	require.Error(t, err)
	assert.Equal(t, "endpoint-tls", DiagnosticCode(err))
	assert.NotContains(t, err.Error(), "AKIA-DO-NOT-LEAK")

	cancel()
	require.NoError(t, <-done)
}

func TestServerClientRedactsUnknownReadyError(t *testing.T) {
	runner := &fakeRunner{process: &fakeProcess{exit: make(chan error, 1)}}
	supervisor, _ := newTestSupervisor(t, runner)
	require.NoError(t, supervisor.Authorize(context.Background(), validAuthorization()))
	runner.process.exit <- errors.New("AKIA-DO-NOT-LEAK secret-value")
	require.Eventually(t, func() bool { return supervisor.State() == StateUnhealthy }, time.Second, time.Millisecond)

	socketDir, err := os.MkdirTemp("/tmp", "wm-redaction-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	require.NoError(t, os.Chmod(socketDir, 0o700))
	socket := filepath.Join(socketDir, "control.sock")
	server := &Server{Supervisor: supervisor, SocketPath: socket, ExpectedRuntimeUID: "uid-a", VerifyPeer: func(*net.UnixConn) error { return nil }, IOTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	require.Eventually(t, func() bool { _, err := os.Stat(socket); return err == nil }, time.Second, time.Millisecond)

	_, err = (Client{SocketPath: socket, IOTimeout: time.Second}).Do(context.Background(), "health-ready", []byte("{}"))
	require.Error(t, err)
	assert.Equal(t, "s3fs-exited", DiagnosticCode(err))
	assert.NotContains(t, err.Error(), "AKIA-DO-NOT-LEAK")
	assert.NotContains(t, err.Error(), "secret-value")

	cancel()
	require.NoError(t, <-done)
}

func TestServerErrorCodeRedactsUnknownInternalError(t *testing.T) {
	code := serverErrorCode(errors.New("AKIA-DO-NOT-LEAK secret-value"))
	assert.Equal(t, fuseprotocol.MounterErrorRejected, code)
	assert.NotContains(t, code, "AKIA-DO-NOT-LEAK")
}
