//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/provisioning"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestExecServiceNative(t *testing.T) {
	if os.Getenv("SANDBOX_EXEC_SERVICE_NATIVE") != "1" {
		t.Skip("requires exact owned Exec service Linux fixture")
	}
	runColdCreateServiceFixtureNative(t, coldServiceNativeVariant{execService: true})
}

type execServiceNativeHooks struct {
	failNext atomic.Bool
	mu       sync.Mutex
	failed   map[*startupBridgeFixture]bool
}

func (h *execServiceNativeHooks) register(bridge *startupBridgeFixture) {
	if !h.failNext.Swap(false) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failed == nil {
		h.failed = make(map[*startupBridgeFixture]bool)
	}
	h.failed[bridge] = true
}

func (h *execServiceNativeHooks) expectedFailure(bridge *startupBridgeFixture) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.failed[bridge]
}

func requireExecServiceFailedBridgeJoined(t *testing.T, bridge *startupBridgeFixture) {
	t.Helper()
	bridge.Close()
	select {
	case <-bridge.done:
	case <-time.After(3 * time.Second):
		t.Fatal("original intentional-unknown bridge handler did not join")
	}
	bridge.wg.Wait()
	require.True(t, bridge.waited, "original Cmd.Wait must run even on unknown")
	t.Logf("exec unknown original bridge Cmd.Wait=%v relay=%v; original IO joined", bridge.waitErr, bridge.result)
}

type execServiceNativeWriter struct {
	output  bytes.Buffer
	marker  string
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (w *execServiceNativeWriter) Write(data []byte) (int, error) {
	n, err := w.output.Write(data)
	if strings.Contains(w.output.String(), w.marker) {
		w.once.Do(func() {
			close(w.started)
			if w.release != nil {
				<-w.release
			}
		})
	}
	return n, err
}

type execServiceNativeAnswer struct {
	result provisioning.PlainExecResult
	err    error
}

func nativeExecRequest(command string, timeout uint32) p.ExecutionRequest {
	return p.ExecutionRequest{Argv: []string{"/bin/sh", "-c", command}, Env: map[string]string{"PATH": "/usr/bin:/bin"}, UID: 1000, GID: 1000, WorkDir: "/workspace", TimeoutSeconds: timeout}
}

func exerciseExecServiceNative(t *testing.T, ctx context.Context, service *provisioning.PlainColdService, backend *state.Backend, admin *clientv3.Client, options provisioning.PlainColdOptions, ns state.Namespace, workspace state.WorkspaceIdentity, request provisioning.PlainColdRequest, created provisioning.PlainColdResult, hooks *execServiceNativeHooks) {
	t.Helper()
	invalid := nativeExecRequest("exit 0", 2)
	invalid.UID++
	rejected, err := service.Exec(ctx, request.SandboxID, uuid.NewString(), invalid, io.Discard, io.Discard)
	require.ErrorIs(t, err, provisioning.ErrPlainColdProfile)
	require.Equal(t, provisioning.PlainExecRejected, rejected.State)
	require.Empty(t, rejected.Operation.OperationID)
	otherOptions := options
	otherOptions.Disk = "2Mi"
	other, err := provisioning.NewPlainColdService(ctx, otherOptions)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	rejected, err = other.Exec(ctx, request.SandboxID, uuid.NewString(), nativeExecRequest("exit 0", 2), io.Discard, io.Discard)
	require.ErrorIs(t, err, provisioning.ErrPlainColdProfile)
	require.Equal(t, provisioning.PlainExecRejected, rejected.State)
	require.NoError(t, other.Close())
	mutation, err := backend.BeginOperation(ctx, state.BeginOperationInput{SandboxID: request.SandboxID, RequestID: uuid.NewString(), Kind: state.OperationMutation})
	require.NoError(t, err)
	require.NotNil(t, mutation.Capability)
	rejected, err = service.Exec(ctx, request.SandboxID, uuid.NewString(), nativeExecRequest("exit 0", 2), io.Discard, io.Discard)
	require.Error(t, err, "original Backend mutation gate rejects new data admission")
	require.NotEqual(t, provisioning.PlainExecLocalTerminal, rejected.State)
	require.Empty(t, rejected.Effect.CommandID)
	require.NoError(t, backend.CancelOperationCapability(ctx, mutation.Capability))

	stdout := &execServiceNativeWriter{marker: "exec-long-started", started: make(chan struct{})}
	var stderr bytes.Buffer
	longRequest := nativeExecRequest("printf 'exec-long-started\\n'; /bin/sleep 32; printf 'exec-long-finished\\n'; printf 'exec-stderr\\n' >&2; exit 7", 45)
	started := time.Now()
	answer := make(chan execServiceNativeAnswer, 1)
	go func() {
		result, err := service.Exec(ctx, request.SandboxID, "native-long-"+uuid.NewString(), longRequest, stdout, &stderr)
		answer <- execServiceNativeAnswer{result, err}
	}()
	select {
	case <-stdout.started:
	case got := <-answer:
		t.Fatalf("actual long command did not emit started marker: state=%s error=%v", got.result.State, got.err)
	case <-ctx.Done():
		t.Fatal("actual long command did not start")
	}
	var long execServiceNativeAnswer
	select {
	case long = <-answer:
	case <-ctx.Done():
		t.Fatal("actual long execution or original Wait owner did not join")
	}
	require.NoError(t, long.err)
	require.Greater(t, time.Since(started), 30*time.Second)
	require.Equal(t, provisioning.PlainExecLocalTerminal, long.result.State)
	receipt := long.result.Receipt
	require.Equal(t, "local_terminal", receipt.State())
	require.Equal(t, uint32(7<<8), receipt.RootWaitStatus())
	require.Positive(t, receipt.RootPID())
	require.True(t, receipt.DrainConfirmed())
	require.True(t, receipt.AuthorityDeadline().After(receipt.NotAfter()), "verified target terminal carries an actually accepted extended renewal deadline")
	require.Equal(t, created.Runtime, receipt.Context().Runtime)
	require.Equal(t, long.result.Operation.OperationID, receipt.Context().OperationID)
	require.NoError(t, long.result.Operation.Validate())
	require.NoError(t, long.result.Effect.Validate())
	require.Contains(t, stdout.output.String(), "exec-long-finished")
	require.Equal(t, "exec-stderr\n", stderr.String())
	effect, err := backend.LoadExecEffect(ctx, long.result.Effect)
	require.NoError(t, err)
	require.NotNil(t, effect)
	require.Equal(t, long.result.Effect.CommandID, effect.Record.CommandID)
	t.Log("exec service actual long command local_terminal exit7 drain and accepted renewal verified")

	// A same-value rewrite invalidates the exact original operation fence. Its
	// real next renewal error must remain unknown even while target can run.
	controlKey := ns.Root() + fmt.Sprintf("p/%02x/controls/%s", workspace.Partition(), request.SandboxID)
	control, err := admin.Get(ctx, controlKey)
	require.NoError(t, err)
	require.Len(t, control.Kvs, 1)
	runUnknown := func(name string, secondary *provisioning.PlainColdService, closeService bool, fenceLoss bool) {
		t.Helper()
		callCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		marker := "exec-" + name + "-started"
		writer := &execServiceNativeWriter{marker: marker, started: make(chan struct{})}
		var release chan struct{}
		var releaseOnce sync.Once
		if closeService {
			release = make(chan struct{})
			writer.release = release
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		}
		seconds := 2
		if fenceLoss {
			seconds = 8
		}
		hooks.failNext.Store(true)
		done := make(chan execServiceNativeAnswer, 1)
		go func() {
			result, err := secondary.Exec(callCtx, request.SandboxID, uuid.NewString(), nativeExecRequest(fmt.Sprintf("printf '%s\\n'; /bin/sleep %d; exit 0", marker, seconds), 15), writer, io.Discard)
			done <- execServiceNativeAnswer{result, err}
		}()
		select {
		case <-writer.started:
		case got := <-done:
			t.Fatalf("%s actual command did not start: state=%s error=%v", name, got.result.State, got.err)
		case <-ctx.Done():
			t.Fatalf("%s actual command start timeout", name)
		}
		var closed chan error
		if fenceLoss {
			_, err := admin.Put(ctx, controlKey, string(control.Kvs[0].Value))
			require.NoError(t, err)
		} else if closeService {
			_, err := secondary.Lookup(ctx, request.SandboxID)
			require.ErrorIs(t, err, state.ErrConflict, "actual Lookup shares active Exec budget")
			_, err = secondary.Create(ctx, workspace, request)
			require.ErrorIs(t, err, state.ErrConflict, "actual Create shares active Exec budget")
			_, err = secondary.Exec(ctx, request.SandboxID, uuid.NewString(), nativeExecRequest("exit 0", 2), io.Discard, io.Discard)
			require.ErrorIs(t, err, state.ErrConflict, "actual second Exec shares original active budget")
			closed = make(chan error, 1)
			go func() { closed <- secondary.Close() }()
			select {
			case <-closed:
				t.Fatal("Close returned before original synchronous writer owner joined")
			case <-time.After(50 * time.Millisecond):
			}
			releaseOnce.Do(func() { close(release) })
		} else {
			cancel()
		}
		var got execServiceNativeAnswer
		select {
		case got = <-done:
		case <-ctx.Done():
			t.Fatalf("%s original Exec/worker/Wait owners did not join", name)
		}
		require.Error(t, got.err)
		require.Equal(t, provisioning.PlainExecUnknown, got.result.State)
		require.NoError(t, got.result.Operation.Validate())
		require.NoError(t, got.result.Effect.Validate())
		require.NotEqual(t, "local_terminal", got.result.Receipt.State())
		if fenceLoss {
			require.ErrorIs(t, got.err, state.ErrConflict, "actual failed original renewal cannot become success")
		}
		if closed != nil {
			require.NoError(t, <-closed)
		}
		t.Logf("exec service actual %s returned unknown with original references and all local owners joined: %v", name, got.err)
	}
	runUnknown("renewal-fence-loss", service, false, true)
	runUnknown("caller-cancel", service, false, false)
	secondaryOptions := options
	secondaryOptions.API.MaxActive = 1
	secondary, err := provisioning.NewPlainColdService(ctx, secondaryOptions)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, secondary.Close()) })
	runUnknown("service-close", secondary, true, false)

	view, err := backend.ReadSandboxView(ctx, request.SandboxID)
	require.NoError(t, err)
	history, err := backend.ReadSandboxRuntime(ctx, view)
	require.NoError(t, err)
	current := view.Records().Control
	closeLiveLookupNativeGate(t, ctx, options, history.Publication, *current, created.Runtime)
	hooks.failNext.Store(true)
	rejected, err = service.Exec(ctx, request.SandboxID, uuid.NewString(), nativeExecRequest("exit 0", 2), io.Discard, io.Discard)
	require.Error(t, err, "original target gate rejects real service Start")
	require.NotEqual(t, provisioning.PlainExecLocalTerminal, rejected.State)
	require.NotEqual(t, "local_terminal", rejected.Receipt.State())
	t.Log("exec service fixed profile, Backend gate, renewal failure, cancel, Close, capacity and target gate verified")
}
