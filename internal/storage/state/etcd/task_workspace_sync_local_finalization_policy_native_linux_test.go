//go:build linux

package etcd

import (
	"archive/tar"
	"context"
	"crypto/ed25519"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func finalizationNativeCold(t *testing.T, data []byte, entries uint64, exclude []string, deleteExtras bool, before func(string), slots ...uint32) (*quiescenceNativeFixture, *deliveryNativeWire, string) {
	t.Helper()
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, "local", "test-storage", "bucket", "team/project/"), Exclude: exclude, DeleteExtraneous: deleteExtras})
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
		input := acquisitionInput(t, "finalization-source")
		input.Workspace = plan.workspace
		acquired, err := base.AcquireIntent(context.Background(), input)
		require.NoError(t, err)
		require.Equal(t, OutcomeCommitted, acquired.Outcome)
		claim, err := base.ClaimCreation(context.Background(), input.Workspace, input.IntentID, "finalization-preparer", time.Minute)
		require.NoError(t, err)
		t.Cleanup(func() { _ = base.ReleaseCreationClaim(context.Background(), claim) })
		dispatch, err := base.DeclareRuntimeDispatch(context.Background(), claim, dispatchInputForDeclaration())
		require.NoError(t, err)
		certificate := preparationCertificate(t, base, dispatch.Entry.Record, root, "plain", RuntimeReference{ID: uuid.NewString(), UID: uuid.NewString(), BootID: uuid.NewString()})
		return base, raw, input, claim, certificate, root
	}
	budgetSlots := uint32(1)
	if len(slots) > 0 {
		budgetSlots = slots[0]
	}
	f, prepared, wire := deliveryNativeFixtureWithRecoveryProfileDataPartition(t, 0, func(TaskRecord) TaskFinalSyncPlan { p, err := plan.FinalSyncPlan(); require.NoError(t, err); return p }, &TaskWorkspaceSyncBudgetOptions{uuid.NewString(), budgetSlots, 4096}, setup, data, entries, true, true, budgetSlots == 64, 180*time.Second)
	done := deliveryBegin(f, prepared)
	deliveryWaitSent(t, wire, done)
	close(wire.body)
	r := deliveryWait(t, done)
	require.NoError(t, r.err)
	require.NoError(t, <-wire.done)
	s, err := f.b.PrepareTaskWorkspaceSyncSource(f.ctx, r.archive, plan)
	require.NoError(t, err)
	root := archiveTestDirectory(t)
	target, err := openTaskWorkspaceSyncLocalTarget(f.ctx, workspaceSyncLocalNativeWorker, plan, root)
	require.NoError(t, err)
	require.NoError(t, s.bindLocalFinalizationTarget(f.ctx, target))
	if before != nil {
		before(root)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(path, 0700)
			}
			return nil
		})
	})
	require.NoError(t, s.Close())
	require.NoError(t, target.Close())
	recoveryExecutionReopen(t, f)
	return f, wire, root
}

func TestTaskWorkspaceSyncLocalFinalizationNativePolicy(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	for _, prune := range []bool{false, true} {
		t.Run(map[bool]string{false: "retain", true: "prune"}[prune], func(t *testing.T) {
			data := archiveTarBytes(t, []*tar.Header{{Name: "workspace", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "workspace/file", Typeflag: tar.TypeReg, Mode: 0600, Size: 1}, {Name: "workspace/empty", Typeflag: tar.TypeDir, Mode: 0555}}, []string{"", "x", ""})
			f, wire, root := finalizationNativeCold(t, data, 3, []string{"protected/deep/keep"}, prune, func(root string) {
				require.NoError(t, os.MkdirAll(filepath.Join(root, "protected/deep/keep"), 0700))
				require.NoError(t, os.WriteFile(filepath.Join(root, "protected/deep/keep/secret"), []byte("keep"), 0600))
				require.NoError(t, os.WriteFile(filepath.Join(root, "protected/deep/drop"), []byte("drop"), 0600))
				require.NoError(t, os.WriteFile(filepath.Join(root, "extra"), []byte("drop"), 0600))
				require.NoError(t, os.Chmod(filepath.Join(root, "protected/deep"), 0000))
			})
			s, err := f.b.prepareTaskWorkspaceSyncRecoveryMode(f.ctx, f.c, 0, wire.destination, workspaceSyncLocalNativeWorker, true)
			require.NoError(t, err)
			s.localIOComplete = func(stage string) { t.Logf("actual effect %s", stage) }
			r, err := s.finalizeLocal(f.ctx)
			require.NoError(t, s.Close())
			require.NoError(t, err)
			require.True(t, r.LocalComplete)
			info, err := os.Stat(filepath.Join(root, "protected/deep"))
			require.NoError(t, err)
			require.Zero(t, info.Mode().Perm())
			require.NoError(t, os.Chmod(filepath.Join(root, "protected/deep"), 0700))
			got, err := os.ReadFile(filepath.Join(root, "protected/deep/keep/secret"))
			require.NoError(t, err)
			require.Equal(t, "keep", string(got))
			_, err = os.Stat(filepath.Join(root, "extra"))
			if prune {
				require.True(t, os.IsNotExist(err))
			} else {
				require.NoError(t, err)
			}
			_, err = os.Stat(filepath.Join(root, "protected/deep/drop"))
			if prune {
				require.True(t, os.IsNotExist(err))
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestTaskWorkspaceSyncLocalFinalizationNativeCompletion(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	f, wire, slot := recoveryExecutionCold(t)
	r, err := f.b.FinalizeTaskWorkspaceSyncLocal(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker)
	require.NoError(t, err)
	require.True(t, r.LocalComplete)
	require.NoError(t, <-wire.done)
	progress, complete, err := f.b.namespace.workspaceSyncLocalKeys(f.c.reference.Task)
	require.NoError(t, err)
	before, err := f.raw.Get(f.ctx, progress)
	require.NoError(t, err)
	marker, err := f.raw.Get(f.ctx, complete)
	require.NoError(t, err)
	require.Equal(t, before.Kvs[0].ModRevision, marker.Kvs[0].CreateRevision)
	r2, err := f.b.FinalizeTaskWorkspaceSyncLocal(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker)
	require.NoError(t, err)
	require.True(t, r2.LocalComplete)
	require.Equal(t, r.Completion, r2.Completion)
	require.NoError(t, <-wire.done)
	require.Zero(t, r2.CompletedFiles+r2.CreatedDirectories+r2.RemovedEntries+r2.UpdatedDirectoryModes)
	after, err := f.raw.Get(f.ctx, progress)
	require.NoError(t, err)
	require.Equal(t, before.Kvs, after.Kvs)
	_, err = f.b.RecoverTaskWorkspaceSyncLocal(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker)
	require.ErrorIs(t, err, ErrConflict)
}

func TestTaskWorkspaceSyncLocalFinalizationNativeEmpty(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	data := archiveTarBytes(t, []*tar.Header{{Name: "workspace", Typeflag: tar.TypeDir, Mode: 0000}, {Name: "workspace/a", Typeflag: tar.TypeDir, Mode: 0000}, {Name: "workspace/a/empty", Typeflag: tar.TypeDir, Mode: 0555}}, []string{"", "", ""})
	f, wire, root := finalizationNativeCold(t, data, 3, nil, false, nil)
	before, err := f.raw.Get(f.ctx, f.c.checkpointKey)
	require.NoError(t, err)
	r, err := f.b.FinalizeTaskWorkspaceSyncLocal(f.ctx, f.c, 0, wire.destination, workspaceSyncLocalNativeWorker)
	require.NoError(t, err)
	require.True(t, r.LocalComplete)
	require.Nil(t, r.Checkpoint)
	require.Zero(t, r.CompletedFiles)
	after, err := f.raw.Get(f.ctx, f.c.checkpointKey)
	require.NoError(t, err)
	require.Equal(t, before.Kvs, after.Kvs)
	info, err := os.Stat(filepath.Join(root, "a"))
	require.NoError(t, err)
	require.Zero(t, info.Mode().Perm())
	require.NoError(t, os.Chmod(filepath.Join(root, "a"), 0700))
	info, err = os.Stat(filepath.Join(root, "a/empty"))
	require.NoError(t, err)
	require.EqualValues(t, 0555, info.Mode().Perm())
}

func TestTaskWorkspaceSyncLocalFinalizationNativeLedger(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	data := archiveTarBytes(t, []*tar.Header{{Name: "workspace", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "workspace/empty", Typeflag: tar.TypeDir, Mode: 0000}}, []string{"", ""})
	f, wire, root := finalizationNativeCold(t, data, 2, []string{"protected/keep"}, true, func(root string) {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "protected/keep"), 0700))
		require.NoError(t, os.WriteFile(filepath.Join(root, "protected/drop"), []byte("drop"), 0600))
		require.NoError(t, os.Chmod(filepath.Join(root, "protected"), 0000))
	})
	s, err := f.b.prepareTaskWorkspaceSyncRecoveryMode(f.ctx, f.c, 0, wire.destination, workspaceSyncLocalNativeWorker, true)
	require.NoError(t, err)
	reached := false
	s.localIOComplete = func(stage string) {
		if stage == "directory-chmod" && len(s.finalization.progress.Ledger.Frames) > 0 && !reached {
			reached = true
			s.cancel()
		}
	}
	r, err := s.finalizeLocal(f.ctx)
	require.Error(t, err)
	require.False(t, r.LocalComplete)
	require.True(t, reached)
	require.NoError(t, s.Close())
	require.NoError(t, <-wire.done)
	progress, complete, err := f.b.namespace.workspaceSyncLocalKeys(f.c.reference.Task)
	require.NoError(t, err)
	got, err := f.raw.Get(f.ctx, progress)
	require.NoError(t, err)
	p, err := decodeWorkspaceSyncLocalProgress(got.Kvs[0])
	require.NoError(t, err)
	require.Len(t, p.Ledger.Frames, 1)
	missing, err := f.raw.Get(f.ctx, complete)
	require.NoError(t, err)
	require.Empty(t, missing.Kvs)
	recoveryExecutionReopen(t, f)
	r, err = f.b.FinalizeTaskWorkspaceSyncLocal(f.ctx, f.c, 0, wire.destination, workspaceSyncLocalNativeWorker)
	require.NoError(t, err)
	require.True(t, r.LocalComplete)
	info, err := os.Stat(filepath.Join(root, "protected"))
	require.NoError(t, err)
	require.Zero(t, info.Mode().Perm())
	require.NoError(t, os.Chmod(filepath.Join(root, "protected"), 0700))
	_, err = os.Stat(filepath.Join(root, "protected/drop"))
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(root, "protected/keep"))
	require.NoError(t, err)
}
