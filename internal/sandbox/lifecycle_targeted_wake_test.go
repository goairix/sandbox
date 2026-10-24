package sandbox

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

type targetedWakeRepository struct {
	state.ActiveSandboxRepository
	loaded chan string
	scans  atomic.Int32
}

func (r *targetedWakeRepository) BeginDestroy(_ context.Context, id string) (*state.ActiveSandboxRecord, int64, bool, error) {
	return &state.ActiveSandboxRecord{SandboxID: id, Phase: state.ActiveSandboxDestroying}, 0, true, nil
}

func (r *targetedWakeRepository) Load(_ context.Context, id string) (*state.ActiveSandboxRecord, error) {
	r.loaded <- id
	return nil, nil
}

func (r *targetedWakeRepository) Scan(context.Context, uint64, int64) (state.ActiveSandboxPage, error) {
	r.scans.Add(1)
	return state.ActiveSandboxPage{}, nil
}

func TestScheduleDestroyReconcilesExactSandboxWithoutFullScan(t *testing.T) {
	var instanceID string
	for i := 0; i < 100; i++ {
		candidate := fmt.Sprintf("api-%d", i)
		if lifecycleScanDelay(candidate) > 500*time.Millisecond {
			instanceID = candidate
			break
		}
	}
	require.NotEmpty(t, instanceID)

	repo := &targetedWakeRepository{loaded: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	manager := &Manager{
		config:          ManagerConfig{RuntimeType: "kubernetes", InstanceID: instanceID},
		activeSandboxes: repo,
		controlCtx:      ctx,
		cleanupWake:     make(chan string, 1),
	}
	manager.wg.Add(1)
	go manager.runActiveLifecycleCoordinator()
	t.Cleanup(func() {
		cancel()
		manager.wg.Wait()
	})

	require.NoError(t, manager.ScheduleDestroy(context.Background(), "sandbox-target"))
	select {
	case id := <-repo.loaded:
		require.Equal(t, "sandbox-target", id)
	case <-time.After(300 * time.Millisecond):
		t.Fatal("exact sandbox was not loaded promptly after ScheduleDestroy")
	}
	require.Zero(t, repo.scans.Load(), "a targeted destroy must not start a full scan")
}
