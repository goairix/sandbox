package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
)

func newExecTestManager(rt *mockRuntime, cfg ManagerConfig) *Manager {
	mgr := NewManager(rt, nil, nil, cfg)
	mgr.sandboxes["sandbox-test"] = &Sandbox{
		ID:        "sandbox-test",
		RuntimeID: "runtime-test",
		State:     StateReady,
		Config: SandboxConfig{
			Network: NetworkConfig{Enabled: true},
		},
	}
	return mgr
}

func TestManagerResolveExecTimeout(t *testing.T) {
	mgr := NewManager(newMockRuntime(), nil, nil, ManagerConfig{
		ExecTimeoutSeconds:    30,
		MaxExecTimeoutSeconds: 600,
	})

	tests := []struct {
		name      string
		requested int
		want      int
		wantErr   bool
	}{
		{name: "default", requested: 0, want: 30},
		{name: "request override", requested: 300, want: 300},
		{name: "exceeds maximum", requested: 601, wantErr: true},
		{name: "negative", requested: -1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mgr.resolveExecTimeout(tt.requested)
			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrInvalidExecTimeout)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestManagerResolveExecTimeoutWithoutMaximum(t *testing.T) {
	mgr := NewManager(newMockRuntime(), nil, nil, ManagerConfig{
		ExecTimeoutSeconds: 30,
	})

	got, err := mgr.resolveExecTimeout(601)

	require.NoError(t, err)
	assert.Equal(t, 601, got)
}

func TestManagerExecPropagatesEffectiveTimeout(t *testing.T) {
	tests := []struct {
		name      string
		requested int
		want      int
	}{
		{name: "default timeout", requested: 0, want: 30},
		{name: "request timeout", requested: 300, want: 300},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := newMockRuntime()
			mgr := newExecTestManager(rt, ManagerConfig{
				ExecTimeoutSeconds:    30,
				MaxExecTimeoutSeconds: 600,
			})

			_, err := mgr.Exec(context.Background(), "sandbox-test", runtime.ExecRequest{
				Command: "true",
				Timeout: tt.requested,
			})

			require.NoError(t, err)
			_, gotReq := rt.lastExec()
			assert.Equal(t, tt.want, gotReq.Timeout)
		})
	}
}

func TestManagerExecRejectsInvalidTimeout(t *testing.T) {
	rt := newMockRuntime()
	mgr := newExecTestManager(rt, ManagerConfig{
		ExecTimeoutSeconds:    30,
		MaxExecTimeoutSeconds: 600,
	})

	_, err := mgr.Exec(context.Background(), "sandbox-test", runtime.ExecRequest{
		Command: "true",
		Timeout: 601,
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidExecTimeout)
	ctx, _ := rt.lastExec()
	assert.Nil(t, ctx)
}

func TestManagerExecTimeout(t *testing.T) {
	rt := newMockRuntime()
	rt.execFunc = func(ctx context.Context, _ string, _ runtime.ExecRequest) (*runtime.ExecResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	mgr := newExecTestManager(rt, ManagerConfig{
		ExecTimeoutSeconds:    1,
		MaxExecTimeoutSeconds: 600,
	})

	_, err := mgr.Exec(context.Background(), "sandbox-test", runtime.ExecRequest{Command: "sleep"})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrExecTimeout)
	sb, getErr := mgr.Get(context.Background(), "sandbox-test")
	require.NoError(t, getErr)
	assert.Equal(t, StateError, sb.State)
}

func TestManagerExecCallerCancellationIsNotTimeout(t *testing.T) {
	rt := newMockRuntime()
	rt.execFunc = func(ctx context.Context, _ string, _ runtime.ExecRequest) (*runtime.ExecResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	mgr := newExecTestManager(rt, ManagerConfig{
		ExecTimeoutSeconds:    30,
		MaxExecTimeoutSeconds: 600,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := mgr.Exec(ctx, "sandbox-test", runtime.ExecRequest{Command: "true"})

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrExecTimeout)
}

func TestManagerExecStreamContextLivesUntilStreamCompletion(t *testing.T) {
	rt := newMockRuntime()
	runtimeEvents := make(chan runtime.StreamEvent)
	rt.execStreamFunc = func(context.Context, string, runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
		return runtimeEvents, nil
	}
	mgr := newExecTestManager(rt, ManagerConfig{
		ExecTimeoutSeconds:    30,
		MaxExecTimeoutSeconds: 600,
	})

	out, err := mgr.ExecStream(context.Background(), "sandbox-test", runtime.ExecRequest{Command: "true"})
	require.NoError(t, err)
	runtimeCtx, gotReq := rt.lastStream()
	assert.Equal(t, 30, gotReq.Timeout)
	select {
	case <-runtimeCtx.Done():
		t.Fatal("stream context was canceled when ExecStream returned")
	default:
	}

	close(runtimeEvents)
	for range out {
	}
	require.Eventually(t, func() bool {
		select {
		case <-runtimeCtx.Done():
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
}

func TestManagerExecStreamTimeoutEmitsErrorAndCloses(t *testing.T) {
	rt := newMockRuntime()
	rt.execStreamFunc = func(context.Context, string, runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
		return make(chan runtime.StreamEvent), nil
	}
	mgr := newExecTestManager(rt, ManagerConfig{
		ExecTimeoutSeconds:    1,
		MaxExecTimeoutSeconds: 600,
	})

	out, err := mgr.ExecStream(context.Background(), "sandbox-test", runtime.ExecRequest{Command: "sleep"})
	require.NoError(t, err)

	select {
	case event, ok := <-out:
		require.True(t, ok)
		assert.Equal(t, runtime.StreamError, event.Type)
		assert.Equal(t, ErrExecTimeout.Error(), event.Content)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stream timeout event")
	}
	_, ok := <-out
	assert.False(t, ok)
	sb, getErr := mgr.Get(context.Background(), "sandbox-test")
	require.NoError(t, getErr)
	assert.Equal(t, StateError, sb.State)
}

func TestManagerExecStreamNormalCompletionSetsIdle(t *testing.T) {
	rt := newMockRuntime()
	runtimeEvents := make(chan runtime.StreamEvent, 1)
	runtimeEvents <- runtime.StreamEvent{Type: runtime.StreamDone, Content: "0"}
	close(runtimeEvents)
	rt.execStreamFunc = func(context.Context, string, runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
		return runtimeEvents, nil
	}
	mgr := newExecTestManager(rt, ManagerConfig{
		ExecTimeoutSeconds:    30,
		MaxExecTimeoutSeconds: 600,
	})

	out, err := mgr.ExecStream(context.Background(), "sandbox-test", runtime.ExecRequest{Command: "true"})
	require.NoError(t, err)
	var got []runtime.StreamEvent
	for event := range out {
		got = append(got, event)
	}

	assert.Equal(t, []runtime.StreamEvent{{Type: runtime.StreamDone, Content: "0"}}, got)
	sb, getErr := mgr.Get(context.Background(), "sandbox-test")
	require.NoError(t, getErr)
	assert.Equal(t, StateIdle, sb.State)
}

func TestManagerExecStreamInitializationFailureCancelsContext(t *testing.T) {
	rt := newMockRuntime()
	rt.execStreamFunc = func(context.Context, string, runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
		return nil, errors.New("stream init failed")
	}
	mgr := newExecTestManager(rt, ManagerConfig{
		ExecTimeoutSeconds:    30,
		MaxExecTimeoutSeconds: 600,
	})

	_, err := mgr.ExecStream(context.Background(), "sandbox-test", runtime.ExecRequest{Command: "true"})

	require.EqualError(t, err, "stream init failed")
	runtimeCtx, _ := rt.lastStream()
	select {
	case <-runtimeCtx.Done():
	default:
		t.Fatal("stream context was not canceled after initialization failed")
	}
}

func TestManager_CreateEphemeralSandbox(t *testing.T) {
	rt := newMockRuntime()
	mgr := NewManager(rt, nil, nil, ManagerConfig{
		PoolConfig:     PoolConfig{MinSize: 2, MaxSize: 10, Image: "sandbox:latest"},
		DefaultTimeout: 30,
	})

	ctx := context.Background()
	mgr.Start(ctx)
	defer mgr.Stop(ctx)

	time.Sleep(100 * time.Millisecond)

	sb, err := mgr.Create(ctx, SandboxConfig{
		Mode:    ModeEphemeral,
		Timeout: 30,
	})
	require.NoError(t, err)
	assert.Equal(t, StateReady, sb.State)
	assert.Equal(t, ModeEphemeral, sb.Config.Mode)
}

func TestManager_CreatePersistentSandbox(t *testing.T) {
	rt := newMockRuntime()
	mgr := NewManager(rt, nil, nil, ManagerConfig{
		PoolConfig:     PoolConfig{MinSize: 1, MaxSize: 5, Image: "sandbox:latest"},
		DefaultTimeout: 60,
	})

	ctx := context.Background()
	mgr.Start(ctx)
	defer mgr.Stop(ctx)

	time.Sleep(100 * time.Millisecond)

	sb, err := mgr.Create(ctx, SandboxConfig{
		Mode:    ModePersistent,
		Timeout: 60,
	})
	require.NoError(t, err)
	assert.Equal(t, StateReady, sb.State)
	assert.Equal(t, ModePersistent, sb.Config.Mode)

	// Should be retrievable by ID
	got, err := mgr.Get(ctx, sb.ID)
	require.NoError(t, err)
	assert.Equal(t, sb.ID, got.ID)
}

func TestManager_Destroy(t *testing.T) {
	rt := newMockRuntime()
	mgr := NewManager(rt, nil, nil, ManagerConfig{
		PoolConfig:     PoolConfig{MinSize: 1, MaxSize: 5, Image: "sandbox:latest"},
		DefaultTimeout: 30,
	})

	ctx := context.Background()
	mgr.Start(ctx)
	defer mgr.Stop(ctx)

	time.Sleep(100 * time.Millisecond)

	sb, err := mgr.Create(ctx, SandboxConfig{
		Mode: ModeEphemeral,
	})
	require.NoError(t, err)

	err = mgr.Destroy(ctx, sb.ID)
	require.NoError(t, err)

	_, err = mgr.Get(ctx, sb.ID)
	assert.Error(t, err)
}
