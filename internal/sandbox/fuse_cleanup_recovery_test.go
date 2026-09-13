package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage/state"
	redisstate "github.com/goairix/sandbox/internal/storage/state/redis"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func newDistributedFUSERecoveryManager(t *testing.T) (*Manager, *fuseManagerRuntime, *atomicMemoryStore) {
	t.Helper()
	rt := newFUSEManagerRuntime()
	m, _, _, store := newFUSETestManager(t, rt)
	m.config.RuntimeType = "kubernetes"
	m.config.InstanceID = "creator"
	m.activeSandboxes = newMemoryActiveRepository()
	m.config.ActiveSandboxes = m.activeSandboxes
	t.Cleanup(func() { require.NoError(t, m.Stop(context.Background())) })
	return m, rt, store
}

type failClaimReplyRepository struct {
	state.FUSEPoolRepository
	failed    bool
	targetUID string
}

func (r *failClaimReplyRepository) ClaimCleanup(ctx context.Context, id string, from state.FUSEPoolState, maintainer, reservation string, revision uint64, runtimeID, uid, token string, ttl time.Duration) (*state.FUSEPoolRecord, error) {
	record, err := r.FUSEPoolRepository.ClaimCleanup(ctx, id, from, maintainer, reservation, revision, runtimeID, uid, token, ttl)
	if err == nil && uid == r.targetUID && !r.failed {
		r.failed = true
		return nil, errors.New("crash after cleanup claim committed")
	}
	return record, err
}

type failRemovalReplyRuntime struct {
	*fuseManagerRuntime
	failed    bool
	targetUID string
}

func (r *failRemovalReplyRuntime) FinalizePreparedSandboxRemoval(ctx context.Context, id, uid string, evidence runtime.TerminationEvidence) error {
	if err := r.fuseManagerRuntime.FinalizePreparedSandboxRemoval(ctx, id, uid, evidence); err != nil {
		return err
	}
	if uid == r.targetUID && !r.failed {
		r.failed = true
		return errors.New("crash after Pod removal")
	}
	return nil
}

type failSessionRemovalStore struct{ *atomicMemoryStore }

type failedFUSECheckpointAckRepository struct {
	*memoryActiveRepository
	ackErr error
}

func (r failedFUSECheckpointAckRepository) CheckpointController(ctx context.Context, lease state.ActiveSandboxControllerLease, revision uint64, checkpoint string) (*state.ActiveSandboxRecord, error) {
	updated, err := r.memoryActiveRepository.CheckpointController(ctx, lease, revision, checkpoint)
	if err != nil {
		return updated, err
	}
	return updated, r.ackErr
}

func TestDistributedFUSECheckpointRequiresDurableAck(t *testing.T) {
	for _, ackErr := range []error{state.ErrDurabilityUnconfirmed, errors.New("checkpoint response lost")} {
		t.Run(ackErr.Error(), func(t *testing.T) {
			m, rt, _ := newDistributedFUSERecoveryManager(t)
			ctx := context.Background()
			sb, err := m.Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/checkpoint-ack"})
			require.NoError(t, err)
			base := m.activeSandboxes.(*memoryActiveRepository)
			m.activeSandboxes = failedFUSECheckpointAckRepository{memoryActiveRepository: base, ackErr: ackErr}
			m.mu.RLock()
			lifecycle := m.fuseLifecycles[sb.ID]
			m.mu.RUnlock()
			lifecycle.controller.repository = m.activeSandboxes
			m.teardownFUSESandbox(lifecycle, ErrSandboxCleanupPending)
			require.False(t, rt.wasRemoved(sb.RuntimeID), "leader readback is not a durable acknowledgement")
			record, err := base.Load(ctx, sb.ID)
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(record.CleanupCheckpoint, fuseCleanupCheckpointPrefix))
			poolRecords, err := m.fusePool.repo.ListByPoolKey(ctx, "pool-key")
			require.NoError(t, err)
			for _, record := range poolRecords {
				if record.RuntimeUID == sb.RuntimeUID {
					require.Equal(t, state.FUSEPoolConsumed, record.State, "failed ACK must not advance destructive cleanup")
				}
			}
		})
	}
}

func (s failSessionRemovalStore) CompareAndDelete(ctx context.Context, key string, value []byte) (bool, error) {
	if strings.HasPrefix(key, sandboxSessionKeyPrefix) {
		return false, errors.New("crash after pool completion")
	}
	return s.atomicMemoryStore.CompareAndDelete(ctx, key, value)
}

func TestDistributedFUSECleanupResumesPostFlushCrashStages(t *testing.T) {
	for _, stage := range []string{"claim", "runtime_removed", "pool_completed"} {
		t.Run(stage, func(t *testing.T) {
			m, rt, store := newDistributedFUSERecoveryManager(t)
			ctx := context.Background()
			prepared, err := m.fusePool.repo.ListByPoolKey(ctx, "pool-key")
			require.NoError(t, err)
			require.Len(t, prepared, 1)
			switch stage {
			case "claim":
				m.fusePool.repo = &failClaimReplyRepository{FUSEPoolRepository: m.fusePool.repo, targetUID: prepared[0].RuntimeUID}
			case "runtime_removed":
				wrapper := &failRemovalReplyRuntime{fuseManagerRuntime: rt, targetUID: prepared[0].RuntimeUID}
				m.runtime = wrapper
				m.fusePool.runtime = wrapper
			case "pool_completed":
				m.SetSessionStore(NewSessionStore(failSessionRemovalStore{store}, time.Hour))
			}
			sb, err := m.Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/crash-" + stage})
			require.NoError(t, err)
			m.mu.RLock()
			lifecycle := m.fuseLifecycles[sb.ID]
			m.mu.RUnlock()
			m.teardownFUSESandbox(lifecycle, ErrSandboxCleanupPending)
			record, err := m.activeSandboxes.Load(ctx, sb.ID)
			require.NoError(t, err)
			require.NotNil(t, record)
			require.NoError(t, m.Stop(ctx))
			cfg := m.config
			cfg.InstanceID = "peer"
			peer := NewManager(m.runtime, m.filesystem, m.fsMeta, cfg)
			peer.SetSessionStore(NewSessionStore(store, time.Hour))
			t.Cleanup(func() { require.NoError(t, peer.Stop(ctx)) })
			cleanupCtx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			cleanupErr := peer.Destroy(cleanupCtx, sb.ID)
			if cleanupErr != nil {
				peer.mu.RLock()
				current := peer.fuseLifecycles[sb.ID]
				peer.mu.RUnlock()
				if current != nil {
					current.teardownMu.Lock()
					t.Logf("cleanup stopped at stage %s", current.lastFailureStage)
					current.teardownMu.Unlock()
				}
			}
			require.NoError(t, cleanupErr)
			record, err = peer.activeSandboxes.Load(ctx, sb.ID)
			require.NoError(t, err)
			require.Nil(t, record)
			keys, err := workspaceStateKeysFromOwner(sb.Workspace.Owner)
			require.NoError(t, err)
			owner, err := store.Get(ctx, keys.owner)
			require.NoError(t, err)
			require.Nil(t, owner)
		})
	}
}

type canceledQuiesceRuntime struct {
	*fuseManagerRuntime
	entered chan struct{}
	calls   atomic.Int32
}

func (r *canceledQuiesceRuntime) QuiesceWorkspace(ctx context.Context, _ runtime.RuntimeRef, _ int64) (runtime.WorkspaceQuiesceToken, error) {
	if r.calls.Add(1) == 1 {
		close(r.entered)
	}
	<-ctx.Done()
	return runtime.WorkspaceQuiesceToken{}, ctx.Err()
}

func TestDistributedFUSEBackgroundCleanupStopsOnShutdown(t *testing.T) {
	rt := newFUSEManagerRuntime()
	m, _, _, _ := newFUSETestManager(t, rt)
	m.config.RuntimeType = "kubernetes"
	m.config.InstanceID = "creator"
	m.activeSandboxes = newMemoryActiveRepository()
	m.config.ActiveSandboxes = m.activeSandboxes
	sb, err := m.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/shutdown"})
	require.NoError(t, err)
	wrapper := &canceledQuiesceRuntime{fuseManagerRuntime: rt, entered: make(chan struct{})}
	m.runtime = wrapper
	m.mu.RLock()
	lifecycle := m.fuseLifecycles[sb.ID]
	m.mu.RUnlock()
	for i := 0; i < 20; i++ {
		m.scheduleFUSETeardown(lifecycle, ErrSandboxCleanupPending)
	}
	select {
	case <-wrapper.entered:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	require.NoError(t, m.Stop(ctx), "tracked cleanup must be canceled, not keep Stop blocked")
	require.EqualValues(t, 1, wrapper.calls.Load(), "repeated scheduling must share one cleanup worker")
	record, err := m.activeSandboxes.Load(context.Background(), sb.ID)
	require.NoError(t, err)
	require.NotNil(t, record, "shutdown retains durable cleanup evidence")
}

func TestDistributedFUSEWatcherCleanupStopsOnShutdown(t *testing.T) {
	rt := newFUSEManagerRuntime()
	m, _, _, _ := newFUSETestManager(t, rt)
	m.config.RuntimeType = "kubernetes"
	m.config.InstanceID = "creator"
	m.config.FUSEHealthInterval = time.Millisecond
	m.activeSandboxes = newMemoryActiveRepository()
	m.config.ActiveSandboxes = m.activeSandboxes
	wrapper := &canceledQuiesceRuntime{fuseManagerRuntime: rt, entered: make(chan struct{})}
	m.runtime = wrapper
	_, err := m.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/watcher-shutdown"})
	require.NoError(t, err)
	rt.mu.Lock()
	rt.health.Ready = false
	rt.mu.Unlock()
	select {
	case <-wrapper.entered:
	case <-time.After(time.Second):
		t.Fatal("watcher cleanup did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	require.NoError(t, m.Stop(ctx), "watcher must not enter an uncancelable cleanup loop")
}

func TestDistributedEphemeralFUSEStopNeverFinalizesSharedRuntime(t *testing.T) {
	for i := 0; i < 30; i++ {
		rt := newFUSEManagerRuntime()
		m, _, _, store := newFUSETestManager(t, rt)
		m.SetEphemeralLifecycleStore(NewEphemeralLifecycleStore(store))
		m.config.RuntimeType = "kubernetes"
		m.config.InstanceID = "creator"
		m.activeSandboxes = newMemoryActiveRepository()
		m.config.ActiveSandboxes = m.activeSandboxes
		wrapper := &canceledQuiesceRuntime{fuseManagerRuntime: rt, entered: make(chan struct{})}
		m.runtime = wrapper
		sb, err := m.Create(context.Background(), SandboxConfig{Mode: ModeEphemeral, WorkspacePath: "team/ephemeral-stop"})
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		err = m.Stop(ctx)
		cancel()
		require.NoError(t, err)
		require.Zero(t, wrapper.calls.Load(), "stopCh and watcher cancellation races must not finalize shared ephemeral runtimes")
		record, err := m.activeSandboxes.Load(context.Background(), sb.ID)
		require.NoError(t, err)
		require.Equal(t, state.ActiveSandboxActive, record.Phase)
	}
}

func TestDistributedFUSEWatcherDoesNotSuppressRuntimeFailure(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "missing"}[missing], func(t *testing.T) {
			m, rt, _ := newDistributedFUSERecoveryManager(t)
			sb, err := m.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/failure"})
			require.NoError(t, err)
			m.mu.RLock()
			lifecycle := m.fuseLifecycles[sb.ID]
			m.mu.RUnlock()
			rt.mockRuntime.mu.Lock()
			if missing {
				delete(rt.sandboxes, sb.RuntimeID)
			} else {
				rt.sandboxes[sb.RuntimeID].State = "failed"
			}
			rt.mockRuntime.mu.Unlock()
			require.Error(t, m.checkFUSELifecycle(context.Background(), lifecycle), "active runtime failure must trigger cleanup, not be mistaken for exclusive admission")
		})
	}
}

func TestDistributedFUSEQuiescedFlushRecoveredByUncachedPeer(t *testing.T) {
	m, rt, store := newDistributedFUSERecoveryManager(t)
	verifyQuiescedFUSEPeerCleanup(t, m, rt, store)
}

func TestDistributedFUSEQuiescedFlushRecoveredWithRealRedis(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("requires TEST_REDIS_ADDR")
	}
	store, err := redisstate.New(context.Background(), redisstate.Options{Addr: addr})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	repository, err := redisstate.NewActiveSandboxRepository(store, "fuse-cleanup-"+uuid.NewString())
	require.NoError(t, err)
	m, rt, _ := newDistributedFUSERecoveryManager(t)
	m.activeSandboxes = repository
	m.config.ActiveSandboxes = repository
	m.config.WorkspaceCoordinator = NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	m.SetSessionStore(NewSessionStore(store, time.Hour))
	verifyQuiescedFUSEPeerCleanup(t, m, rt, store)
}

func verifyQuiescedFUSEPeerCleanup(t *testing.T, m *Manager, rt *fuseManagerRuntime, store state.AtomicStore) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sb, err := m.Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/quiesced-" + uuid.NewString()})
	require.NoError(t, err)
	snapshot, opCtx, release, err := m.beginDistributedWorkspaceOperation(ctx, sb.ID)
	require.NoError(t, err)
	snapshot.WorkspaceTransition = workspaceFlushPending
	snapshot.Workspace.Flushed = false
	require.NoError(t, m.persistActiveSandboxUpdate(opCtx, snapshot))
	_, err = rt.QuiesceWorkspace(opCtx, runtime.RuntimeRef{ID: sb.RuntimeID, UID: sb.RuntimeUID}, sb.Workspace.LeaseGeneration)
	require.NoError(t, err)
	rt.mu.Lock()
	rt.health.Ready = false
	rt.mu.Unlock()
	release()                       // crashed flush leaves a durable pending transition
	require.NoError(t, m.Stop(ctx)) // relinquish this process's controllers
	cfg := m.config
	cfg.InstanceID = "peer"
	peer := NewManager(rt, m.filesystem, m.fsMeta, cfg)
	peer.SetSessionStore(NewSessionStore(store, time.Hour))
	t.Cleanup(func() { require.NoError(t, peer.Stop(context.Background())) })
	require.Error(t, peer.restoreFUSESandbox(ctx, snapshot), "public recovery must still reject a quiesced runtime")
	require.NoError(t, peer.Destroy(ctx, sb.ID), "cleanup must recover exact consumed runtime despite quiesced health")
	record, err := peer.activeSandboxes.Load(ctx, sb.ID)
	require.NoError(t, err)
	require.Nil(t, record)
	require.True(t, rt.wasRemoved(sb.RuntimeID))
	keys, err := workspaceStateKeysFromOwner(snapshot.Workspace.Owner)
	require.NoError(t, err)
	owner, err := store.Get(ctx, keys.owner)
	require.NoError(t, err)
	require.Nil(t, owner)
	lease, err := store.Get(ctx, keys.lease)
	require.NoError(t, err)
	require.Nil(t, lease)
	if memory, ok := store.(*atomicMemoryStore); ok {
		// The fixture keeps counters separately from its byte-value entries.
		memory.mu.Lock()
		generation := memory.increments[keys.generation]
		memory.mu.Unlock()
		require.Equal(t, snapshot.Workspace.LeaseGeneration, generation)
	} else {
		generation, err := store.Get(ctx, keys.generation)
		require.NoError(t, err)
		require.NotNil(t, generation, "cleanup must preserve fencing generation")
	}
	poolRecords, err := m.fusePool.repo.ListByPoolKey(ctx, "pool-key")
	require.NoError(t, err)
	for _, record := range poolRecords {
		require.NotEqual(t, state.FUSEPoolConsumed, record.State)
	}
}

func TestDistributedFUSEWatcherSkipsAuthoritativelyClosedAdmission(t *testing.T) {
	m, rt, _ := newDistributedFUSERecoveryManager(t)
	ctx := context.Background()
	sb, err := m.Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/exclusive"})
	require.NoError(t, err)
	_, _, release, err := m.beginDistributedWorkspaceOperation(ctx, sb.ID)
	require.NoError(t, err)
	rt.mu.Lock()
	rt.health.Ready = false
	rt.mu.Unlock()
	m.mu.RLock()
	lifecycle := m.fuseLifecycles[sb.ID]
	m.mu.RUnlock()
	require.NoError(t, m.checkFUSELifecycle(ctx, lifecycle), "exclusive flush owns health transitions")
	release()
	require.ErrorIs(t, m.checkFUSELifecycle(ctx, lifecycle), ErrSandboxNotReady, "open admission must not hide failed health")
}

func TestDistributedFUSECleanupRejectsIdentityDrift(t *testing.T) {
	for _, field := range []string{"pod_uid", "pool_revision", "owner_generation", "controller"} {
		t.Run(field, func(t *testing.T) {
			m, rt, store := newDistributedFUSERecoveryManager(t)
			ctx := context.Background()
			sb, err := m.Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/drift"})
			require.NoError(t, err)
			record, _, _, err := m.activeSandboxes.BeginDestroy(ctx, sb.ID)
			require.NoError(t, err)
			snapshot, err := decodeActiveSandboxPhase(record, sb.ID, state.ActiveSandboxDestroying)
			require.NoError(t, err)
			require.NoError(t, m.Stop(ctx))
			cfg := m.config
			cfg.InstanceID = "peer"
			peer := NewManager(rt, m.filesystem, m.fsMeta, cfg)
			peer.SetSessionStore(NewSessionStore(store, time.Hour))
			t.Cleanup(func() { require.NoError(t, peer.Stop(ctx)) })
			controller, acquired, err := peer.acquireActiveController(ctx, snapshot)
			require.NoError(t, err)
			require.True(t, acquired)
			t.Cleanup(func() { require.NoError(t, controller.Stop(ctx)) })
			rt.mu.Lock()
			rt.health.Ready = false
			rt.mu.Unlock()
			switch field {
			case "pod_uid":
				rt.mockRuntime.mu.Lock()
				rt.sandboxes[sb.RuntimeID].RuntimeUID = "replacement"
				rt.mockRuntime.mu.Unlock()
			case "pool_revision":
				snapshot.Workspace.FUSERecordRevision++
			case "owner_generation":
				snapshot.Workspace.Owner.Generation++
				snapshot.Workspace.LeaseGeneration++
			case "controller":
				require.NoError(t, controller.Stop(ctx))
			}
			require.Error(t, peer.restoreFUSECleanupWithController(ctx, snapshot, controller))
			require.False(t, rt.wasRemoved(sb.RuntimeID))
			owners, err := store.Keys(ctx, workspaceOwnerKeyPrefix+"*")
			require.NoError(t, err)
			require.NotEmpty(t, owners, "uncertain identity must retain ownership evidence")
		})
	}
}

func TestDistributedFUSEFinalActiveDeleteFailureRetainsControllerUntilStop(t *testing.T) {
	m, rt, store := newDistributedFUSERecoveryManager(t)
	ctx := context.Background()
	sb, err := m.Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/final-delete"})
	require.NoError(t, err)
	original := m.activeSandboxes.(*memoryActiveRepository)
	m.activeSandboxes = failedFinalDeleteRepository{original}
	m.mu.RLock()
	lifecycle := m.fuseLifecycles[sb.ID]
	m.mu.RUnlock()
	lifecycle.controller.repository = m.activeSandboxes
	m.teardownFUSESandbox(lifecycle, ErrSandboxCleanupPending)
	m.mu.RLock()
	tracked := m.fuseLifecycles[sb.ID]
	m.mu.RUnlock()
	require.Same(t, lifecycle, tracked, "failed deletion must retain the controller for shutdown")
	record, err := original.Load(ctx, sb.ID)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(record.CleanupCheckpoint, fuseCleanupCheckpointPrefix))
	require.NoError(t, m.Stop(ctx))
	cfg := m.config
	cfg.InstanceID = "peer"
	cfg.ActiveSandboxes = original
	peer := NewManager(rt, m.filesystem, m.fsMeta, cfg)
	peer.SetSessionStore(NewSessionStore(store, time.Hour))
	t.Cleanup(func() { require.NoError(t, peer.Stop(ctx)) })
	cleanupCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	require.NoError(t, peer.Destroy(cleanupCtx, sb.ID), "natural Stop must release the tracked controller for failover")
	record, err = original.Load(ctx, sb.ID)
	require.NoError(t, err)
	require.Nil(t, record)
}

func TestDistributedFUSECheckpointCleanupRejectsContradictoryClaim(t *testing.T) {
	for _, drift := range []string{"claimed_uid", "claimed_revision", "consumed_after_claim"} {
		t.Run(drift, func(t *testing.T) {
			m, rt, store := newDistributedFUSERecoveryManager(t)
			ctx := context.Background()
			pool := m.fusePool.repo.(*memoryFUSEPoolRepository)
			prepared, err := pool.ListByPoolKey(ctx, "pool-key")
			require.NoError(t, err)
			m.fusePool.repo = &failClaimReplyRepository{FUSEPoolRepository: pool, targetUID: prepared[0].RuntimeUID}
			sb, err := m.Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/contradictory-claim"})
			require.NoError(t, err)
			m.mu.RLock()
			lifecycle := m.fuseLifecycles[sb.ID]
			m.mu.RUnlock()
			m.teardownFUSESandbox(lifecycle, ErrSandboxCleanupPending)
			record, err := m.activeSandboxes.Load(ctx, sb.ID)
			require.NoError(t, err)
			var cp fuseCleanupCheckpoint
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(record.CleanupCheckpoint, fuseCleanupCheckpointPrefix)), &cp))
			claimed, ok := pool.record(sb.Workspace.FUSEPreparationID)
			require.True(t, ok)
			cp.Claimed = &claimed
			switch drift {
			case "claimed_uid":
				cp.Claimed.RuntimeUID = "foreign-uid"
			case "claimed_revision":
				cp.Claimed.Revision = cp.Consumed.Revision
			case "consumed_after_claim":
				pool.seed(cp.Consumed)
			}
			raw, err := json.Marshal(cp)
			require.NoError(t, err)
			_, err = lifecycle.controller.checkpoint(ctx, record.Revision, fuseCleanupCheckpointPrefix+string(raw))
			require.NoError(t, err)
			record, err = m.activeSandboxes.Load(ctx, sb.ID)
			require.NoError(t, err)
			snapshot, err := decodeActiveSandboxPhase(record, sb.ID, state.ActiveSandboxCleanupPending)
			require.NoError(t, err)
			require.NoError(t, m.Stop(ctx))
			cfg := m.config
			cfg.InstanceID = "peer"
			peer := NewManager(rt, m.filesystem, m.fsMeta, cfg)
			peer.SetSessionStore(NewSessionStore(store, time.Hour))
			t.Cleanup(func() { require.NoError(t, peer.Stop(ctx)) })
			controller, acquired, err := peer.acquireActiveController(ctx, snapshot)
			require.NoError(t, err)
			require.True(t, acquired)
			t.Cleanup(func() { require.NoError(t, controller.Stop(ctx)) })
			require.Error(t, peer.restoreFUSECleanupWithController(ctx, snapshot, controller))
			require.False(t, rt.wasRemoved(sb.RuntimeID))
		})
	}
}

func TestDistributedFUSECheckpointCleanupRejectsMissingConfigAndIdentityDrift(t *testing.T) {
	for _, field := range []string{"workspace_nil", "pool_nil", "coordinator_nil", "session_nil", "owner_uid", "owner_generation", "prefix", "provider", "bucket", "storage_hash"} {
		t.Run(field, func(t *testing.T) {
			m, rt, store := newDistributedFUSERecoveryManager(t)
			ctx := context.Background()
			prepared, err := m.fusePool.repo.ListByPoolKey(ctx, "pool-key")
			require.NoError(t, err)
			require.Len(t, prepared, 1)
			m.fusePool.repo = &failClaimReplyRepository{FUSEPoolRepository: m.fusePool.repo, targetUID: prepared[0].RuntimeUID}
			sb, err := m.Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/checkpoint-drift"})
			require.NoError(t, err)
			m.mu.RLock()
			lifecycle := m.fuseLifecycles[sb.ID]
			m.mu.RUnlock()
			m.teardownFUSESandbox(lifecycle, ErrSandboxCleanupPending)
			record, err := m.activeSandboxes.Load(ctx, sb.ID)
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(record.CleanupCheckpoint, fuseCleanupCheckpointPrefix))
			snapshot, err := decodeActiveSandboxPhase(record, sb.ID, state.ActiveSandboxCleanupPending)
			require.NoError(t, err)
			require.NoError(t, m.Stop(ctx))
			cfg := m.config
			cfg.InstanceID = "peer"
			peer := NewManager(rt, m.filesystem, m.fsMeta, cfg)
			peer.SetSessionStore(NewSessionStore(store, time.Hour))
			t.Cleanup(func() { require.NoError(t, peer.Stop(ctx)) })
			controller, acquired, err := peer.acquireActiveController(ctx, snapshot)
			require.NoError(t, err)
			require.True(t, acquired)
			t.Cleanup(func() { require.NoError(t, controller.Stop(ctx)) })
			switch field {
			case "workspace_nil":
				snapshot.Workspace = nil
			case "pool_nil":
				peer.fusePool = nil
			case "coordinator_nil":
				peer.config.WorkspaceCoordinator = nil
			case "session_nil":
				peer.sessions = nil
			case "owner_uid":
				snapshot.Workspace.Owner.RuntimeUID = "foreign-uid"
			case "owner_generation":
				snapshot.Workspace.Owner.Generation++
			case "prefix":
				snapshot.Workspace.Owner.Prefix = "foreign/"
			case "provider":
				snapshot.Workspace.Owner.Provider = "foreign"
			case "bucket":
				snapshot.Workspace.Owner.Bucket = "foreign"
			case "storage_hash":
				snapshot.Workspace.Owner.StorageIdentityHash = "foreign"
			}
			require.Error(t, peer.restoreFUSECleanupWithController(ctx, snapshot, controller))
			require.False(t, rt.wasRemoved(sb.RuntimeID))
			objects := m.config.WorkspaceObjectClient.(*fuseMarkerClient)
			objects.mu.Lock()
			deleted := len(objects.deletedKeys)
			objects.mu.Unlock()
			require.Zero(t, deleted, "invalid snapshot must not delete foreign probe objects")
		})
	}
}
