package mounter

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

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
	defer server.Close()
	defer client.Close()
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
