//go:build linux

package etcd

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func recoveryExecutionCold(t *testing.T) (*quiescenceNativeFixture, *deliveryNativeWire, uint32) {
	f, prepared, wire, plan := recoveryNativePrepared(t)
	done := deliveryBegin(f, prepared)
	deliveryWaitSent(t, wire, done)
	close(wire.body)
	r := deliveryWait(t, done)
	require.NoError(t, r.err)
	require.NoError(t, <-wire.done)
	s, err := f.b.PrepareTaskWorkspaceSyncSource(f.ctx, r.archive, plan)
	require.NoError(t, err)
	_, err = s.next(f.ctx)
	require.NoError(t, err)
	e, err := s.next(f.ctx)
	require.NoError(t, err)
	body, err := s.openBody(f.ctx, e)
	require.NoError(t, err)
	target, err := openTaskWorkspaceSyncLocalTarget(f.ctx, workspaceSyncLocalNativeWorker, plan, archiveTestDirectory(t))
	require.NoError(t, err)
	require.NoError(t, s.bindLocalPut(f.ctx, body, target))
	_, err = s.reservePutIntent(f.ctx, body)
	require.NoError(t, err)
	old := f.b
	o := integrationOptions(old, f.raw)
	o.TaskWorkspaceArchiveRecovery = &TaskWorkspaceArchiveRecoveryOptions{TaskWorkspaceArchiveSpoolOptions{old.archiveStore.recovery.directory, old.archiveStore.limits.Jobs, old.archiveStore.limits.JobBytes, old.archiveStore.limits.TotalBytes, old.archiveStore.limits.Entries, old.archiveStore.limits.JobIndexBytes, old.archiveStore.limits.TotalIndexBytes}}
	o.TaskWorkspaceSyncRecyclingBudget = &old.workspaceSyncBudget.options
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	o.PublicationTrust = &RuntimePublicationTrust{AuthorityID: old.publicationAuthorityID, Target: old.publicationTarget, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}
	o.Clock, o.RequestTimeout = old.authorityClock, 3*time.Second
	require.NoError(t, s.Close())
	require.NoError(t, target.Close())
	require.NoError(t, old.ReleaseTaskClaim(f.ctx, f.c))
	require.NoError(t, old.Close())
	f.b, err = New(f.ctx, o)
	require.NoError(t, err)
	archiveTrackCleanup(f.b)
	t.Cleanup(func() { require.NoError(t, f.b.Close()) })
	f.c, err = f.b.ClaimTask(f.ctx, f.c.reference.Task, "cold-recovery", time.Minute)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.b.ReleaseTaskClaim(context.Background(), f.c) })
	return f, wire, 0
}

func TestTaskWorkspaceSyncRecoveryNativeCold(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	f, wire, slot := recoveryExecutionCold(t)
	r, err := f.b.RecoverTaskWorkspaceSyncLocal(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker)
	require.NoError(t, err)
	require.True(t, r.RegularFilesComplete)
	require.EqualValues(t, 2, r.CompletedFiles)
	require.EqualValues(t, 8, r.CompletedBytes)
	require.Nil(t, f.c.finalSyncDraft)
}

func TestTaskWorkspaceSyncRecoveryNativeAuthority(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	f, wire, slot := recoveryExecutionCold(t)
	s, err := f.b.prepareTaskWorkspaceSyncRecovery(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker)
	require.NoError(t, err)
	require.NoError(t, <-wire.done)
	require.Len(t, s.authority.comparisons, 50)
	require.NoError(t, s.authority.guard(f.ctx))
	require.Nil(t, f.c.finalSyncDraft)
	require.EqualValues(t, 1, wire.sends.Load(), "historical query never sends DATA or the original command")
	require.NoError(t, s.Close())
}
