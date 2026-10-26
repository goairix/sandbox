package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

type rejectingFUSEBoundStore struct {
	*atomicMemoryStore
	boundSetCalls int
	boundNXCalls  int
}

func (s *rejectingFUSEBoundStore) SetFUSEBound(_ context.Context, _ string, _ []byte, _ time.Duration, _ state.FUSEPoolPublication) error {
	s.boundSetCalls++
	return state.ErrFUSEPoolConflict
}

func (s *rejectingFUSEBoundStore) SetNXFUSEBound(_ context.Context, _ string, _ []byte, _ time.Duration, _ state.FUSEPoolPublication) (bool, error) {
	s.boundNXCalls++
	return false, state.ErrFUSEPoolConflict
}

func TestFUSESessionPublicationUsesPoolFence(t *testing.T) {
	store := &rejectingFUSEBoundStore{atomicMemoryStore: newAtomicMemoryStore()}
	sessions := NewSessionStore(store, time.Hour)
	sandbox := &Sandbox{
		ID: "sandbox-bound", RuntimeID: "runtime-bound", RuntimeUID: "uid-bound", CreatedAt: time.Now(),
		Workspace: &WorkspaceInfo{MountType: WorkspaceMountFUSE, FUSEPreparationID: "prep-bound",
			FUSEPoolKey: "pool-bound", FUSEReservationToken: "reservation-bound"},
	}
	err := sessions.Save(context.Background(), sandbox)
	require.ErrorIs(t, err, state.ErrFUSEPoolConflict)
	require.Equal(t, 1, store.boundSetCalls)
	require.False(t, store.hasKey(sandboxSessionKeyPrefix+sandbox.ID))
}

func TestFUSEEphemeralPublicationUsesPoolFence(t *testing.T) {
	store := &rejectingFUSEBoundStore{atomicMemoryStore: newAtomicMemoryStore()}
	ephemeral := NewEphemeralLifecycleStore(store)
	record := validEphemeralRecord()
	record.MountType = WorkspaceMountFUSE
	record.Owner.MountType = WorkspaceMountFUSE
	record.Owner.MountAttempt = 1
	record.PreparationID = "prep-bound"
	record.PoolKey = "pool-bound"
	record.ReservationToken = "reservation-bound"
	record.PoolRevision = 1
	err := ephemeral.Create(context.Background(), record)
	require.ErrorIs(t, err, state.ErrFUSEPoolConflict)
	require.Equal(t, 1, store.boundNXCalls)
	require.False(t, store.hasKey(ephemeralLifecycleKeyPrefix+record.SandboxID))
}

type rejectingFUSEBoundActive struct {
	*memoryActiveRepository
	boundCalls int
}

type requiredFUSEPublicationRepository struct{ state.FUSEPoolRepository }

func (requiredFUSEPublicationRepository) RequiresFUSEPublicationFence() {}

type hiddenFUSEPublicationRepository struct{ state.FUSEPoolRepository }

func TestEtcdFUSECreateRejectsMissingPublicationFenceBeforeAcquire(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    Mode
		missing string
		hidden  bool
	}{
		{name: "session", mode: ModePersistent, missing: "session"},
		{name: "ephemeral", mode: ModeEphemeral, missing: "ephemeral"},
		{name: "active", mode: ModePersistent, missing: "active"},
		{name: "wrapped pool and session", mode: ModePersistent, missing: "session", hidden: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, repo, store := newFUSETestManager(t, newFUSEManagerRuntime())
			m.fusePool.repo = requiredFUSEPublicationRepository{FUSEPoolRepository: repo}
			if tc.hidden {
				m.fusePool.repo = hiddenFUSEPublicationRepository{FUSEPoolRepository: repo}
				m.fusePool.config.PublicationFenceRequired = true
			}
			m.config.RuntimeType = "kubernetes"
			m.SetSessionStore(NewSessionStore(&rejectingFUSEBoundStore{atomicMemoryStore: store}, time.Hour))
			m.SetEphemeralLifecycleStore(NewEphemeralLifecycleStore(&rejectingFUSEBoundStore{atomicMemoryStore: store}))
			m.activeSandboxes = &rejectingFUSEBoundActive{memoryActiveRepository: newMemoryActiveRepository()}
			switch tc.missing {
			case "session":
				m.SetSessionStore(NewSessionStore(store, time.Hour))
			case "ephemeral":
				m.SetEphemeralLifecycleStore(NewEphemeralLifecycleStore(store))
			case "active":
				m.activeSandboxes = newMemoryActiveRepository()
			}
			_, err := m.Create(context.Background(), SandboxConfig{
				Mode: tc.mode, WorkspacePath: "team/publication-fence", WorkspaceMountMode: WorkspaceMountFUSE,
			})
			require.ErrorIs(t, err, ErrInvalidFUSEPoolConfig)
			inventory, err := repo.ListByPoolKey(context.Background(), "pool-key")
			require.NoError(t, err)
			require.Len(t, inventory, 1)
			require.Equal(t, state.FUSEPoolPrepared, inventory[0].State)
		})
	}
}

func (r *rejectingFUSEBoundActive) PublishFUSEBound(_ context.Context, _ state.ActiveSandboxRecord, _ state.FUSEPoolPublication) error {
	r.boundCalls++
	return state.ErrFUSEPoolConflict
}

func TestFUSEActivePublicationUsesPoolFence(t *testing.T) {
	m, _, _, _ := newFUSETestManager(t, newFUSEManagerRuntime())
	m.config.RuntimeType = "kubernetes"
	repository := &rejectingFUSEBoundActive{memoryActiveRepository: newMemoryActiveRepository()}
	m.activeSandboxes = repository
	sandbox := &Sandbox{
		ID: "sandbox-bound", RuntimeID: "runtime-bound", RuntimeUID: "uid-bound", CreatedAt: time.Now(),
		Workspace: &WorkspaceInfo{MountType: WorkspaceMountFUSE, FUSEPreparationID: "prep-bound",
			FUSEPoolKey: "pool-bound", FUSEReservationToken: "reservation-bound"},
	}
	_, err := m.publishActiveSandboxState(context.Background(), sandbox, false)
	require.True(t, errors.Is(err, state.ErrFUSEPoolConflict))
	require.Equal(t, 1, repository.boundCalls)
	record, loadErr := repository.Load(context.Background(), sandbox.ID)
	require.NoError(t, loadErr)
	require.Nil(t, record)
}
