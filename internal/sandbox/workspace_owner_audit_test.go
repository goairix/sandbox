package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceOwnerAuditDoesNotMigrateLegacySessions(t *testing.T) {
	managers, store, rt := distributedSyncManagers(t)
	m := managers[0]
	m.runtime = missingAwareRuntime{rt}
	m.SetSessionStore(NewSessionStore(store, time.Hour))
	req := validLeaseRequest()
	req.MountType, req.Runtime, req.RuntimeUID = WorkspaceMountSync, "kubernetes", "uid-a"
	lease, err := m.config.WorkspaceCoordinator.Acquire(context.Background(), req)
	require.NoError(t, err)
	store.expireKey(lease.Key)
	legacy, err := json.Marshal(Sandbox{ID: req.SandboxID, RuntimeID: req.RuntimeID, CreatedAt: time.Now()})
	require.NoError(t, err)
	legacyKey := legacySandboxSessionKeyPrefix + req.SandboxID
	require.NoError(t, store.Set(context.Background(), legacyKey, legacy, 0))
	audit, err := m.AuditWorkspaceOwners(context.Background())
	require.NoError(t, err)
	require.Equal(t, "session_present", audit[0].Status)
	current, err := store.Get(context.Background(), sandboxSessionKeyPrefix+req.SandboxID)
	require.NoError(t, err)
	require.Nil(t, current, "audit must not publish a migrated session")
	raw, err := store.Get(context.Background(), legacyKey)
	require.NoError(t, err)
	require.Equal(t, legacy, raw, "audit must not delete legacy state")
}

func TestWorkspaceOwnerAuditIsReadOnlyAndRejectsLiveLease(t *testing.T) {
	managers, store, rt := distributedSyncManagers(t)
	m := managers[0]
	m.runtime = missingAwareRuntime{rt}
	req := validLeaseRequest()
	req.MountType = WorkspaceMountSync
	req.Runtime = "kubernetes"
	req.RuntimeUID = "uid-a"
	lease, err := m.config.WorkspaceCoordinator.Acquire(context.Background(), req)
	require.NoError(t, err)
	owner := lease.OwnerSnapshot()
	audit, err := m.AuditWorkspaceOwners(context.Background())
	require.NoError(t, err)
	require.Len(t, audit, 1)
	require.Equal(t, "lease_live", audit[0].Status)
	require.False(t, audit[0].Recoverable)
	require.Error(t, m.RecoverWorkspaceOwner(context.Background(), owner))
	raw, err := store.Get(context.Background(), lease.ownerKey)
	require.NoError(t, err)
	require.NotEmpty(t, raw)
}

func TestWorkspaceOwnerRecoveryRejectsUnprovenSyncOutputAndScope(t *testing.T) {
	managers, store, rt := distributedSyncManagers(t)
	m := managers[0]
	m.runtime = missingAwareRuntime{rt}
	req := validLeaseRequest()
	req.MountType = WorkspaceMountSync
	req.Runtime = "kubernetes"
	req.RuntimeUID = "uid-a"
	lease, err := m.config.WorkspaceCoordinator.Acquire(context.Background(), req)
	require.NoError(t, err)
	owner := lease.OwnerSnapshot()
	// Expire only this test lease, retaining the persistent fencing owner.
	store.expireKey(lease.Key)
	audit, err := m.AuditWorkspaceOwners(context.Background())
	require.NoError(t, err)
	require.False(t, audit[0].Recoverable)
	wrong := owner
	wrong.Generation++
	require.Error(t, m.RecoverWorkspaceOwner(context.Background(), wrong))
	require.Error(t, m.RecoverWorkspaceOwner(context.Background(), owner))
	raw, err := store.Get(context.Background(), lease.ownerKey)
	require.NoError(t, err)
	require.NotEmpty(t, raw, "missing runtime is not proof of final output or namespace ownership")
	store.mu.Lock()
	require.Equal(t, owner.Generation, store.increments[lease.generationKey], "generation must never be reset")
	store.mu.Unlock()
}

type terminatedAuditRuntime struct{ missingAwareRuntime }

func (r terminatedAuditRuntime) ConfirmTerminated(_ context.Context, _, uid string) (runtime.TerminationEvidence, error) {
	return runtime.TerminationEvidence{RuntimeUID: uid, ProcessExited: true, GracefulUnmount: true}, nil
}

func TestWorkspaceOwnerRecoveryRequiresExactBackendOwnerAndGracefulTermination(t *testing.T) {
	managers, store, rt := distributedSyncManagers(t)
	m := managers[0]
	m.config.RuntimeType = "docker"
	m.runtime = terminatedAuditRuntime{missingAwareRuntime{rt}}
	req := validLeaseRequest()
	req.StorageIdentity = m.fsMeta.StorageIdentity
	req.RuntimeUID = "uid-a"
	lease, err := m.config.WorkspaceCoordinator.Acquire(context.Background(), req)
	require.NoError(t, err)
	owner := lease.OwnerSnapshot()
	store.expireKey(lease.Key)
	audit, err := m.AuditWorkspaceOwners(context.Background())
	require.NoError(t, err)
	require.True(t, audit[0].Recoverable)
	wrong := owner
	wrong.Generation++
	require.Error(t, m.RecoverWorkspaceOwner(context.Background(), wrong))
	require.NoError(t, m.RecoverWorkspaceOwner(context.Background(), owner))
	raw, err := store.Get(context.Background(), lease.ownerKey)
	require.NoError(t, err)
	require.Empty(t, raw)
	store.mu.Lock()
	require.Equal(t, owner.Generation, store.increments[lease.generationKey])
	store.mu.Unlock()
}

func TestWorkspaceOwnerAuditBlocksMissingScopeAndOutputEvidence(t *testing.T) {
	for _, runtimeType := range []string{"kubernetes", "docker"} {
		t.Run(runtimeType, func(t *testing.T) {
			managers, store, rt := distributedSyncManagers(t)
			m := managers[0]
			m.config.RuntimeType = runtimeType
			m.runtime = missingAwareRuntime{rt}
			req := validLeaseRequest()
			req.MountType, req.Runtime, req.RuntimeUID = WorkspaceMountSync, runtimeType, "uid-a"
			req.StorageIdentity = m.fsMeta.StorageIdentity
			lease, err := m.config.WorkspaceCoordinator.Acquire(context.Background(), req)
			require.NoError(t, err)
			store.expireKey(lease.Key)
			audit, err := m.AuditWorkspaceOwners(context.Background())
			require.NoError(t, err)
			status := "sync_output_unconfirmed"
			if runtimeType == "kubernetes" {
				status = "runtime_scope_unconfirmed"
			}
			require.Equal(t, status, audit[0].Status)
			require.False(t, audit[0].Recoverable)
			require.Error(t, m.RecoverWorkspaceOwner(context.Background(), lease.OwnerSnapshot()))
		})
	}
}

// Compile-time guard ensures the replacement fixture uses the public runtime.
var _ runtime.Runtime = missingAwareRuntime{}

func (s *atomicMemoryStore) CompareAndDeleteIfAbsent(_ context.Context, key string, expected []byte, absentKey string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.getLocked(absentKey); exists {
		return false, nil
	}
	entry, exists := s.getLocked(key)
	if !exists || !bytes.Equal(entry.value, expected) {
		return false, nil
	}
	delete(s.entries, key)
	return true, nil
}
