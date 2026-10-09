package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

type workspaceFinalizationObjectClient struct{ *fuseMarkerClient }

func (c workspaceFinalizationObjectClient) HeadObject(ctx context.Context, key string) (bool, error) {
	if strings.HasSuffix(key, "/") {
		return true, nil // root markers survive deletion of a distinct probe object
	}
	return c.fuseMarkerClient.HeadObject(ctx, key)
}

// Leave the durable flush/termination checkpoint after owner release, as a
// crashed creator whose final session removal failed would do.
func workspaceFinalizationPeer(t *testing.T) (*Manager, *fuseManagerRuntime, *atomicMemoryStore, *Sandbox) {
	t.Helper()
	m, rt, store := newDistributedFUSERecoveryManager(t)
	m.SetSessionStore(NewSessionStore(failSessionRemovalStore{store}, time.Hour))
	sb, err := m.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/finalization"})
	require.NoError(t, err)
	m.mu.RLock()
	lifecycle := m.fuseLifecycles[sb.ID]
	m.mu.RUnlock()
	m.teardownFUSESandbox(lifecycle, ErrSandboxCleanupPending)
	require.True(t, rt.wasRemoved(sb.RuntimeID))
	keys, err := workspaceStateKeysFromOwner(sb.Workspace.Owner)
	require.NoError(t, err)
	owner, err := store.Get(context.Background(), keys.owner)
	require.NoError(t, err)
	require.Nil(t, owner)
	active, err := m.activeSandboxes.Load(context.Background(), sb.ID)
	require.NoError(t, err)
	require.NotNil(t, active)
	require.NoError(t, m.Stop(context.Background()))
	cfg := m.config
	cfg.InstanceID = "finalization-peer"
	cfg.FUSEPool = NewFUSEPool(rt, m.fusePool.repo, m.fusePool.config, m.fusePool.spec)
	peer := NewManager(rt, m.filesystem, m.fsMeta, cfg)
	peer.SetSessionStore(NewSessionStore(store, time.Hour))
	t.Cleanup(func() { require.NoError(t, peer.Stop(context.Background())) })
	marker := peer.config.WorkspaceObjectClient.(*fuseMarkerClient)
	marker.exists = true
	peer.config.WorkspaceObjectClient = workspaceFinalizationObjectClient{marker}
	require.NoError(t, peer.fusePool.WarmUp(context.Background()))
	return peer, rt, store, sb
}

func TestWorkspaceRequestFinishesCleanupAfterOwnerReleased(t *testing.T) {
	peer, _, _, old := workspaceFinalizationPeer(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	created, reused, err := peer.GetOrCreate(ctx, old.Config)
	require.NoError(t, err, "the valid checkpoint must finish cleanup before creating")
	require.False(t, reused)
	require.NotEqual(t, old.ID, created.ID)
	active, err := peer.activeSandboxes.Load(ctx, old.ID)
	require.NoError(t, err)
	require.Nil(t, active)
	_, err = peer.sessions.LoadReadOnly(ctx, old.ID)
	require.ErrorIs(t, err, ErrSandboxNotFound)
}

type transientWorkspaceFinalDeleteRepository struct {
	*memoryActiveRepository
	attempts atomic.Int32
}

func (r *transientWorkspaceFinalDeleteRepository) DeleteControllerWithResult(ctx context.Context, lease state.ActiveSandboxControllerLease, revision uint64) (bool, error) {
	if r.attempts.Add(1) == 1 {
		return false, errors.New("transient final active deletion failure")
	}
	return r.memoryActiveRepository.DeleteControllerWithResult(ctx, lease, revision)
}

func TestWorkspaceRequestRetriesTransientCleanupFinalization(t *testing.T) {
	peer, _, _, old := workspaceFinalizationPeer(t)
	repo := &transientWorkspaceFinalDeleteRepository{memoryActiveRepository: peer.activeSandboxes.(*memoryActiveRepository)}
	peer.activeSandboxes = repo
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	created, reused, err := peer.GetOrCreate(ctx, old.Config)
	require.NoError(t, err, "a temporary finalization failure must retry within the request budget")
	require.False(t, reused)
	require.NotEqual(t, old.ID, created.ID)
	active, err := repo.Load(ctx, old.ID)
	require.NoError(t, err)
	require.Nil(t, active)
}

func TestWorkspaceRequestWaitsForOwnerlessCleanupFinalization(t *testing.T) {
	peer, _, store, old := workspaceFinalizationPeer(t)
	peer.activeSandboxes = failedFinalDeleteRepository{peer.activeSandboxes.(*memoryActiveRepository)}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	created, _, err := peer.GetOrCreate(ctx, old.Config)
	require.ErrorIs(t, err, ErrWorkspaceLookupUnavailable, "unfinished cleanup must wait within the request deadline")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, created)
	active, err := peer.activeSandboxes.Load(context.Background(), old.ID)
	require.NoError(t, err)
	require.NotNil(t, active)
	keys, err := workspaceStateKeysFromOwner(old.Workspace.Owner)
	require.NoError(t, err)
	owner, err := store.Get(context.Background(), keys.owner)
	require.NoError(t, err)
	require.Nil(t, owner, "replacement must wait for active record deletion")
}

func TestWorkspaceRequestRetainsUnprovenOwnerlessCleanup(t *testing.T) {
	for _, drift := range []string{"missing_checkpoint", "corrupt_checkpoint", "snapshot_uid", "snapshot_owner", "claimed_uid", "termination_uid", "termination_unconfirmed", "live_replacement"} {
		t.Run(drift, func(t *testing.T) {
			peer, rt, store, old := workspaceFinalizationPeer(t)
			repo := peer.activeSandboxes.(*memoryActiveRepository)
			active, err := repo.Load(context.Background(), old.ID)
			require.NoError(t, err)
			switch drift {
			case "missing_checkpoint":
				active.CleanupCheckpoint = ""
			case "corrupt_checkpoint":
				active.CleanupCheckpoint = fuseCleanupCheckpointPrefix + "{"
			case "snapshot_uid", "snapshot_owner":
				var snapshot Sandbox
				require.NoError(t, json.Unmarshal(active.Snapshot, &snapshot))
				if drift == "snapshot_uid" {
					snapshot.RuntimeUID = "foreign-uid"
				} else {
					snapshot.Workspace.Owner.Prefix = "foreign/"
				}
				active.Snapshot, err = json.Marshal(snapshot)
				require.NoError(t, err)
			case "claimed_uid", "termination_uid", "termination_unconfirmed":
				var cp fuseCleanupCheckpoint
				require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(active.CleanupCheckpoint, fuseCleanupCheckpointPrefix)), &cp))
				require.NotNil(t, cp.Claimed)
				switch drift {
				case "claimed_uid":
					cp.Claimed.RuntimeUID = "foreign-uid"
				case "termination_uid":
					cp.Claimed.TerminationEvidence.RuntimeUID = "foreign-uid"
				default:
					cp.Claimed.TerminationEvidence = nil
				}
				raw, err := json.Marshal(cp)
				require.NoError(t, err)
				active.CleanupCheckpoint = fuseCleanupCheckpointPrefix + string(raw)
			case "live_replacement":
				rt.mockRuntime.mu.Lock()
				rt.sandboxes[old.RuntimeID] = &runtime.SandboxInfo{RuntimeID: old.RuntimeID, RuntimeUID: "foreign-uid", State: "running"}
				rt.mockRuntime.mu.Unlock()
			}
			repo.mu.Lock()
			repo.records[old.ID] = *active
			repo.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			created, _, err := peer.GetOrCreate(ctx, old.Config)
			require.Error(t, err)
			require.Nil(t, created)
			retained, err := repo.Load(context.Background(), old.ID)
			require.NoError(t, err)
			require.NotNil(t, retained, "uncertain evidence must never be erased")
			require.Equal(t, active.Snapshot, retained.Snapshot)
			require.Equal(t, active.CleanupCheckpoint, retained.CleanupCheckpoint)
			keys, err := workspaceStateKeysFromOwner(old.Workspace.Owner)
			require.NoError(t, err)
			owner, err := store.Get(context.Background(), keys.owner)
			require.NoError(t, err)
			require.Nil(t, owner, "no replacement owner may be published")
		})
	}
}

func TestWorkspaceFinalizationRequiresDurableOwnerAndLeaseAbsence(t *testing.T) {
	for _, phase := range []string{"owner", "lease"} {
		t.Run(phase, func(t *testing.T) {
			peer, _, store, old := workspaceFinalizationPeer(t)
			keys, err := workspaceStateKeysFromOwner(old.Workspace.Owner)
			require.NoError(t, err)
			failKey := keys.owner
			if phase == "lease" {
				failKey = keys.lease
			}
			peer.config.WorkspaceCoordinator.store = failedAbsenceAckStore{atomicMemoryStore: store, failKey: failKey}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			created, _, err := peer.GetOrCreate(ctx, old.Config)
			require.Error(t, err, "nil leader reads must not replace durable deletion acknowledgements")
			require.Nil(t, created)
			active, err := peer.activeSandboxes.Load(context.Background(), old.ID)
			require.NoError(t, err)
			require.NotNil(t, active)
			owner, err := store.Get(context.Background(), keys.owner)
			require.NoError(t, err)
			require.Nil(t, owner)
			peer.config.WorkspaceCoordinator.store = store
			recovered, reused, err := peer.GetOrCreate(context.Background(), old.Config)
			require.NoError(t, err)
			require.False(t, reused)
			require.NotEqual(t, old.ID, recovered.ID)
		})
	}
}

func TestWorkspaceFinalizationAfterLostLeaseAckRequiresDurableAbsence(t *testing.T) {
	ctx := context.Background()
	m, rt, store := newDistributedFUSERecoveryManager(t)
	old, err := m.Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/lost-release-ack"})
	require.NoError(t, err)
	keys, err := workspaceStateKeysFromOwner(old.Workspace.Owner)
	require.NoError(t, err)
	store.failCompareAndDeleteAfterWriting(keys.lease, state.ErrDurabilityUnconfirmed)
	m.mu.RLock()
	lifecycle := m.fuseLifecycles[old.ID]
	m.mu.RUnlock()
	m.teardownFUSESandbox(lifecycle, ErrSandboxCleanupPending)
	require.False(t, lifecycle.leaseReleased)
	ownerRaw, err := store.Get(ctx, keys.owner)
	require.NoError(t, err)
	require.Nil(t, ownerRaw)
	leaseRaw, err := store.Get(ctx, keys.lease)
	require.NoError(t, err)
	require.Nil(t, leaseRaw)
	require.NoError(t, m.Stop(ctx))
	cfg := m.config
	cfg.InstanceID = "lost-ack-peer"
	cfg.FUSEPool = NewFUSEPool(rt, m.fusePool.repo, m.fusePool.config, m.fusePool.spec)
	peer := NewManager(rt, m.filesystem, m.fsMeta, cfg)
	peer.SetSessionStore(NewSessionStore(store, time.Hour))
	t.Cleanup(func() { require.NoError(t, peer.Stop(ctx)) })
	marker := peer.config.WorkspaceObjectClient.(*fuseMarkerClient)
	marker.exists = true
	peer.config.WorkspaceObjectClient = workspaceFinalizationObjectClient{marker}
	peer.config.WorkspaceCoordinator.store = failedAbsenceAckStore{atomicMemoryStore: store, failKey: keys.lease}
	require.NoError(t, peer.fusePool.WarmUp(ctx))
	requestCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	created, _, err := peer.GetOrCreate(requestCtx, old.Config)
	require.Error(t, err, "the previous lease delete was unacknowledged and durable absence cannot now be confirmed")
	require.Nil(t, created)
	active, err := peer.activeSandboxes.Load(ctx, old.ID)
	require.NoError(t, err)
	require.NotNil(t, active)
}
