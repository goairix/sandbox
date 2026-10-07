//go:build linux && (amd64 || arm64)

package etcd

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	transport "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/stretchr/testify/require"
)

func nativeBootstrapRefused(t *testing.T, request transport.BootstrapRequest) {
	t.Helper()
	raw, err := net.DialTimeout("unix", transport.ControlSocket, time.Second)
	require.NoError(t, err)
	defer raw.Close()
	require.NoError(t, raw.SetDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, transport.WriteBootstrap(raw, request))
	require.NoError(t, raw.(*net.UnixConn).CloseWrite())
	var response transport.BootstrapResponse
	require.Error(t, transport.ReadBootstrap(raw, &response))
}

// Only the new finite bridge-caller selector uses this test-parent pipe adapter.
// It contains no target selector, credentials or command material.
type nativeBridgePipe struct {
	input, output        *os.File
	cmd                  *exec.Cmd
	stderr               *nativeBoundedStderr
	cancel               context.CancelFunc
	done                 chan struct{}
	waitErr              error
	inputOnce, closeOnce sync.Once
	inputErr             error
	drainBytes           int64
	drainErr             error
}

func (c *nativeBridgePipe) Read(b []byte) (int, error)  { return c.output.Read(b) }
func (c *nativeBridgePipe) Write(b []byte) (int, error) { return c.input.Write(b) }
func (c *nativeBridgePipe) CloseWrite() error {
	c.inputOnce.Do(func() { c.inputErr = c.input.Close() })
	return c.inputErr
}
func (c *nativeBridgePipe) Close() error {
	c.closeOnce.Do(func() {
		c.CloseWrite()
		// A verified terminal frame is not raw pipe EOF. Drain only this owned
		// response pipe, without retaining a transcript, before closing its read
		// side. Deadline and byte ceiling bound cancellation/hostile output too.
		if err := c.output.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
			c.cancel()
		}
		c.drainBytes, c.drainErr = io.CopyN(io.Discard, c.output, 65537)
		if c.drainErr != io.EOF {
			c.cancel()
		}
		c.output.Close()
		<-c.done
		c.cancel()
	})
	return c.waitErr
}
func (c *nativeBridgePipe) LocalAddr() net.Addr {
	return &net.UnixAddr{Name: "owned-backend-stdin", Net: "pipe"}
}
func (c *nativeBridgePipe) RemoteAddr() net.Addr {
	return &net.UnixAddr{Name: "fixed-production-bridge", Net: "pipe"}
}
func (c *nativeBridgePipe) SetDeadline(d time.Time) error {
	if err := c.input.SetWriteDeadline(d); err != nil {
		return err
	}
	return c.output.SetReadDeadline(d)
}
func (c *nativeBridgePipe) SetReadDeadline(d time.Time) error  { return c.output.SetReadDeadline(d) }
func (c *nativeBridgePipe) SetWriteDeadline(d time.Time) error { return c.input.SetWriteDeadline(d) }
func startNativeBridge(parent context.Context) (*nativeBridgePipe, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	childInput, input, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, err
	}
	output, childOutput, err := os.Pipe()
	if err != nil {
		childInput.Close()
		input.Close()
		cancel()
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "/native/sandbox-launcher", "bridge")
	stderr := &nativeBoundedStderr{}
	cmd.Env = []string{"GOMAXPROCS=2"}
	cmd.Stdin = childInput
	cmd.Stdout = childOutput
	cmd.Stderr = stderr
	if err = cmd.Start(); err != nil {
		childInput.Close()
		input.Close()
		output.Close()
		childOutput.Close()
		cancel()
		return nil, err
	}
	childInput.Close()
	childOutput.Close()
	c := &nativeBridgePipe{input: input, output: output, cmd: cmd, stderr: stderr, cancel: cancel, done: make(chan struct{})}
	// This is the sole original Wait; Close joins it and os/exec's stderr/context
	// workers. The finite drain interval is local cleanup, never target authority.
	go func() { defer close(c.done); c.waitErr = cmd.Wait() }()
	return c, nil
}

// os/exec owns its sole stderr copy worker; the single original Wait joins it.
type nativeBoundedStderr struct {
	bytes.Buffer
	truncated bool
}

func (w *nativeBoundedStderr) Write(b []byte) (int, error) {
	n := len(b)
	left := 32768 - w.Len()
	if len(b) > left {
		w.truncated = true
		b = b[:left]
	}
	w.Buffer.Write(b)
	return n, nil
}
func nativeBackendBridgeCost(t *testing.T, child int) {
	t.Helper()
	values := map[string]string{}
	for _, path := range []string{"/proc/self/status", "/sys/fs/cgroup/memory.current", "/sys/fs/cgroup/memory.peak", "/sys/fs/cgroup/pids.current", "/sys/fs/cgroup/cpu.stat"} {
		f, err := os.Open(path)
		require.NoError(t, err)
		b, err := io.ReadAll(io.LimitReader(f, 16385))
		f.Close()
		require.NoError(t, err)
		require.LessOrEqual(t, len(b), 16384)
		values[path] = string(b)
	}
	wire, err := json.Marshal(values)
	require.NoError(t, err)
	t.Logf("ACTUAL_BACKEND_BRIDGE_ACTIVE_COST backend_pid=%d bridge_pid=%d observed_at=%s fixed_own_files=%s", os.Getpid(), child, time.Now().UTC().Format(time.RFC3339Nano), wire)
}
func assertNativeBridgeJoined(t *testing.T, role string, c *nativeBridgePipe) {
	t.Helper()
	err := c.Close()
	code := -1
	if c.cmd.ProcessState != nil {
		code = c.cmd.ProcessState.ExitCode()
	}
	t.Logf("ACTUAL_BRIDGE_CHILD_JOINED role=%s pid=%d exit=%d drain_bytes=%d drain_error=%v stderr_bytes=%d truncated=%t wait_error=%v", role, c.cmd.Process.Pid, code, c.drainBytes, c.drainErr, c.stderr.Len(), c.stderr.truncated, err)
	require.NoError(t, err)
	require.Zero(t, code)
	require.ErrorIs(t, c.drainErr, io.EOF)
	require.False(t, c.stderr.truncated)
	require.Empty(t, c.stderr.String())
}
func TestAuthenticatedTargetNativeBridge(t *testing.T) {
	f := nativeExecutionSetupWithActivationChecks(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	credential, err := p.NewManagementTLSCredential(f.secret.Issuer, f.secret.IssuerWire, "command_issuer")
	require.NoError(t, err)
	cfg, err := p.NewManagementTLSConfig(p.ManagementTLSOptions{Verifier: f.b.execVerifier, Clock: f.clock, Credential: credential, PeerCertificate: f.runtimeWire, PeerRuntime: &f.identity})
	require.NoError(t, err)
	pipe, err := startNativeBridge(ctx)
	require.NoError(t, err)
	defer pipe.Close()
	conn := tls.Client(pipe, cfg)
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, conn.HandshakeContext(ctx))
	nativeBackendBridgeCost(t, pipe.cmd.Process.Pid)
	require.NoError(t, transport.WriteRequest(conn, transport.Envelope{Version: 1, Purpose: "exec_inspect"}))
	require.NoError(t, conn.CloseWrite())
	require.NoError(t, pipe.CloseWrite())
	kind, wire, err := transport.ReadEvent(conn)
	require.NoError(t, err)
	require.Equal(t, transport.EventDiagnostics, kind)
	require.LessOrEqual(t, len(wire), 16384)
	var diagnostic map[string]any
	require.NoError(t, json.Unmarshal(wire, &diagnostic))
	require.Equal(t, true, diagnostic["complete"])
	require.Equal(t, float64(1), diagnostic["pid"])
	require.Equal(t, f.identity.Runtime.BootID, diagnostic["boot_id"])
	require.Equal(t, "open", diagnostic["gate"])
	assertNativeBridgeJoined(t, "inspect", pipe)
	fmt.Printf("ACTUAL_PRODUCTION_BRIDGE_INSPECT_EXIT0 child_pid=%d exact_argv=%q diagnostic=%s\n", pipe.cmd.Process.Pid, pipe.cmd.Args, wire)
	var children []*nativeBridgePipe
	destination, err := transport.NewDestination(transport.DestinationOptions{Identity: f.identity, RuntimeCertificate: f.runtimeWire, Credential: credential, Verifier: f.b.execVerifier, Clock: f.clock, Dial: func(dial context.Context) (net.Conn, error) {
		if err := dial.Err(); err != nil {
			return nil, err
		}
		child, err := startNativeBridge(ctx)
		if err != nil {
			return nil, err
		}
		children = append(children, child)
		return child, nil
	}})
	require.NoError(t, err)
	defer func() {
		for _, child := range children {
			child.Close()
		}
	}()
	request := nativeRequest("probe", 5)
	request.Stdin = []byte{0, 1, 255, 10}
	_, prepared := f.prepare(t, request)
	h, err := f.b.DeliverExecEffect(ctx, prepared, destination)
	require.NoError(t, err)
	defer h.Close()
	require.Len(t, children, 1)
	nativeBackendBridgeCost(t, children[0].cmd.Process.Pid)
	var stdout, stderr bytes.Buffer
	receipt, err := h.Wait(ctx, &stdout, &stderr)
	require.NoError(t, err)
	require.Equal(t, "local_terminal", receipt.State())
	require.True(t, receipt.DrainConfirmed())
	require.Zero(t, receipt.RootWaitStatus())
	var actual struct {
		Stdin    []byte
		UID, GID int
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &actual))
	require.Equal(t, request.Stdin, actual.Stdin)
	require.Equal(t, 1000, actual.UID)
	require.Equal(t, 1000, actual.GID)
	require.Contains(t, stderr.String(), "STDERR_MARKER")
	queried, err := h.Wait(ctx, io.Discard, io.Discard)
	require.NoError(t, err)
	require.Equal(t, receipt.Wire(), queried.Wire())
	require.Len(t, children, 2)
	require.NotEqual(t, pipe.cmd.Process.Pid, children[0].cmd.Process.Pid)
	require.NotEqual(t, pipe.cmd.Process.Pid, children[1].cmd.Process.Pid)
	require.NotEqual(t, children[0].cmd.Process.Pid, children[1].cmd.Process.Pid)
	for i, child := range children {
		assertNativeBridgeJoined(t, []string{"prepared-delivery", "same-command-query"}[i], child)
	}
	t.Logf("ACTUAL_ORIGINAL_PREPARED_BRIDGE_EXEC_AND_QUERY command=%s root_pid=%d receipt=%s stdout=%s stderr=%s", prepared.draft.commandID, receipt.RootPID(), receipt.Wire(), stdout.Bytes(), stderr.Bytes())
}
