//go:build linux

package etcd

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func workspaceSyncRecyclingNativeBody(t *testing.T, plan TaskWorkspaceSyncPlan, budget *TaskWorkspaceSyncBudgetOptions, setup taskDeliverySetupFactory) (*quiescenceNativeFixture, *PreparedTaskWorkspaceSyncSource, *taskWorkspaceSyncBody, *taskWorkspaceSyncLocalTarget) {
	t.Helper()
	if setup == nil {
		setup = func(t *testing.T) (*Backend, *clientv3.Client, AcquireIntentInput, *CreationClaim, []byte, ed25519.PrivateKey) {
			original, raw := integrationBackend(t)
			root := ed25519.NewKeyFromSeed(make([]byte, 32))
			options := integrationOptions(original, raw)
			options.PublicationTrust = &RuntimePublicationTrust{AuthorityID: options.Identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}
			options.Clock = goodAuthorityClock()
			base, err := New(context.Background(), options)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, base.Close()) })
			archiveTrackCleanup(base)
			input := acquisitionInput(t, "recycling-local-source")
			input.Workspace = plan.workspace
			result, err := base.AcquireIntent(context.Background(), input)
			require.NoError(t, err)
			require.Equal(t, OutcomeCommitted, result.Outcome)
			claim, err := base.ClaimCreation(context.Background(), input.Workspace, input.IntentID, "local-preparer", 30*time.Second)
			require.NoError(t, err)
			t.Cleanup(func() { _ = base.ReleaseCreationClaim(context.Background(), claim) })
			dispatch, err := base.DeclareRuntimeDispatch(context.Background(), claim, dispatchInputForDeclaration())
			require.NoError(t, err)
			certificate := preparationCertificate(t, base, dispatch.Entry.Record, root, "plain", RuntimeReference{ID: uuid.NewString(), UID: uuid.NewString(), BootID: uuid.NewString()})
			return base, raw, input, claim, certificate, root
		}
	}
	f, prepared, wire := deliveryNativeFixtureWithProfileData(t, 0, func(TaskRecord) TaskFinalSyncPlan {
		p, err := plan.FinalSyncPlan()
		require.NoError(t, err)
		return p
	}, budget, setup, nil, 0, true, 60*time.Second)
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
	entry, err := s.next(f.ctx)
	require.NoError(t, err)
	body, err := s.openBody(f.ctx, entry)
	require.NoError(t, err)
	target, err := openTaskWorkspaceSyncLocalTarget(f.ctx, workspaceSyncLocalNativeWorker, plan, archiveTestDirectory(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	return f, s, body, target
}

func workspaceSyncRecyclingNativeDefault(t *testing.T) (*quiescenceNativeFixture, *PreparedTaskWorkspaceSyncSource, *taskWorkspaceSyncBody, *taskWorkspaceSyncLocalTarget) {
	t.Helper()
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, "local", "test-storage", "bucket", "team/project/")})
	require.NoError(t, err)
	return workspaceSyncRecyclingNativeBody(t, plan, &TaskWorkspaceSyncBudgetOptions{uuid.NewString(), 1, 4096}, nil)
}

func TestTaskWorkspaceSyncRecyclingNativeAdmission(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit owned Linux native fixture required")
	}
	f, s, body, target := workspaceSyncRecyclingNativeDefault(t)
	all, err := f.raw.Get(f.ctx, f.b.namespace.Root(), clientv3.WithPrefix())
	require.NoError(t, err)
	freeCount := 0
	for _, kv := range all.Kvs {
		if _, err := decodeWorkspaceSyncRecyclingSlot(kv); err == nil {
			freeCount++
		}
	}
	require.Equal(t, 256, freeCount, "operator provisioned all permanent slots before traffic")
	require.NoError(t, s.bindLocalPut(f.ctx, body, target))
	result, err := s.reservePutIntent(f.ctx, body)
	require.NoError(t, err, "original bound unread local body reserves the recycling profile")
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.True(t, body.intent.acknowledged)
	entry, err := f.b.LoadTaskWorkspaceSyncIntent(f.ctx, result.Reference)
	require.NoError(t, err)
	require.Equal(t, uint32(2), entry.Record.Version)
	witness, err := s.putLocal(f.ctx, body, target)
	require.NoError(t, err)
	require.True(t, witness.original())
	actual, err := os.ReadFile(filepath.Join(target.pin.Root, body.entry.path))
	require.NoError(t, err)
	require.Equal(t, "hello", string(actual))
	_, err = s.putLocal(f.ctx, body, target)
	require.Error(t, err, "one-shot body cannot write twice")
	occupied, err := f.raw.Get(f.ctx, body.intent.slotKey)
	require.NoError(t, err)
	require.Len(t, occupied.Kvs, 1)
	slot, err := decodeWorkspaceSyncRecyclingSlot(occupied.Kvs[0])
	require.NoError(t, err)
	require.Equal(t, "occupied", slot.State, "Task2 keeps successful IO charged until Task3")
	require.Equal(t, result.Reference.Revision, occupied.Kvs[0].ModRevision)
	require.EqualValues(t, 2, occupied.Kvs[0].Version)
	t.Logf("original profile2 IO bytes=%d hash=%s slotCreate=%d ACKmod=%d nativeVersion=%d originalPins=%d", witness.bytes, witness.sha256, occupied.Kvs[0].CreateRevision, occupied.Kvs[0].ModRevision, occupied.Kvs[0].Version, len(s.archive.owner.comparisons))
}
