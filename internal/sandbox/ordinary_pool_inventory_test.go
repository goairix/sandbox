package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
)

type boundedOrdinaryInventoryRuntime struct {
	*sharedPoolRuntime
	rejectBroad   atomic.Bool
	rejectPublish atomic.Bool
}

func (r *boundedOrdinaryInventoryRuntime) PublishOrdinaryPoolPrepared(ctx context.Context, ref runtime.RuntimeRef, key, instance string) error {
	if r.rejectPublish.Load() {
		return fmt.Errorf("network identity is not ready")
	}
	return r.sharedPoolRuntime.PublishOrdinaryPoolPrepared(ctx, ref, key, instance)
}

func (r *boundedOrdinaryInventoryRuntime) ListSandboxes(ctx context.Context, labels map[string]string) ([]runtime.SandboxInfo, error) {
	if r.rejectBroad.Load() && labels["sandbox.pool.instance"] == "" {
		return nil, fmt.Errorf("refill must not scan active sandbox inventory")
	}
	return r.sharedPoolRuntime.ListSandboxes(ctx, labels)
}

func TestOrdinaryRecoveryCannotAdmitUnpublishedRuntime(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		t.Run(fmt.Sprintf("recorded=%t", recorded), func(t *testing.T) {
			ctx := context.Background()
			r := &boundedOrdinaryInventoryRuntime{sharedPoolRuntime: newSharedPoolRuntime()}
			store := newAtomicMemoryStore()
			p := NewPool(r, PoolConfig{MinSize: 0, MaxSize: 1, Image: "sandbox:v1"})
			p.EnableShared(store, "unpublished-recovery")
			require.NoError(t, p.Start(ctx))
			t.Cleanup(func() { p.Drain(ctx); require.NoError(t, p.DrainRelease(ctx)) })
			instance := randSuffix(24)
			_, err := r.CreateSandbox(ctx, runtime.SandboxSpec{ID: "unpublished", Labels: map[string]string{"sandbox.pool": "true", "sandbox.pool.key": p.shared.poolKey, "sandbox.pool.instance": instance, "sandbox.pool.state": "preparing"}})
			require.NoError(t, err)
			if recorded {
				raw, err := json.Marshal(ordinaryPoolRecord{PreparationID: instance, PoolKey: p.shared.poolKey, State: ordinaryPoolPreparing, UpdatedAt: time.Now().UTC(), Revision: 1})
				require.NoError(t, err)
				_, err = store.SetNX(ctx, p.shared.recordKey(instance), raw, 0)
				require.NoError(t, err)
			}
			r.rejectPublish.Store(true)
			require.Error(t, p.shared.reconcile(ctx, nil))
			entries, err := p.shared.listCurrentRecords(ctx)
			require.NoError(t, err)
			for _, entry := range entries {
				require.NotEqual(t, ordinaryPoolPrepared, entry.record.State)
			}
		})
	}
}

func TestOrdinaryRefillUsesRecordedRuntimeInventory(t *testing.T) {
	ctx := context.Background()
	r := &boundedOrdinaryInventoryRuntime{sharedPoolRuntime: newSharedPoolRuntime()}
	p := NewPool(r, PoolConfig{MinSize: 1, MaxSize: 1, Image: "sandbox:v1"})
	p.EnableShared(newAtomicMemoryStore(), "bounded-inventory")
	require.NoError(t, p.Start(ctx))
	t.Cleanup(func() { r.rejectBroad.Store(false); p.Drain(ctx); require.NoError(t, p.DrainRelease(ctx)) })
	require.NoError(t, p.WarmUp(ctx))
	r.rejectBroad.Store(true)
	require.NoError(t, p.shared.refill(ctx))
	require.Equal(t, 1, p.Size())
}
