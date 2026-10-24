package sandbox

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

type indexedLifecycleRepository struct {
	state.ActiveSandboxRepository
	fullScans        atomic.Int32
	dueScans         atomic.Int32
	workspaceScans   atomic.Int32
	withBadCandidate bool
	loaded           []string
}

func (r *indexedLifecycleRepository) Scan(context.Context, uint64, int64) (state.ActiveSandboxPage, error) {
	r.fullScans.Add(1)
	return state.ActiveSandboxPage{}, nil
}

func (r *indexedLifecycleRepository) ScanDue(_ context.Context, _ time.Time, cursor string, _ int64) (state.ActiveSandboxDuePage, error) {
	r.dueScans.Add(1)
	if r.withBadCandidate {
		if cursor == "" {
			return state.ActiveSandboxDuePage{Records: []state.ActiveSandboxRecord{{SandboxID: "bad"}}, Cursor: "next"}, nil
		}
		return state.ActiveSandboxDuePage{Records: []state.ActiveSandboxRecord{{SandboxID: "later"}}}, nil
	}
	return state.ActiveSandboxDuePage{}, nil
}

func (r *indexedLifecycleRepository) Load(_ context.Context, id string) (*state.ActiveSandboxRecord, error) {
	r.loaded = append(r.loaded, id)
	if id == "bad" {
		return nil, errors.New("one corrupt record")
	}
	return nil, nil
}

func (r *indexedLifecycleRepository) ScanWorkspace(context.Context, uint64, int64) (state.ActiveSandboxPage, error) {
	r.workspaceScans.Add(1)
	return state.ActiveSandboxPage{}, nil
}

func TestIndexedLifecycleTickDoesNotFullScanFarFutureRecords(t *testing.T) {
	repository := &indexedLifecycleRepository{}
	manager := &Manager{activeSandboxes: repository}
	require.NoError(t, manager.reconcileActiveLifecycleControllers(context.Background()))
	require.Zero(t, repository.fullScans.Load())
	require.Positive(t, repository.dueScans.Load())
	require.Positive(t, repository.workspaceScans.Load())
}

func TestIndexedLifecycleTickDoesNotStarveAfterOneBadCandidate(t *testing.T) {
	repository := &indexedLifecycleRepository{withBadCandidate: true}
	manager := &Manager{activeSandboxes: repository}
	require.NoError(t, manager.reconcileActiveLifecycleControllers(context.Background()))
	require.Equal(t, []string{"bad", "later"}, repository.loaded)
	require.Zero(t, repository.fullScans.Load())
}
