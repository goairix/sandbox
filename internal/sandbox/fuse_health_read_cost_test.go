package sandbox

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
)

type countedFUSEHealthRuntime struct {
	*fuseManagerRuntime
	getCalls    atomic.Int64
	healthCalls atomic.Int64
}

func (r *countedFUSEHealthRuntime) GetSandbox(ctx context.Context, id string) (*runtime.SandboxInfo, error) {
	r.getCalls.Add(1)
	return r.fuseManagerRuntime.GetSandbox(ctx, id)
}

func (r *countedFUSEHealthRuntime) WorkspaceHealth(ctx context.Context, ref runtime.RuntimeRef) (*runtime.WorkspaceHealth, error) {
	r.healthCalls.Add(1)
	return r.fuseManagerRuntime.WorkspaceHealth(ctx, ref)
}

func TestDistributedFUSEHealthUsesAdmissionAndExactHealthReads(t *testing.T) {
	m, rt, _ := newDistributedFUSERecoveryManager(t)
	m.config.FUSEHealthInterval = time.Hour
	sb, err := m.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/health-reads"})
	require.NoError(t, err)

	m.mu.RLock()
	lifecycle := m.fuseLifecycles[sb.ID]
	m.mu.RUnlock()
	require.NotNil(t, lifecycle)

	// Admission already validates the running runtime identity. The health
	// operation must still check the exact UID before accepting readiness.
	counted := &countedFUSEHealthRuntime{fuseManagerRuntime: rt}
	m.runtime = counted
	require.NoError(t, m.checkFUSELifecycle(context.Background(), lifecycle))
	require.EqualValues(t, 1, counted.getCalls.Load())
	require.EqualValues(t, 1, counted.healthCalls.Load())

	originalUID := lifecycle.record.RuntimeUID
	lifecycle.record.RuntimeUID = "different-runtime-uid"
	defer func() { lifecycle.record.RuntimeUID = originalUID }()
	require.ErrorIs(t, m.checkFUSELifecycle(context.Background(), lifecycle), ErrSandboxNotReady)
	require.EqualValues(t, 2, counted.getCalls.Load(), "admission still inspects the runtime")
	require.EqualValues(t, 1, counted.healthCalls.Load(), "identity drift cannot reach health")
}
