package etcd

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// reserveLostReplyKV arms the existing commit-reply fault after candidate
// selection. The selection itself is read-only and must not consume it.
type reserveLostReplyKV struct {
	*lostReplyKV
}

func (kv *reserveLostReplyKV) Get(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	read, err := kv.lostReplyKV.Get(ctx, key, opts...)
	if err == nil && strings.Contains(key, "/fusepool/v1/record/") {
		kv.drop.Store(true)
	}
	return read, err
}

func preparingPoolRecord(id string) state.FUSEPoolRecord {
	return state.FUSEPoolRecord{
		PreparationID: id, PoolKey: "shared-pool", State: state.FUSEPoolPreparing,
		MaintainerToken: "maintainer", Revision: 1,
	}
}

func TestPoolCreateAdmissionNeverOverbooks(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	otherStore, err := New(context.Background(), Options{
		Endpoints: []string{os.Getenv("TEST_ETCD_ENDPOINT")}, Prefix: s.prefix, Timeout: 3 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, otherStore.Close()) })
	otherRepository := NewFUSEPoolRepository(otherStore)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "refill", 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for id, candidate := range map[string]*FUSEPoolRepository{
		"preparation-a": repository, "preparation-b": otherRepository,
	} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			results <- candidate.CreatePreparingWithAdmission(ctx, preparingPoolRecord(id), "refill", 1, 10*time.Second)
		}(id)
	}
	wg.Wait()
	close(results)
	var created, rejected int
	for err := range results {
		switch {
		case err == nil:
			created++
		case errors.Is(err, state.ErrFUSEPoolConflict):
			rejected++
		default:
			t.Fatalf("unexpected admission result: %v", err)
		}
	}
	require.Equal(t, 1, created)
	require.Equal(t, 1, rejected)
	count, err := repository.CountPreparingAndPrepared(ctx, "shared-pool")
	require.NoError(t, err)
	require.Equal(t, 1, count)
	err = otherRepository.CreatePreparingWithAdmission(ctx, preparingPoolRecord("preparation-c"), "wrong", 1, 10*time.Second)
	require.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)

	first, err := repository.ReadByPreparationID(ctx, "preparation-a")
	require.NoError(t, err)
	second, err := repository.ReadByPreparationID(ctx, "preparation-b")
	require.NoError(t, err)
	require.NotEqual(t, first == nil, second == nil)
}

func TestPoolCreateRequiresLiveRefillLock(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	ctx := context.Background()
	err := repository.CreatePreparingWithAdmission(ctx, preparingPoolRecord("preparation-a"), "missing", 1, 5*time.Second)
	require.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", time.Second)
	require.NoError(t, err)
	require.True(t, won)
	err = repository.CreatePreparingWithAdmission(ctx, preparingPoolRecord("preparation-a"), "other", 1, 5*time.Second)
	require.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)
	require.NoError(t, repository.UnlockRefill(ctx, "shared-pool", "holder"))
	err = repository.CreatePreparingWithAdmission(ctx, preparingPoolRecord("preparation-a"), "holder", 1, 5*time.Second)
	require.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)
}

func TestPoolDuplicateCreateDoesNotIncreaseCapacityCount(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	record := preparingPoolRecord("same-preparation")
	require.NoError(t, repository.CreatePreparingWithAdmission(ctx, record, "holder", 2, 10*time.Second))
	require.ErrorIs(t,
		repository.CreatePreparingWithAdmission(ctx, record, "holder", 2, 10*time.Second),
		state.ErrFUSEPoolConflict)
	count, err := repository.CountPreparingAndPrepared(ctx, "shared-pool")
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestPoolRefillRenewKeepsOneLiveHolder(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", time.Second)
	require.NoError(t, err)
	require.True(t, won)
	won, err = repository.RenewRefillLock(ctx, "shared-pool", "wrong", 5*time.Second)
	require.NoError(t, err)
	require.False(t, won)
	won, err = repository.RenewRefillLock(ctx, "shared-pool", "holder", 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	time.Sleep(2200 * time.Millisecond)
	won, err = repository.TryRefillLock(ctx, "shared-pool", "other", 5*time.Second)
	require.NoError(t, err)
	require.False(t, won)
	require.ErrorIs(t, repository.UnlockRefill(ctx, "shared-pool", "other"), state.ErrFUSEPoolTokenMismatch)
	require.NoError(t, repository.UnlockRefill(ctx, "shared-pool", "holder"))
}

func TestPoolBindPreparingRuntimeFencesUIDOwner(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	for _, id := range []string{"preparation-a", "preparation-b"} {
		require.NoError(t, repository.CreatePreparingWithAdmission(ctx, preparingPoolRecord(id), "holder", 2, 10*time.Second))
	}
	_, err = repository.BindPreparingRuntime(ctx, "preparation-a", "runtime-a", "uid-a", "wrong", 1)
	require.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)
	bound, err := repository.BindPreparingRuntime(ctx, "preparation-a", "runtime-a", "uid-a", "holder", 1)
	require.NoError(t, err)
	require.Equal(t, uint64(2), bound.Revision)
	require.Equal(t, "uid-a", bound.RuntimeUID)
	_, err = repository.BindPreparingRuntime(ctx, "preparation-a", "runtime-a", "uid-a", "holder", 1)
	require.ErrorIs(t, err, state.ErrFUSEPoolCASMismatch)
	_, err = repository.BindPreparingRuntime(ctx, "preparation-b", "runtime-b", "uid-a", "holder", 1)
	require.ErrorIs(t, err, state.ErrFUSEPoolConflict)
	stillUnbound, err := repository.ReadByPreparationID(ctx, "preparation-b")
	require.NoError(t, err)
	require.Empty(t, stillUnbound.RuntimeUID)
}

func TestPoolBindRejectsExpiredPreparationLease(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	require.NoError(t, repository.CreatePreparingWithAdmission(ctx,
		preparingPoolRecord("expiring-preparation"), "holder", 1, time.Second))
	require.Eventually(t, func() bool {
		read, err := s.client.Get(ctx, repository.prepareDeadlineKey("expiring-preparation"))
		return err == nil && len(read.Kvs) == 0
	}, 4*time.Second, 50*time.Millisecond)
	_, err = repository.BindPreparingRuntime(ctx, "expiring-preparation", "runtime", "uid", "holder", 1)
	require.ErrorIs(t, err, state.ErrFUSEPoolCASMismatch)
}

func TestPoolPreparingToPreparedMovesCapacityWithinOnePool(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	require.NoError(t, repository.CreatePreparingWithAdmission(ctx,
		preparingPoolRecord("preparation"), "holder", 1, 10*time.Second))
	_, err = repository.BindPreparingRuntime(ctx, "preparation", "runtime", "uid", "holder", 1)
	require.NoError(t, err)
	_, err = repository.Transition(ctx, "preparation", state.FUSEPoolPreparing, state.FUSEPoolPrepared, "wrong", 2)
	require.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)
	prepared, err := repository.Transition(ctx, "preparation", state.FUSEPoolPreparing, state.FUSEPoolPrepared, "maintainer", 2)
	require.NoError(t, err)
	require.Equal(t, state.FUSEPoolPrepared, prepared.State)
	require.Equal(t, uint64(3), prepared.Revision)
	count, err := repository.CountPreparingAndPrepared(ctx, "shared-pool")
	require.NoError(t, err)
	require.Equal(t, 1, count)
	_, err = repository.Transition(ctx, "preparation", state.FUSEPoolPreparing, state.FUSEPoolPrepared, "maintainer", 2)
	require.ErrorIs(t, err, state.ErrFUSEPoolCASMismatch)
}

func TestPoolReservePreparedConsumesOneAvailableSlot(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	require.NoError(t, repository.CreatePreparingWithAdmission(ctx,
		preparingPoolRecord("preparation"), "holder", 1, 10*time.Second))
	_, err = repository.BindPreparingRuntime(ctx, "preparation", "runtime", "uid", "holder", 1)
	require.NoError(t, err)
	_, err = repository.Transition(ctx, "preparation", state.FUSEPoolPreparing, state.FUSEPoolPrepared, "maintainer", 2)
	require.NoError(t, err)

	reserved, err := repository.ReservePrepared(ctx, "shared-pool", "reservation", time.Second)
	require.NoError(t, err)
	require.Equal(t, state.FUSEPoolReserved, reserved.State)
	require.Equal(t, "reservation", reserved.ReservationToken)
	require.Equal(t, uint64(4), reserved.Revision)
	require.False(t, reserved.ReservedUntil.IsZero())

	count, err := repository.CountPreparingAndPrepared(ctx, "shared-pool")
	require.NoError(t, err)
	require.Zero(t, count)
	again, err := repository.ReservePrepared(ctx, "shared-pool", "other", time.Second)
	require.NoError(t, err)
	require.Nil(t, again)
	require.Eventually(t, func() bool {
		read, err := s.client.Get(ctx, repository.reservationDeadlineKey("preparation"))
		return err == nil && len(read.Kvs) == 0
	}, 4*time.Second, 50*time.Millisecond)
	stored, err := repository.ReadByPreparationID(ctx, "preparation")
	require.NoError(t, err)
	require.Equal(t, state.FUSEPoolReserved, stored.State)
}

func TestPoolReservePreparedConcurrentClientsChooseDistinctRecords(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	otherStore, err := New(context.Background(), Options{
		Endpoints: []string{os.Getenv("TEST_ETCD_ENDPOINT")}, Prefix: s.prefix, Timeout: 3 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, otherStore.Close()) })
	otherRepository := NewFUSEPoolRepository(otherStore)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	for _, id := range []string{"preparation-a", "preparation-b"} {
		require.NoError(t, repository.CreatePreparingWithAdmission(ctx,
			preparingPoolRecord(id), "holder", 2, 10*time.Second))
		_, err := repository.BindPreparingRuntime(ctx, id, id+"-runtime", id+"-uid", "holder", 1)
		require.NoError(t, err)
		_, err = repository.Transition(ctx, id, state.FUSEPoolPreparing, state.FUSEPoolPrepared, "maintainer", 2)
		require.NoError(t, err)
	}

	results := make(chan *state.FUSEPoolRecord, 2)
	errors := make(chan error, 2)
	var wg sync.WaitGroup
	for _, client := range []*FUSEPoolRepository{repository, otherRepository} {
		wg.Add(1)
		go func(client *FUSEPoolRepository) {
			defer wg.Done()
			reserved, err := client.ReservePrepared(ctx, "shared-pool", "reservation", 5*time.Second)
			results <- reserved
			errors <- err
		}(client)
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	ids := make(map[string]bool)
	for reserved := range results {
		require.NotNil(t, reserved)
		ids[reserved.PreparationID] = true
	}
	require.Len(t, ids, 2)
	count, err := repository.CountPreparingAndPrepared(ctx, "shared-pool")
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestPoolReservePreparedRejectsMissingUIDOwner(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	require.NoError(t, repository.CreatePreparingWithAdmission(ctx,
		preparingPoolRecord("preparation"), "holder", 1, 10*time.Second))
	_, err = repository.BindPreparingRuntime(ctx, "preparation", "runtime", "uid", "holder", 1)
	require.NoError(t, err)
	_, err = repository.Transition(ctx, "preparation", state.FUSEPoolPreparing, state.FUSEPoolPrepared, "maintainer", 2)
	require.NoError(t, err)
	_, err = s.client.Delete(ctx, repository.uidOwnerKey("uid"))
	require.NoError(t, err)

	reserved, err := repository.ReservePrepared(ctx, "shared-pool", "reservation", time.Minute)
	require.Nil(t, reserved)
	require.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
	count, err := repository.CountPreparingAndPrepared(ctx, "shared-pool")
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestPoolReservePreparedLostReplyRequiresExactReadback(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	require.NoError(t, repository.CreatePreparingWithAdmission(ctx,
		preparingPoolRecord("preparation"), "holder", 1, 10*time.Second))
	_, err = repository.BindPreparingRuntime(ctx, "preparation", "runtime", "uid", "holder", 1)
	require.NoError(t, err)
	_, err = repository.Transition(ctx, "preparation", state.FUSEPoolPreparing, state.FUSEPoolPrepared, "maintainer", 2)
	require.NoError(t, err)

	realKV := s.client.KV
	fault := &reserveLostReplyKV{lostReplyKV: &lostReplyKV{KV: realKV}}
	s.client.KV = fault
	reserved, err := repository.ReservePrepared(ctx, "shared-pool", "reservation", time.Minute)
	s.client.KV = realKV
	require.Nil(t, reserved)
	require.ErrorContains(t, err, "reply lost after commit")

	stored, err := repository.ReadByPreparationID(ctx, "preparation")
	require.NoError(t, err)
	require.Equal(t, state.FUSEPoolReserved, stored.State)
	require.Equal(t, "reservation", stored.ReservationToken)
	count, err := repository.CountPreparingAndPrepared(ctx, "shared-pool")
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestPoolReturnPreparedRequiresLiveReservationAndCapacity(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	require.NoError(t, repository.CreatePreparingWithAdmission(ctx,
		preparingPoolRecord("preparation-a"), "holder", 1, 10*time.Second))
	_, err = repository.BindPreparingRuntime(ctx, "preparation-a", "runtime-a", "uid-a", "holder", 1)
	require.NoError(t, err)
	_, err = repository.Transition(ctx, "preparation-a", state.FUSEPoolPreparing, state.FUSEPoolPrepared, "maintainer", 2)
	require.NoError(t, err)
	reserved, err := repository.ReservePrepared(ctx, "shared-pool", "reservation", time.Second)
	require.NoError(t, err)
	require.NoError(t, repository.CreatePreparingWithAdmission(ctx,
		preparingPoolRecord("preparation-b"), "holder", 1, 10*time.Second))

	_, err = repository.ReturnPreparedWithAdmission(ctx, "preparation-a", "wrong", reserved.Revision, 1)
	require.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)
	_, err = repository.ReturnPreparedWithAdmission(ctx, "preparation-a", "reservation", reserved.Revision, 1)
	require.ErrorIs(t, err, state.ErrFUSEPoolConflict)
	count, err := repository.CountPreparingAndPrepared(ctx, "shared-pool")
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.Eventually(t, func() bool {
		read, err := s.client.Get(ctx, repository.reservationDeadlineKey("preparation-a"))
		return err == nil && len(read.Kvs) == 0
	}, 4*time.Second, 50*time.Millisecond)
	_, err = repository.ReturnPreparedWithAdmission(ctx, "preparation-a", "reservation", reserved.Revision, 2)
	require.ErrorIs(t, err, state.ErrFUSEPoolInvalidTransition)
}

func TestPoolReturnPreparedClearsReservationAtomically(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	require.NoError(t, repository.CreatePreparingWithAdmission(ctx,
		preparingPoolRecord("preparation"), "holder", 1, 10*time.Second))
	_, err = repository.BindPreparingRuntime(ctx, "preparation", "runtime", "uid", "holder", 1)
	require.NoError(t, err)
	_, err = repository.Transition(ctx, "preparation", state.FUSEPoolPreparing, state.FUSEPoolPrepared, "maintainer", 2)
	require.NoError(t, err)
	reserved, err := repository.ReservePrepared(ctx, "shared-pool", "reservation", 5*time.Second)
	require.NoError(t, err)
	returned, err := repository.ReturnPreparedWithAdmission(ctx, "preparation", "reservation", reserved.Revision, 1)
	require.NoError(t, err)
	require.Equal(t, state.FUSEPoolPrepared, returned.State)
	require.Empty(t, returned.ReservationToken)
	require.True(t, returned.ReservedUntil.IsZero())
	require.Equal(t, reserved.Revision+1, returned.Revision)
	count, err := repository.CountPreparingAndPrepared(ctx, "shared-pool")
	require.NoError(t, err)
	require.Equal(t, 1, count)
	deadline, err := s.client.Get(ctx, repository.reservationDeadlineKey("preparation"))
	require.NoError(t, err)
	require.Empty(t, deadline.Kvs)
	_, err = repository.ReturnPreparedWithAdmission(ctx, "preparation", "reservation", reserved.Revision, 1)
	require.ErrorIs(t, err, state.ErrFUSEPoolCASMismatch)
}

func TestPoolCheckoutFencesReservationThenAllowsBoundCompletion(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	require.NoError(t, repository.CreatePreparingWithAdmission(ctx,
		preparingPoolRecord("preparation"), "holder", 1, 10*time.Second))
	_, err = repository.BindPreparingRuntime(ctx, "preparation", "runtime", "uid", "holder", 1)
	require.NoError(t, err)
	_, err = repository.Transition(ctx, "preparation", state.FUSEPoolPreparing, state.FUSEPoolPrepared, "maintainer", 2)
	require.NoError(t, err)
	reserved, err := repository.ReservePrepared(ctx, "shared-pool", "reservation", time.Second)
	require.NoError(t, err)
	_, err = repository.Transition(ctx, "preparation", state.FUSEPoolReserved, state.FUSEPoolBinding, "wrong", reserved.Revision)
	require.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)
	binding, err := repository.Transition(ctx, "preparation", state.FUSEPoolReserved, state.FUSEPoolBinding, "reservation", reserved.Revision)
	require.NoError(t, err)
	require.Equal(t, state.FUSEPoolBinding, binding.State)
	require.Equal(t, reserved.Revision+1, binding.Revision)
	require.Eventually(t, func() bool {
		read, err := s.client.Get(ctx, repository.reservationDeadlineKey("preparation"))
		return err == nil && len(read.Kvs) == 0
	}, 4*time.Second, 50*time.Millisecond)
	consumed, err := repository.Transition(ctx, "preparation", state.FUSEPoolBinding, state.FUSEPoolConsumed, "reservation", binding.Revision)
	require.NoError(t, err)
	require.Equal(t, state.FUSEPoolConsumed, consumed.State)
	require.Equal(t, binding.Revision+1, consumed.Revision)
	_, err = repository.Transition(ctx, "preparation", state.FUSEPoolBinding, state.FUSEPoolConsumed, "reservation", binding.Revision)
	require.ErrorIs(t, err, state.ErrFUSEPoolCASMismatch)
}

func TestPoolExpiredReservationCannotBeginCheckout(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	require.NoError(t, repository.CreatePreparingWithAdmission(ctx,
		preparingPoolRecord("preparation"), "holder", 1, 10*time.Second))
	_, err = repository.BindPreparingRuntime(ctx, "preparation", "runtime", "uid", "holder", 1)
	require.NoError(t, err)
	_, err = repository.Transition(ctx, "preparation", state.FUSEPoolPreparing, state.FUSEPoolPrepared, "maintainer", 2)
	require.NoError(t, err)
	reserved, err := repository.ReservePrepared(ctx, "shared-pool", "reservation", time.Second)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		read, err := s.client.Get(ctx, repository.reservationDeadlineKey("preparation"))
		return err == nil && len(read.Kvs) == 0
	}, 4*time.Second, 50*time.Millisecond)
	_, err = repository.Transition(ctx, "preparation", state.FUSEPoolReserved, state.FUSEPoolBinding, "reservation", reserved.Revision)
	require.ErrorIs(t, err, state.ErrFUSEPoolInvalidTransition)
	stored, err := repository.ReadByPreparationID(ctx, "preparation")
	require.NoError(t, err)
	require.Equal(t, state.FUSEPoolReserved, stored.State)
}
