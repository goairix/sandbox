//go:build linux

package etcd

import (
	"archive/tar"
	"context"
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func recoveryExecutionCold(t *testing.T) (*quiescenceNativeFixture, *deliveryNativeWire, uint32) {
	return recoveryExecutionColdData(t, nil, 0)
}

func recoveryExecutionColdData(t *testing.T, data []byte, entries uint64) (*quiescenceNativeFixture, *deliveryNativeWire, uint32) {
	var f *quiescenceNativeFixture
	var prepared *PreparedTaskWorkspaceArchive
	var wire *deliveryNativeWire
	var plan TaskWorkspaceSyncPlan
	if data == nil {
		f, prepared, wire, plan = recoveryNativePrepared(t)
	} else {
		var err error
		plan, err = NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, "local", "test-storage", "bucket", "team/project/")})
		require.NoError(t, err)
		setup := func(t *testing.T) (*Backend, *clientv3.Client, AcquireIntentInput, *CreationClaim, []byte, ed25519.PrivateKey) {
			original, raw := integrationBackend(t)
			root := ed25519.NewKeyFromSeed(make([]byte, 32))
			o := integrationOptions(original, raw)
			o.PublicationTrust = &RuntimePublicationTrust{AuthorityID: o.Identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}
			o.Clock = goodAuthorityClock()
			base, err := New(context.Background(), o)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, base.Close()) })
			archiveTrackCleanup(base)
			input := acquisitionInput(t, "recovery-data-source")
			input.Workspace = plan.workspace
			acquired, err := base.AcquireIntent(context.Background(), input)
			require.NoError(t, err)
			require.Equal(t, OutcomeCommitted, acquired.Outcome)
			claim, err := base.ClaimCreation(context.Background(), input.Workspace, input.IntentID, "recovery-preparer", time.Minute)
			require.NoError(t, err)
			t.Cleanup(func() { _ = base.ReleaseCreationClaim(context.Background(), claim) })
			dispatch, err := base.DeclareRuntimeDispatch(context.Background(), claim, dispatchInputForDeclaration())
			require.NoError(t, err)
			certificate := preparationCertificate(t, base, dispatch.Entry.Record, root, "plain", RuntimeReference{ID: uuid.NewString(), UID: uuid.NewString(), BootID: uuid.NewString()})
			return base, raw, input, claim, certificate, root
		}
		f, prepared, wire = deliveryNativeFixtureWithRecoveryProfileData(t, 0, func(TaskRecord) TaskFinalSyncPlan { p, err := plan.FinalSyncPlan(); require.NoError(t, err); return p }, &TaskWorkspaceSyncBudgetOptions{uuid.NewString(), 1, 4096}, setup, data, entries, true, true, 120*time.Second)
	}
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
	require.NoError(t, s.Close())
	require.NoError(t, target.Close())
	recoveryExecutionReopen(t, f)
	return f, wire, 0
}

func recoveryExecutionReopen(t *testing.T, f *quiescenceNativeFixture) {
	t.Helper()
	old := f.b
	o := integrationOptions(old, f.raw)
	o.TaskWorkspaceArchiveRecovery = &TaskWorkspaceArchiveRecoveryOptions{TaskWorkspaceArchiveSpoolOptions{old.archiveStore.recovery.directory, old.archiveStore.limits.Jobs, old.archiveStore.limits.JobBytes, old.archiveStore.limits.TotalBytes, old.archiveStore.limits.Entries, old.archiveStore.limits.JobIndexBytes, old.archiveStore.limits.TotalIndexBytes}}
	o.TaskWorkspaceSyncRecyclingBudget = &old.workspaceSyncBudget.options
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	o.PublicationTrust = &RuntimePublicationTrust{AuthorityID: old.publicationAuthorityID, Target: old.publicationTarget, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}
	o.Clock, o.RequestTimeout = old.authorityClock, 3*time.Second
	require.NoError(t, old.ReleaseTaskClaim(f.ctx, f.c))
	require.NoError(t, old.Close())
	next, err := New(f.ctx, o)
	require.NoError(t, err)
	f.b = next
	archiveTrackCleanup(f.b)
	t.Cleanup(func() { require.NoError(t, next.Close()) })
	f.c, err = f.b.ClaimTask(f.ctx, f.c.reference.Task, "cold-recovery", time.Minute)
	require.NoError(t, err)
	claim := f.c
	t.Cleanup(func() { _ = next.ReleaseTaskClaim(context.Background(), claim) })
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

func recoveryExecutionFirst(t *testing.T, s *taskWorkspaceSyncRecoverySource) (*taskWorkspaceSyncRecoveryBody, *taskWorkspaceSyncRecoveryCharge, *taskWorkspaceSyncRecoveryCompletion) {
	t.Helper()
	for {
		e, err := s.next(s.ctx)
		require.NoError(t, err)
		if e.kind == tar.TypeDir {
			continue
		}
		body, err := s.openBody(s.ctx, e)
		require.NoError(t, err)
		charge, err := s.reserveNext(s.ctx, body)
		require.NoError(t, err)
		q, err := s.reconcileLocal(s.ctx, body, charge)
		require.NoError(t, err)
		return body, charge, q
	}
}

func TestTaskWorkspaceSyncRecoveryNativeContinuation(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	f, wire, slot := recoveryExecutionCold(t)
	s, err := f.b.prepareTaskWorkspaceSyncRecovery(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker)
	require.NoError(t, err)
	require.NoError(t, <-wire.done)
	body, charge, q := recoveryExecutionFirst(t, s)
	require.NoError(t, s.settleLocal(f.ctx, charge, q))
	_, err = s.recycleLocal(f.ctx, charge, q)
	require.NoError(t, err)
	require.NoError(t, body.Close())
	path, frozen := filepath.Join(s.target.pin.Root, body.entry.path), s.manifest.Binding.TargetDigest
	before, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	recoveryExecutionReopen(t, f)
	s, err = f.b.prepareTaskWorkspaceSyncRecovery(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker)
	require.NoError(t, err)
	require.NoError(t, <-wire.done)
	require.NotNil(t, s.checkpoint)
	require.Equal(t, frozen, s.checkpoint.TargetDigest)
	s.localIOComplete = func(stage string) {
		require.NotEqual(t, path, filepath.Join(s.target.pin.Root, s.current.path), "checkpointed file must not be truncated or rewritten: %s", stage)
	}
	complete, err := s.run(s.ctx)
	require.NoError(t, err)
	require.True(t, complete)
	require.EqualValues(t, 1, s.completedFiles)
	require.EqualValues(t, 3, s.completedBytes)
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
	require.Equal(t, before.ModTime(), after.ModTime())
	require.NoError(t, s.Close())
}

func TestTaskWorkspaceSyncRecoveryNativeInterruptedIO(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	for _, fault := range []string{"truncate", "write", "fsync", "close"} {
		t.Run(fault, func(t *testing.T) {
			large := strings.Repeat("p", 70001)
			data := archiveTarBytes(t, []*tar.Header{{Name: "workspace", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "workspace/z", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "workspace/z/large", Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(large))}}, []string{"", "", large})
			f, wire, slot := recoveryExecutionColdData(t, data, 3)
			s, err := f.b.prepareTaskWorkspaceSyncRecovery(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker)
			require.NoError(t, err)
			require.NoError(t, <-wire.done)
			var reached atomic.Bool
			s.localIOComplete = func(stage string) {
				if stage == fault && reached.CompareAndSwap(false, true) {
					s.cancel()
				}
			}
			complete, err := s.run(s.ctx)
			require.Error(t, err)
			require.False(t, complete)
			require.True(t, reached.Load())
			require.Zero(t, s.completedFiles)
			root := s.target.pin.Root
			if fault == "write" {
				info, err := os.Stat(filepath.Join(root, "z/large"))
				require.NoError(t, err)
				require.EqualValues(t, 32768, info.Size(), "first actual write left a partial charged file")
			}
			require.NoError(t, s.Close())
			recoveryExecutionReopen(t, f)
			r, err := f.b.RecoverTaskWorkspaceSyncLocal(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker)
			require.NoError(t, err)
			require.True(t, r.RegularFilesComplete)
			require.EqualValues(t, 1, r.CompletedFiles)
			actual, err := os.ReadFile(filepath.Join(root, "z/large"))
			require.NoError(t, err)
			require.Equal(t, large, string(actual))
		})
	}
}

func TestTaskWorkspaceSyncRecoveryNativeSequential65(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	headers := []*tar.Header{{Name: "workspace", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "workspace/z", Typeflag: tar.TypeDir, Mode: 0700}}
	bodies := []string{"", ""}
	for i := 0; i < 65; i++ {
		text := fmt.Sprintf("file-%02d", i)
		headers = append(headers, &tar.Header{Name: fmt.Sprintf("workspace/z/%02d", i), Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(text))})
		bodies = append(bodies, text)
	}
	f, wire, slot := recoveryExecutionColdData(t, archiveTarBytes(t, headers, bodies), 67)
	r, err := f.b.RecoverTaskWorkspaceSyncLocal(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker)
	require.NoError(t, err)
	require.True(t, r.RegularFilesComplete)
	require.EqualValues(t, 65, r.CompletedFiles)
	require.EqualValues(t, 455, r.CompletedBytes)
	require.EqualValues(t, 67, r.Checkpoint.EntryOrdinal)
	all, err := f.raw.Get(f.ctx, fmt.Sprintf("%sp/%02x/workspace-sync-recycling-", f.b.namespace.Root(), f.c.reference.Task.Partition), clientv3.WithPrefix())
	require.NoError(t, err)
	require.Len(t, all.Kvs, 1, "S1 contains only its permanent free slot after 65 actual checkpoint GCs")
	charged, err := decodeWorkspaceSyncRecyclingSlot(all.Kvs[0])
	require.NoError(t, err)
	require.Equal(t, "free", charged.State)
	require.EqualValues(t, 196, all.Kvs[0].Version)
}
