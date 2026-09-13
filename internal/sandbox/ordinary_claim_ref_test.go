package sandbox

import (
	"context"
	"testing"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
)

type exactManagerClaimRuntime struct {
	*sharedPoolRuntime
	claimRef  runtime.RuntimeRef
	removeRef runtime.RuntimeRef
}

func (r *exactManagerClaimRuntime) ClaimOrdinaryPool(_ context.Context, ref runtime.RuntimeRef, _ string) error {
	r.claimRef = ref
	return runtime.ErrInvalidRuntimeRef
}

func (r *exactManagerClaimRuntime) RemoveOrdinarySandbox(_ context.Context, ref runtime.RuntimeRef) error {
	r.removeRef = ref
	return runtime.ErrInvalidRuntimeRef
}

func TestManagerOrdinaryClaimAndFailureCleanupUseAcquiredRef(t *testing.T) {
	ctx := context.Background()
	r := &exactManagerClaimRuntime{sharedPoolRuntime: newSharedPoolRuntime()}
	m := NewManager(r, nil, nil, ManagerConfig{RuntimeType: "kubernetes", PoolConfig: PoolConfig{MinSize: 1, MaxSize: 1, Image: "sandbox:latest"}, PoolStateStore: newAtomicMemoryStore(), PoolScope: "exact-claim"})
	require.NoError(t, m.Start(ctx))
	t.Cleanup(func() { require.NoError(t, m.Stop(ctx)); require.NoError(t, m.pool.DrainRelease(ctx)) })
	_, err := m.Create(ctx, SandboxConfig{Mode: ModeEphemeral})
	require.ErrorIs(t, err, runtime.ErrInvalidRuntimeRef)
	require.NotEmpty(t, r.claimRef.UID)
	require.Equal(t, r.claimRef, r.removeRef, "failed claim must not delete a replacement by runtime name")
	current, err := r.GetSandbox(ctx, r.claimRef.ID)
	require.NoError(t, err)
	require.Equal(t, current.RuntimeUID, r.claimRef.UID)
}
