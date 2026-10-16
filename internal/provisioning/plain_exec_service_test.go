package provisioning

import (
	"context"
	"io"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/stretchr/testify/require"
)

func TestPlainExecRejectsInvalidPayloadBeforeBackend(t *testing.T) {
	s := &PlainColdService{config: launcherconfig.Config{UID: 1000, GID: 1000}, limit: 1, active: make(map[*plainColdCall]struct{})}
	s.self = s
	valid := p.ExecutionRequest{Argv: []string{"/bin/true"}, Env: map[string]string{}, UID: 1000, GID: 1000, WorkDir: "/", TimeoutSeconds: 45}
	for name, change := range map[string]func(*p.ExecutionRequest){
		"argv":    func(r *p.ExecutionRequest) { r.Argv = nil },
		"uid":     func(r *p.ExecutionRequest) { r.UID++ },
		"gid":     func(r *p.ExecutionRequest) { r.GID++ },
		"tty":     func(r *p.ExecutionRequest) { r.TTY = true },
		"network": func(r *p.ExecutionRequest) { r.RequiresNetwork = true },
		"workdir": func(r *p.ExecutionRequest) { r.WorkDir = "/../work" },
		"timeout": func(r *p.ExecutionRequest) { r.TimeoutSeconds = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			request := valid
			change(&request)
			result, err := s.Exec(context.Background(), "sandbox", "stable-request", request, io.Discard, io.Discard)
			require.Error(t, err, "nil concrete Backend must never be reached by rejected payload")
			require.Equal(t, PlainExecRejected, result.State)
			require.Equal(t, state.OperationReference{}, result.Operation)
			require.Equal(t, state.ExecEffectReference{}, result.Effect)
			require.Empty(t, result.Receipt.Wire(), "zero wait status is not successful result evidence")
			require.Empty(t, s.active)
		})
	}
}

func TestPlainExecSharesAdmissionAndKeepsLongDeadline(t *testing.T) {
	// Local lifecycle ownership only; this is not simulated exec success.
	s := &PlainColdService{limit: 1, active: make(map[*plainColdCall]struct{})}
	s.self = s
	call, err := s.enterFor(context.Background(), 75*time.Second)
	require.NoError(t, err)
	deadline, ok := call.context.Deadline()
	require.True(t, ok)
	require.Greater(t, time.Until(deadline), 70*time.Second)
	_, err = s.enter(context.Background())
	require.ErrorIs(t, err, state.ErrConflict, "Create and Lookup share Exec active budget")
	s.leave(call)
	old, err := s.enter(context.Background())
	require.NoError(t, err)
	deadline, ok = old.context.Deadline()
	require.True(t, ok)
	require.LessOrEqual(t, time.Until(deadline), 20*time.Second)
	require.Greater(t, time.Until(deadline), 19*time.Second)
	_, err = s.enterFor(context.Background(), 75*time.Second)
	require.ErrorIs(t, err, state.ErrConflict, "Exec shares Create and Lookup active budget")
	s.leave(old)
}
