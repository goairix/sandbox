package etcd

import (
	"context"
	"fmt"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"testing"
	"time"
)

func TestCreationClaimPermanentIntentLookup(t *testing.T) {
	b, _ := integrationBackend(t)
	input := acquisitionInput(t, "claim-lookup")
	result, err := b.AcquireIntent(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	reader, ok := any(b).(interface {
		LoadCreationIntent(context.Context, WorkspaceIdentity, string) (*CreationIntentRecord, error)
	})
	require.True(t, ok, "backend must expose permanent intent point lookup")
	record, err := reader.LoadCreationIntent(context.Background(), input.Workspace, input.IntentID)
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, "pending", record.Phase)
	require.Equal(t, input.IntentID, record.IntentID)
	missing, err := reader.LoadCreationIntent(context.Background(), input.Workspace, "missing")
	require.NoError(t, err)
	require.Nil(t, missing)
}

func TestCreationClaimAtomicOriginalLease(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	input := acquisitionInput(t, "claim-atomic")
	_, err := b.AcquireIntent(ctx, input)
	require.NoError(t, err)
	before, err := raw.Get(ctx, b.namespace.Root(), clientv3.WithPrefix())
	require.NoError(t, err)
	claimant, ok := any(b).(interface {
		ClaimCreation(context.Context, WorkspaceIdentity, string, string, time.Duration) (*CreationClaim, error)
	})
	require.True(t, ok, "backend must atomically claim the permanent pending intent")
	claim, err := claimant.ClaimCreation(ctx, input.Workspace, input.IntentID, "worker", 30*time.Second)
	require.NoError(t, err)
	require.NotNil(t, claim)
	claimed, err := raw.Get(ctx, claim.claimKey)
	require.NoError(t, err)
	require.Len(t, claimed.Kvs, 1)
	guard, err := raw.Get(ctx, claim.guardKey)
	require.NoError(t, err)
	require.Len(t, guard.Kvs, 1)
	require.Equal(t, claimed.Kvs[0].CreateRevision, guard.Kvs[0].CreateRevision)
	require.Equal(t, claimed.Kvs[0].Value, guard.Kvs[0].Value)
	require.NotZero(t, claimed.Kvs[0].Lease)
	require.Equal(t, claimed.Kvs[0].Lease, guard.Kvs[0].Lease)
	for _, kv := range before.Kvs {
		got, e := raw.Get(ctx, string(kv.Key))
		require.NoError(t, e)
		require.Equal(t, kv, got.Kvs[0])
	}
}

type creationClaimLifecycle interface {
	RenewCreationClaim(context.Context, *CreationClaim) error
	ReleaseCreationClaim(context.Context, *CreationClaim) error
	creationClaimComparisons(*CreationClaim) ([]clientv3.Cmp, error)
}

func claimFixture(t *testing.T) (*Backend, *clientv3.Client, AcquireIntentInput, *CreationClaim) {
	t.Helper()
	b, raw := integrationBackend(t)
	input := acquisitionInput(t, "creation-claim")
	_, err := b.AcquireIntent(context.Background(), input)
	require.NoError(t, err)
	claim, err := b.ClaimCreation(context.Background(), input.Workspace, input.IntentID, "worker", 30*time.Second)
	require.NoError(t, err)
	return b, raw, input, claim
}
func TestCreationClaimRenewAndRelease(t *testing.T) {
	b, raw, input, c := claimFixture(t)
	ctx := context.Background()
	lifecycle, ok := any(b).(creationClaimLifecycle)
	require.True(t, ok, "original claim must support renew, release and copied comparison capabilities")
	before, err := raw.Get(ctx, c.claimKey)
	require.NoError(t, err)
	ref := c.Reference()
	ref.WorkerID = "tampered"
	require.Equal(t, "worker", c.Reference().WorkerID)
	require.NoError(t, lifecycle.RenewCreationClaim(ctx, c))
	after, err := raw.Get(ctx, c.claimKey)
	require.NoError(t, err)
	require.Equal(t, before.Kvs, after.Kvs)
	first, err := lifecycle.creationClaimComparisons(c)
	require.NoError(t, err)
	require.Len(t, first, 24)
	first[0].Key[0] = 'X'
	first[1].TargetUnion.(*pb.Compare_Value).Value[0] = 'X'
	second, err := lifecycle.creationClaimComparisons(c)
	require.NoError(t, err)
	require.NotEqual(t, first[0].Key, second[0].Key)
	require.NotEqual(t, first[1].TargetUnion, second[1].TargetUnion)
	require.NoError(t, lifecycle.ReleaseCreationClaim(ctx, c))
	require.NoError(t, lifecycle.ReleaseCreationClaim(ctx, c))
	_, err = lifecycle.creationClaimComparisons(c)
	require.ErrorIs(t, err, ErrGuardExpired)
	require.ErrorIs(t, lifecycle.RenewCreationClaim(ctx, c), ErrGuardExpired)
	for _, key := range []string{c.claimKey, c.guardKey} {
		got, e := raw.Get(ctx, key)
		require.NoError(t, e)
		require.Empty(t, got.Kvs)
	}
	owner, err := b.LoadOwner(ctx, input.Workspace)
	require.NoError(t, err)
	require.NotNil(t, owner)
	intent, err := b.LoadCreationIntent(ctx, input.Workspace, input.IntentID)
	require.NoError(t, err)
	require.Equal(t, "pending", intent.Phase)
}
func TestCreationClaimConcurrentOneWinner(t *testing.T) {
	b, _ := integrationBackend(t)
	ctx := context.Background()
	input := acquisitionInput(t, "concurrent-claim")
	_, err := b.AcquireIntent(ctx, input)
	require.NoError(t, err)
	start := make(chan struct{})
	results := make(chan error, 16)
	claims := make(chan *CreationClaim, 16)
	for i := 0; i < 16; i++ {
		go func(i int) {
			<-start
			c, err := b.ClaimCreation(ctx, input.Workspace, input.IntentID, fmt.Sprintf("worker-%d", i), 30*time.Second)
			results <- err
			claims <- c
		}(i)
	}
	close(start)
	winners := 0
	for i := 0; i < 16; i++ {
		err := <-results
		if err == nil {
			winners++
		} else {
			require.ErrorIs(t, err, ErrConflict)
		}
	}
	require.Equal(t, 1, winners)
	for i := 0; i < 16; i++ {
		c := <-claims
		if c != nil {
			require.NoError(t, b.revokeStageLease(clientv3.LeaseID(c.Reference().LeaseID)))
		}
	}
}

func creationStage(t *testing.T, b *Backend, c *CreationClaim, suffix string) (*Stage, string) {
	t.Helper()
	cmps, err := b.creationClaimComparisons(c)
	require.NoError(t, err)
	key := b.namespace.Root() + "p/07/checkpoints/" + suffix
	stage, err := b.BeginStage(context.Background(), c.Reference().Partition, "request-"+suffix, "checkpoint", Mutation{Comparisons: cmps, Writes: []Write{{Key: key, Value: []byte("done")}}}, 30*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.ReleaseStage(context.Background(), stage)) })
	return stage, key
}
func assertCreationStageAborted(t *testing.T, b *Backend, raw *clientv3.Client, stage *Stage, key string) {
	t.Helper()
	ctx := context.Background()
	outcome, err := b.CommitStage(ctx, stage)
	require.Equal(t, OutcomeUnknown, outcome)
	require.Error(t, err)
	outcome, err = b.ResolveStage(ctx, stage.Reference())
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, outcome)
	got, err := raw.Get(ctx, key)
	require.NoError(t, err)
	require.Empty(t, got.Kvs)
}
func TestCreationClaimReacquireRejectsOldCapability(t *testing.T) {
	b, raw, input, old := claimFixture(t)
	ctx := context.Background()
	stage, key := creationStage(t, b, old, "old")
	require.NoError(t, b.ReleaseCreationClaim(ctx, old))
	next, err := b.ClaimCreation(ctx, input.Workspace, input.IntentID, "recovery", 30*time.Second)
	require.NoError(t, err)
	defer func() { require.NoError(t, b.ReleaseCreationClaim(ctx, next)) }()
	require.Greater(t, next.Reference().CreateRevision, old.Reference().CreateRevision)
	require.NotEqual(t, next.Reference().ClaimID, old.Reference().ClaimID)
	require.NotEqual(t, next.Reference().LeaseID, old.Reference().LeaseID)
	_, err = b.creationClaimComparisons(old)
	require.ErrorIs(t, err, ErrGuardExpired)
	assertCreationStageAborted(t, b, raw, stage, key)
	fresh, freshKey := creationStage(t, b, next, "fresh")
	outcome, err := b.CommitStage(ctx, fresh)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	got, err := raw.Get(ctx, freshKey)
	require.NoError(t, err)
	require.Equal(t, "done", string(got.Kvs[0].Value))
}
func TestCreationClaimControlChangeBlocksDelayedStage(t *testing.T) {
	b, raw, input, c := claimFixture(t)
	stage, key := creationStage(t, b, c, "control-change")
	control, err := b.LoadControl(context.Background(), input.Workspace.Partition(), input.SandboxID)
	require.NoError(t, err)
	control.DataGateEpoch++
	value, err := encodeDomainRecord(*control)
	require.NoError(t, err)
	ck, _, err := b.namespace.sandboxKeys(input.Workspace.Partition(), input.SandboxID, "read")
	require.NoError(t, err)
	_, err = raw.Put(context.Background(), ck, value)
	require.NoError(t, err)
	assertCreationStageAborted(t, b, raw, stage, key)
	require.ErrorIs(t, b.RenewCreationClaim(context.Background(), c), ErrGuardExpired)
	require.NoError(t, b.ReleaseCreationClaim(context.Background(), c))
}
func TestCreationClaimGuardRecreateBlocksOldStage(t *testing.T) {
	for _, which := range []string{"guard", "claim"} {
		t.Run(which, func(t *testing.T) {
			b, raw, _, c := claimFixture(t)
			stage, key := creationStage(t, b, c, "recreate-"+which)
			target := c.guardKey
			if which == "claim" {
				target = c.claimKey
			}
			before, err := raw.Get(context.Background(), target)
			require.NoError(t, err)
			_, err = raw.Delete(context.Background(), target)
			require.NoError(t, err)
			_, err = raw.Put(context.Background(), target, c.value, clientv3.WithLease(clientv3.LeaseID(c.Reference().LeaseID)))
			require.NoError(t, err)
			after, err := raw.Get(context.Background(), target)
			require.NoError(t, err)
			require.Equal(t, before.Kvs[0].Value, after.Kvs[0].Value)
			require.Equal(t, before.Kvs[0].Lease, after.Kvs[0].Lease)
			require.Greater(t, after.Kvs[0].CreateRevision, before.Kvs[0].CreateRevision)
			assertCreationStageAborted(t, b, raw, stage, key)
			require.NoError(t, b.ReleaseCreationClaim(context.Background(), c))
		})
	}
}
func TestCreationClaimStageBudget(t *testing.T) {
	b, _, _, c := claimFixture(t)
	cmps, err := b.creationClaimComparisons(c)
	require.NoError(t, err)
	lease := &creationFaultLease{Lease: b.client.Lease}
	b.client.Lease = lease
	for _, writes := range []int{26, 27} {
		mutation := Mutation{Comparisons: cmps}
		for i := 0; i < writes; i++ {
			mutation.Writes = append(mutation.Writes, Write{Key: b.namespace.Root() + fmt.Sprintf("p/07/checkpoints/budget-%d", i), Value: []byte("ok")})
		}
		previous := lease.grants.Load()
		stage, err := b.BeginStage(context.Background(), c.Reference().Partition, "budget", "check", mutation, 30*time.Second)
		if writes == 26 {
			require.NoError(t, err)
			outcome, e := b.CommitStage(context.Background(), stage)
			require.NoError(t, e)
			require.Equal(t, OutcomeCommitted, outcome)
			require.NoError(t, b.ReleaseStage(context.Background(), stage))
		} else {
			require.Nil(t, stage)
			require.ErrorIs(t, err, ErrInvalidMutation)
			require.Equal(t, previous, lease.grants.Load(), "65 operations must reject before Grant")
		}
	}
	require.NoError(t, b.ReleaseCreationClaim(context.Background(), c))
}
