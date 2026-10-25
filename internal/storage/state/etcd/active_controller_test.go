package etcd

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestControllerLeaseFencesOldHolderAfterExpiry(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-controller")
	require.NoError(t, repository.Publish(ctx, record))
	first := state.ActiveSandboxControllerLease{
		SandboxID: record.SandboxID, Token: "first", InstanceID: "api-1", Generation: record.Generation,
	}
	acquired, won, err := repository.AcquireController(ctx, first, time.Second)
	require.NoError(t, err)
	require.True(t, won)
	require.Greater(t, acquired.ExpiresAt, time.Now())
	second := first
	second.Token = "second"
	second.InstanceID = "api-2"
	_, won, err = repository.AcquireController(ctx, second, 5*time.Second)
	require.NoError(t, err)
	require.False(t, won)
	require.Eventually(t, func() bool {
		_, won, err := repository.AcquireController(ctx, second, 5*time.Second)
		return err == nil && won
	}, 4*time.Second, 50*time.Millisecond)
	_, err = repository.RenewController(ctx, *acquired, 5*time.Second)
	require.ErrorIs(t, err, state.ErrActiveSandboxStaleToken)
	require.ErrorIs(t, repository.ReleaseController(ctx, *acquired), state.ErrActiveSandboxStaleToken)
	require.NoError(t, repository.ReleaseController(ctx, second))
	_, won, err = repository.AcquireController(ctx, first, 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
}

func TestControllerRenewPreservesHolderAndChangesLease(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-controller-renew")
	require.NoError(t, repository.Publish(ctx, record))
	holder := state.ActiveSandboxControllerLease{
		SandboxID: record.SandboxID, Token: "holder", InstanceID: "api-1", Generation: record.Generation,
	}
	acquired, won, err := repository.AcquireController(ctx, holder, time.Second)
	require.NoError(t, err)
	require.True(t, won)
	before, err := s.client.Get(ctx, repository.controllerKey(record.SandboxID))
	require.NoError(t, err)
	require.Len(t, before.Kvs, 1)
	renewed, err := repository.RenewController(ctx, *acquired, 5*time.Second)
	require.NoError(t, err)
	require.Greater(t, renewed.ExpiresAt, acquired.ExpiresAt)
	after, err := s.client.Get(ctx, repository.controllerKey(record.SandboxID))
	require.NoError(t, err)
	require.Len(t, after.Kvs, 1)
	require.NotEqual(t, before.Kvs[0].Lease, after.Kvs[0].Lease, "changing TTL requires a new server lease")
	time.Sleep(2200 * time.Millisecond)
	other := holder
	other.Token = "other"
	_, won, err = repository.AcquireController(ctx, other, 5*time.Second)
	require.NoError(t, err)
	require.False(t, won)
	require.NoError(t, repository.ReleaseController(ctx, *renewed))
}

// A normal renewal must not create and revoke a different lease every tick.
// The exact controller key stays leased throughout, including beyond its
// original expiry, so reducing lease churn cannot sacrifice fencing.
func TestControllerRenewReusesExactLeaseAndExtendsIt(t *testing.T) {
	store := testStore(t)
	repository, err := NewActiveSandboxRepository(store, "controller-lease-reuse")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-reused-controller-lease")
	require.NoError(t, repository.Publish(ctx, record))
	holder := state.ActiveSandboxControllerLease{
		SandboxID: record.SandboxID, Token: "holder", InstanceID: "api-a", Generation: record.Generation,
	}
	acquired, won, err := repository.AcquireController(ctx, holder, 4*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	before, err := store.client.Get(ctx, repository.controllerKey(record.SandboxID))
	require.NoError(t, err)
	require.Len(t, before.Kvs, 1)
	require.NotZero(t, before.Kvs[0].Lease)

	time.Sleep(2500 * time.Millisecond)
	renewed, err := repository.RenewController(ctx, *acquired, 4*time.Second)
	require.NoError(t, err)
	after, err := store.client.Get(ctx, repository.controllerKey(record.SandboxID))
	require.NoError(t, err)
	require.Len(t, after.Kvs, 1)
	require.Equal(t, before.Kvs[0].Lease, after.Kvs[0].Lease)

	// The first grant would now be expired. The same holder still fences a
	// competing controller only if the existing lease was renewed on etcd.
	time.Sleep(2500 * time.Millisecond)
	competitor := holder
	competitor.Token = "competitor"
	competitor.InstanceID = "api-b"
	_, won, err = repository.AcquireController(ctx, competitor, 4*time.Second)
	require.NoError(t, err)
	require.False(t, won)
	require.NoError(t, repository.ReleaseController(ctx, *renewed))
}

// Records written before requested_ttl_ns was introduced must take the
// replacement-lease path. Their original TTL cannot be inferred safely from
// the advisory expiry timestamp.
func TestControllerRenewLegacyRecordReplacesLease(t *testing.T) {
	store := testStore(t)
	repository, err := NewActiveSandboxRepository(store, "controller-legacy-lease")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-legacy-controller-lease")
	require.NoError(t, repository.Publish(ctx, record))
	holder := state.ActiveSandboxControllerLease{
		SandboxID: record.SandboxID, Token: "holder", InstanceID: "api-a", Generation: record.Generation,
	}
	acquired, won, err := repository.AcquireController(ctx, holder, 10*time.Second)
	require.NoError(t, err)
	require.True(t, won)

	key := repository.controllerKey(record.SandboxID)
	before, err := store.client.Get(ctx, key)
	require.NoError(t, err)
	require.Len(t, before.Kvs, 1)
	legacyRaw, err := json.Marshal(acquired)
	require.NoError(t, err)
	response, err := store.client.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(key), "=", before.Kvs[0].ModRevision)).
		Then(clientv3.OpPut(key, string(legacyRaw), clientv3.WithLease(clientv3.LeaseID(before.Kvs[0].Lease)))).Commit()
	require.NoError(t, err)
	require.True(t, response.Succeeded)

	renewed, err := repository.RenewController(ctx, *acquired, 10*time.Second)
	require.NoError(t, err)
	after, err := store.client.Get(ctx, key)
	require.NoError(t, err)
	require.Len(t, after.Kvs, 1)
	require.NotEqual(t, before.Kvs[0].Lease, after.Kvs[0].Lease)
	require.NoError(t, repository.ReleaseController(ctx, *renewed))
}
