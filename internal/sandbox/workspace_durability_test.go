package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceDurabilityFailureNeverUsesLeaderReadbackAsSuccess(t *testing.T) {
	for _, operation := range []string{"restore", "bind", "release_owner", "release_lease", "session"} {
		for _, durabilityFailure := range []bool{true, false} {
			t.Run(operation+map[bool]string{true: "/ack", false: "/replylost"}[durabilityFailure], func(t *testing.T) {
				ctx := context.Background()
				store := newAtomicMemoryStore()
				coordinator := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
				lease, err := coordinator.Acquire(ctx, validLeaseRequest())
				require.NoError(t, err)
				failure := errors.New("reply lost")
				if durabilityFailure {
					failure = state.ErrDurabilityUnconfirmed
				}
				if operation != "bind" {
					require.NoError(t, coordinator.BindRuntime(ctx, lease, "uid-a"))
					_, err = coordinator.ConsumeMountAttempt(ctx, lease, "pool-key")
					require.NoError(t, err)
				}
				switch operation {
				case "restore":
					store.expireKey(lease.Key)
					store.mu.Lock()
					store.failSetNXAfterWrite[lease.Key] = failure
					store.mu.Unlock()
					_, err = coordinator.Restore(ctx, lease.OwnerSnapshot())
				case "bind":
					store.failCompareAndSwapAfterWriting(lease.ownerKey, failure, nil)
					err = coordinator.BindRuntime(ctx, lease, "uid-a")
				case "release_owner", "release_lease":
					key := lease.ownerKey
					if operation == "release_lease" {
						key = lease.Key
					}
					store.failCompareAndDeleteAfterWriting(key, failure)
					err = coordinator.Release(ctx, lease, runtime.TerminationEvidence{RuntimeUID: "uid-a", ProcessExited: true})
				case "session":
					sessions := NewSessionStore(store, time.Hour)
					sb := &Sandbox{ID: "session-ack", RuntimeID: "runtime-a", RuntimeUID: "uid-a", Workspace: &WorkspaceInfo{MountType: WorkspaceMountFUSE, FUSEPreparationID: "preparation-a", LeaseGeneration: 1}}
					require.NoError(t, sessions.Save(ctx, sb))
					store.failCompareAndDeleteAfterWriting(sandboxSessionKeyPrefix+sb.ID, failure)
					err = sessions.RemoveMatchingFUSESession(ctx, sb.ID, sb.RuntimeID, sb.RuntimeUID, "preparation-a", 1)
				}
				if durabilityFailure {
					require.ErrorIs(t, err, state.ErrDurabilityUnconfirmed)
					if operation == "release_owner" {
						require.True(t, store.hasKey(lease.Key), "failed owner ACK must not advance lease release")
					}
				} else {
					require.NoError(t, err, "generic lost-reply compatibility is retained")
				}
			})
		}
	}
}

type failedAbsenceAckStore struct {
	*atomicMemoryStore
	failKey      string
	notAbsentKey string
}

func (s failedAbsenceAckStore) ConfirmAbsence(ctx context.Context, key string) (bool, error) {
	if key == s.notAbsentKey {
		return false, nil
	}
	raw, err := s.Get(ctx, key)
	if err != nil || raw != nil {
		return false, err
	}
	if key == s.failKey {
		return true, state.ErrDurabilityUnconfirmed
	}
	return true, nil
}

func TestSessionCleanupMethodsRequireAcknowledgedAbsence(t *testing.T) {
	for _, method := range []string{"runtime", "exact"} {
		t.Run(method, func(t *testing.T) {
			store := newAtomicMemoryStore()
			sb := &Sandbox{ID: "session-absent", RuntimeID: "runtime-a", RuntimeUID: "uid-a"}
			sessions := NewSessionStore(failedAbsenceAckStore{atomicMemoryStore: store, failKey: sandboxSessionKeyPrefix + sb.ID}, time.Hour)
			var err error
			if method == "runtime" {
				err = sessions.RemoveMatchingRuntime(context.Background(), sb)
			} else {
				err = sessions.RemoveExact(context.Background(), sb)
			}
			require.ErrorIs(t, err, state.ErrDurabilityUnconfirmed)
		})
	}
}

func TestWorkspaceOwnerReleaseLostReplyRequiresAbsenceProof(t *testing.T) {
	ctx := context.Background()
	store := newAtomicMemoryStore()
	coordinator := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	lease, err := coordinator.Acquire(ctx, validLeaseRequest())
	require.NoError(t, err)
	require.NoError(t, coordinator.BindRuntime(ctx, lease, "uid-a"))
	_, err = coordinator.ConsumeMountAttempt(ctx, lease, "pool-key")
	require.NoError(t, err)
	store.failCompareAndDeleteAfterWriting(lease.ownerKey, errors.New("reply lost"))
	coordinator.store = failedAbsenceAckStore{atomicMemoryStore: store, failKey: lease.ownerKey}
	err = coordinator.Release(ctx, lease, runtime.TerminationEvidence{RuntimeUID: "uid-a", ProcessExited: true})
	require.ErrorIs(t, err, state.ErrDurabilityUnconfirmed)
	require.True(t, store.hasKey(lease.Key))
}

func TestWorkspaceCleanupRejectsFalseAbsenceProof(t *testing.T) {
	for _, target := range []string{"owner", "lease", "session"} {
		t.Run(target, func(t *testing.T) {
			ctx := context.Background()
			store := newAtomicMemoryStore()
			coordinator := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
			lease, err := coordinator.Acquire(ctx, validLeaseRequest())
			require.NoError(t, err)
			require.NoError(t, coordinator.BindRuntime(ctx, lease, "uid-a"))
			_, err = coordinator.ConsumeMountAttempt(ctx, lease, "pool-key")
			require.NoError(t, err)
			key := lease.ownerKey
			if target == "lease" {
				key = lease.Key
			}
			if target == "session" {
				sb := &Sandbox{ID: "session-reappeared", RuntimeID: "runtime-a", RuntimeUID: "uid-a"}
				sessions := NewSessionStore(failedAbsenceAckStore{atomicMemoryStore: store, notAbsentKey: sandboxSessionKeyPrefix + sb.ID}, time.Hour)
				require.ErrorIs(t, sessions.RemoveMatchingRuntime(ctx, sb), ErrSessionPublicationConflict)
				return
			}
			store.expireKey(lease.ownerKey)
			store.expireKey(lease.Key)
			coordinator.store = failedAbsenceAckStore{atomicMemoryStore: store, notAbsentKey: key}
			err = coordinator.Release(ctx, lease, runtime.TerminationEvidence{RuntimeUID: "uid-a", ProcessExited: true})
			if target == "owner" {
				require.ErrorIs(t, err, ErrWorkspaceOwnerLost)
			} else {
				require.ErrorIs(t, err, ErrWorkspaceLeaseLost)
			}
		})
	}
}

func TestWorkspaceCleanupRetryRequiresAcknowledgedAbsence(t *testing.T) {
	for _, operation := range []string{"owner", "lease", "session"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			store := newAtomicMemoryStore()
			coordinator := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
			lease, err := coordinator.Acquire(ctx, validLeaseRequest())
			require.NoError(t, err)
			require.NoError(t, coordinator.BindRuntime(ctx, lease, "uid-a"))
			_, err = coordinator.ConsumeMountAttempt(ctx, lease, "pool-key")
			require.NoError(t, err)
			if operation == "session" {
				sb := &Sandbox{ID: "session-retry", RuntimeID: "runtime-a", RuntimeUID: "uid-a", Workspace: &WorkspaceInfo{MountType: WorkspaceMountFUSE, FUSEPreparationID: "preparation-a", LeaseGeneration: 1}}
				sessions := NewSessionStore(store, time.Hour)
				require.NoError(t, sessions.Save(ctx, sb))
				key := sandboxSessionKeyPrefix + sb.ID
				store.failCompareAndDeleteAfterWriting(key, state.ErrDurabilityUnconfirmed)
				require.ErrorIs(t, sessions.RemoveMatchingFUSESession(ctx, sb.ID, sb.RuntimeID, sb.RuntimeUID, "preparation-a", 1), state.ErrDurabilityUnconfirmed)
				sessions.store = failedAbsenceAckStore{atomicMemoryStore: store, failKey: key}
				require.ErrorIs(t, sessions.RemoveMatchingFUSESession(ctx, sb.ID, sb.RuntimeID, sb.RuntimeUID, "preparation-a", 1), state.ErrDurabilityUnconfirmed)
				return
			}
			key := lease.ownerKey
			if operation == "lease" {
				key = lease.Key
			}
			store.failCompareAndDeleteAfterWriting(key, state.ErrDurabilityUnconfirmed)
			evidence := runtime.TerminationEvidence{RuntimeUID: "uid-a", ProcessExited: true}
			require.ErrorIs(t, coordinator.Release(ctx, lease, evidence), state.ErrDurabilityUnconfirmed)
			coordinator.store = failedAbsenceAckStore{atomicMemoryStore: store, failKey: key}
			require.ErrorIs(t, coordinator.Release(ctx, lease, evidence), state.ErrDurabilityUnconfirmed)
			if operation == "owner" {
				require.True(t, store.hasKey(lease.Key), "unconfirmed owner absence must retain the lease")
			}
		})
	}
}
