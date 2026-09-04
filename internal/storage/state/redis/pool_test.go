package redis

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func poolTestID(prefix string) string {
	return prefix + ":" + uuid.NewString()
}

func poolTestDigest(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func cleanupFUSEPool(t *testing.T, s *Store, poolKeys, runtimeUIDs []string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, poolKey := range poolKeys {
			digest := poolTestDigest(poolKey)
			require.NoError(t, s.client.Del(ctx, "fusepool:index:"+digest, "fusepool:lock:"+digest).Err())
		}
		for _, runtimeUID := range runtimeUIDs {
			digest := poolTestDigest(runtimeUID)
			require.NoError(t, s.client.Del(ctx, "fusepool:record:"+digest).Err())
			require.NoError(t, s.client.HDel(ctx, "fusepool:record-pools", digest).Err())
			require.NoError(t, s.client.HDel(ctx, "fusepool:reservation-deadlines", digest).Err())
			require.NoError(t, s.client.HDel(ctx, "fusepool:record-uids", digest).Err())
			require.NoError(t, s.client.HDel(ctx, "fusepool:record-pool-values", digest).Err())
		}
	})
}

func preparingRecord(poolKey, runtimeUID string) state.FUSEPoolRecord {
	return state.FUSEPoolRecord{
		RuntimeID:       "runtime-" + runtimeUID,
		RuntimeUID:      runtimeUID,
		PoolKey:         poolKey,
		State:           state.FUSEPoolPreparing,
		MaintainerToken: "maintainer-a",
		Revision:        1,
	}
}

func prepareWarmRecord(t *testing.T, repo *FUSEPoolRepository, record state.FUSEPoolRecord) state.FUSEPoolRecord {
	t.Helper()
	require.NoError(t, repo.CreatePreparing(context.Background(), record))
	got, err := repo.Transition(context.Background(), record.RuntimeUID, state.FUSEPoolPreparing, state.FUSEPoolPrepared, record.MaintainerToken, record.Revision)
	require.NoError(t, err)
	return *got
}

func TestFUSEPoolReservePreparedIsAtomicAcrossClients(t *testing.T) {
	skipIfNoRedis(t)
	a, b := testStore(t), testStore(t)
	repoA, repoB := NewFUSEPoolRepository(a), NewFUSEPoolRepository(b)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
	cleanupFUSEPool(t, a, []string{poolKey}, []string{runtimeUID})
	prepared := prepareWarmRecord(t, repoA, preparingRecord(poolKey, runtimeUID))

	var wins atomic.Int32
	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	for _, repo := range []*FUSEPoolRepository{repoA, repoB} {
		wg.Add(1)
		go func(repo *FUSEPoolRepository) {
			defer wg.Done()
			got, err := repo.ReservePrepared(context.Background(), poolKey, uuid.NewString(), time.Minute)
			if err != nil {
				errCh <- err
				return
			}
			if got != nil {
				wins.Add(1)
				assert.Equal(t, prepared.Revision+1, got.Revision)
				assert.Equal(t, state.FUSEPoolReserved, got.State)
				assert.False(t, got.ReservedUntil.IsZero())
			}
		}(repo)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), wins.Load())
}

func TestFUSEPoolColdPreparingTransitionsDirectlyAndCannotBeStolen(t *testing.T) {
	skipIfNoRedis(t)
	a, b := testStore(t), testStore(t)
	repoA, repoB := NewFUSEPoolRepository(a), NewFUSEPoolRepository(b)
	poolKey, runtimeUID, token := poolTestID("pool"), poolTestID("uid"), uuid.NewString()
	cleanupFUSEPool(t, a, []string{poolKey}, []string{runtimeUID})
	record := preparingRecord(poolKey, runtimeUID)
	record.ReservationToken = token
	record.ReservedUntil = time.Now().Add(time.Minute).UTC()
	require.NoError(t, repoA.CreatePreparing(context.Background(), record))

	stolen, err := repoB.ReservePrepared(context.Background(), poolKey, uuid.NewString(), time.Minute)
	require.NoError(t, err)
	assert.Nil(t, stolen)
	_, err = repoB.Transition(context.Background(), runtimeUID, state.FUSEPoolPreparing, state.FUSEPoolPrepared, record.MaintainerToken, record.Revision)
	assert.ErrorIs(t, err, state.ErrFUSEPoolInvalidTransition)

	reserved, err := repoA.Transition(context.Background(), runtimeUID, state.FUSEPoolPreparing, state.FUSEPoolReserved, token, record.Revision)
	require.NoError(t, err)
	assert.Equal(t, state.FUSEPoolReserved, reserved.State)
	assert.Equal(t, uint64(2), reserved.Revision)
	stolen, err = repoB.ReservePrepared(context.Background(), poolKey, uuid.NewString(), time.Minute)
	require.NoError(t, err)
	assert.Nil(t, stolen)
}

func TestFUSEPoolTransitionRejectsRevisionTokenAndIllegalEdges(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	record := preparingRecord(poolKey, runtimeUID)
	require.NoError(t, repo.CreatePreparing(context.Background(), record))

	_, err := repo.Transition(context.Background(), runtimeUID, state.FUSEPoolPreparing, state.FUSEPoolPrepared, record.MaintainerToken, 99)
	assert.ErrorIs(t, err, state.ErrFUSEPoolCASMismatch)
	_, err = repo.Transition(context.Background(), runtimeUID, state.FUSEPoolPreparing, state.FUSEPoolPrepared, "wrong-token", record.Revision)
	assert.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)
	_, err = repo.Transition(context.Background(), runtimeUID, state.FUSEPoolPreparing, state.FUSEPoolConsumed, record.MaintainerToken, record.Revision)
	assert.ErrorIs(t, err, state.ErrFUSEPoolInvalidTransition)

	listed, err := repo.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, record.Revision, listed[0].Revision)
	assert.Equal(t, state.FUSEPoolPreparing, listed[0].State)
}

func TestFUSEPoolReservedBindingConsumedRequiresReservationToken(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	prepared := prepareWarmRecord(t, repo, preparingRecord(poolKey, runtimeUID))
	reserved, err := repo.ReservePrepared(context.Background(), poolKey, "reservation-a", time.Minute)
	require.NoError(t, err)

	_, err = repo.Transition(context.Background(), runtimeUID, state.FUSEPoolReserved, state.FUSEPoolBinding, "wrong", reserved.Revision)
	assert.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)
	binding, err := repo.Transition(context.Background(), runtimeUID, state.FUSEPoolReserved, state.FUSEPoolBinding, "reservation-a", reserved.Revision)
	require.NoError(t, err)
	consumed, err := repo.Transition(context.Background(), runtimeUID, state.FUSEPoolBinding, state.FUSEPoolConsumed, "reservation-a", binding.Revision)
	require.NoError(t, err)
	assert.Equal(t, state.FUSEPoolConsumed, consumed.State)
	assert.Equal(t, prepared.Revision+3, consumed.Revision)
}

func TestFUSEPoolConditionalDeleteRejectsStaleRecord(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	prepared := prepareWarmRecord(t, repo, preparingRecord(poolKey, runtimeUID))

	deleted, err := repo.ConditionalDelete(context.Background(), runtimeUID, prepared.State, prepared.MaintainerToken, prepared.ReservationToken, prepared.Revision-1)
	assert.False(t, deleted)
	assert.ErrorIs(t, err, state.ErrFUSEPoolCASMismatch)
	listed, err := repo.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	require.Len(t, listed, 1)

	deleted, err = repo.ConditionalDelete(context.Background(), runtimeUID, prepared.State, prepared.MaintainerToken, prepared.ReservationToken, prepared.Revision)
	require.NoError(t, err)
	assert.True(t, deleted)
	listed, err = repo.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	assert.Empty(t, listed)
}

func TestFUSEPoolConditionalDeleteChecksEveryExpectedField(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	prepareWarmRecord(t, repo, preparingRecord(poolKey, runtimeUID))
	reserved, err := repo.ReservePrepared(context.Background(), poolKey, "reservation", time.Minute)
	require.NoError(t, err)

	deleted, err := repo.ConditionalDelete(context.Background(), runtimeUID, state.FUSEPoolPrepared, reserved.MaintainerToken, reserved.ReservationToken, reserved.Revision)
	assert.False(t, deleted)
	assert.ErrorIs(t, err, state.ErrFUSEPoolConflict)
	deleted, err = repo.ConditionalDelete(context.Background(), runtimeUID, reserved.State, "wrong-maintainer", reserved.ReservationToken, reserved.Revision)
	assert.False(t, deleted)
	assert.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)
	deleted, err = repo.ConditionalDelete(context.Background(), runtimeUID, reserved.State, reserved.MaintainerToken, "wrong-reservation", reserved.Revision)
	assert.False(t, deleted)
	assert.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)

	listed, err := repo.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, reserved.Revision, listed[0].Revision)
}

func TestFUSEPoolConditionalDeleteRejectsCorruptPoolMetadata(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	prepared := prepareWarmRecord(t, repo, preparingRecord(poolKey, runtimeUID))
	recordKey := "fusepool:record:" + poolTestDigest(runtimeUID)
	raw, err := s.client.Get(context.Background(), recordKey).Bytes()
	require.NoError(t, err)
	var corrupt state.FUSEPoolRecord
	require.NoError(t, json.Unmarshal(raw, &corrupt))
	corrupt.PoolKey = poolTestID("different-pool")
	raw, err = json.Marshal(corrupt)
	require.NoError(t, err)
	require.NoError(t, s.client.Set(context.Background(), recordKey, raw, 0).Err())

	deleted, err := repo.ConditionalDelete(context.Background(), runtimeUID, prepared.State, prepared.MaintainerToken, prepared.ReservationToken, prepared.Revision)
	assert.False(t, deleted)
	assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
	exists, err := s.Exists(context.Background(), recordKey)
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestFUSEPoolMissingRecordReturnsDomainError(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	runtimeUID := poolTestID("missing-uid")
	cleanupFUSEPool(t, s, nil, []string{runtimeUID})

	_, err := repo.Transition(context.Background(), runtimeUID, state.FUSEPoolPreparing, state.FUSEPoolPrepared, "", 1)
	assert.ErrorIs(t, err, state.ErrFUSEPoolNotFound)
	deleted, err := repo.ConditionalDelete(context.Background(), runtimeUID, state.FUSEPoolPreparing, "", "", 1)
	assert.False(t, deleted)
	assert.ErrorIs(t, err, state.ErrFUSEPoolNotFound)
}

func TestFUSEPoolRefillLockOwnershipAndExpiry(t *testing.T) {
	skipIfNoRedis(t)
	a, b := testStore(t), testStore(t)
	repoA, repoB := NewFUSEPoolRepository(a), NewFUSEPoolRepository(b)
	poolKey := poolTestID("pool")
	cleanupFUSEPool(t, a, []string{poolKey}, nil)

	locked, err := repoA.TryRefillLock(context.Background(), poolKey, "owner-a", 100*time.Millisecond)
	require.NoError(t, err)
	require.True(t, locked)
	locked, err = repoB.TryRefillLock(context.Background(), poolKey, "owner-b", time.Second)
	require.NoError(t, err)
	assert.False(t, locked)
	assert.ErrorIs(t, repoB.UnlockRefill(context.Background(), poolKey, "owner-b"), state.ErrFUSEPoolTokenMismatch)
	locked, err = repoB.TryRefillLock(context.Background(), poolKey, "owner-b", time.Second)
	require.NoError(t, err)
	assert.False(t, locked)

	require.Eventually(t, func() bool {
		ok, lockErr := repoB.TryRefillLock(context.Background(), poolKey, "owner-b", time.Second)
		return lockErr == nil && ok
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, repoB.UnlockRefill(context.Background(), poolKey, "owner-b"))
}

func TestFUSEPoolCountAndListAreCrossClientAndStable(t *testing.T) {
	skipIfNoRedis(t)
	a, b := testStore(t), testStore(t)
	repoA, repoB := NewFUSEPoolRepository(a), NewFUSEPoolRepository(b)
	poolKey := poolTestID("pool")
	runtimeUIDs := []string{poolTestID("uid-c"), poolTestID("uid-a"), poolTestID("uid-b")}
	cleanupFUSEPool(t, a, []string{poolKey}, runtimeUIDs)
	require.NoError(t, repoA.CreatePreparing(context.Background(), preparingRecord(poolKey, runtimeUIDs[0])))
	prepareWarmRecord(t, repoA, preparingRecord(poolKey, runtimeUIDs[1]))
	prepareWarmRecord(t, repoA, preparingRecord(poolKey, runtimeUIDs[2]))
	reserved, err := repoA.ReservePrepared(context.Background(), poolKey, "reservation", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, reserved)

	count, err := repoB.CountPreparingAndPrepared(context.Background(), poolKey)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
	listed, err := repoB.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	require.Len(t, listed, 3)
	gotUIDs := []string{listed[0].RuntimeUID, listed[1].RuntimeUID, listed[2].RuntimeUID}
	wantUIDs := append([]string(nil), runtimeUIDs...)
	sort.Strings(wantUIDs)
	assert.Equal(t, wantUIDs, gotUIDs)
}

func TestFUSEPoolExpiredReservationRemainsReserved(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	prepareWarmRecord(t, repo, preparingRecord(poolKey, runtimeUID))
	reserved, err := repo.ReservePrepared(context.Background(), poolKey, "reservation", 40*time.Millisecond)
	require.NoError(t, err)
	require.NotNil(t, reserved)
	time.Sleep(60 * time.Millisecond)

	got, err := repo.ReservePrepared(context.Background(), poolKey, "other", time.Minute)
	require.NoError(t, err)
	assert.Nil(t, got)
	listed, err := repo.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, state.FUSEPoolReserved, listed[0].State)
	assert.Equal(t, "reservation", listed[0].ReservationToken)
	_, err = repo.Transition(context.Background(), runtimeUID, state.FUSEPoolReserved, state.FUSEPoolBinding, "reservation", reserved.Revision)
	assert.ErrorIs(t, err, state.ErrFUSEPoolInvalidTransition)
}

func TestFUSEPoolExpiredColdPreparationCannotBecomeReserved(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	record := preparingRecord(poolKey, runtimeUID)
	record.ReservationToken = "reservation"
	record.ReservedUntil = time.Now().Add(30 * time.Millisecond).UTC()
	require.NoError(t, repo.CreatePreparing(context.Background(), record))
	time.Sleep(50 * time.Millisecond)

	_, err := repo.Transition(context.Background(), runtimeUID, state.FUSEPoolPreparing, state.FUSEPoolReserved, record.ReservationToken, record.Revision)
	assert.ErrorIs(t, err, state.ErrFUSEPoolInvalidTransition)
	listed, err := repo.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, state.FUSEPoolPreparing, listed[0].State)
	assert.Equal(t, record.Revision, listed[0].Revision)
}

func TestFUSEPoolReturnPreparedRequiresLiveOwnedReservation(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey := poolTestID("pool")
	runtimeUIDs := []string{poolTestID("uid-live"), poolTestID("uid-expired")}
	cleanupFUSEPool(t, s, []string{poolKey}, runtimeUIDs)

	prepareWarmRecord(t, repo, preparingRecord(poolKey, runtimeUIDs[0]))
	live, err := repo.ReservePrepared(context.Background(), poolKey, "live-token", time.Minute)
	require.NoError(t, err)
	returned, err := repo.Transition(context.Background(), live.RuntimeUID, state.FUSEPoolReserved, state.FUSEPoolPrepared, "live-token", live.Revision)
	require.NoError(t, err)
	assert.Equal(t, state.FUSEPoolPrepared, returned.State)
	assert.Empty(t, returned.ReservationToken)
	assert.True(t, returned.ReservedUntil.IsZero())
	assert.Equal(t, live.Revision+1, returned.Revision)

	prepareWarmRecord(t, repo, preparingRecord(poolKey, runtimeUIDs[1]))
	expired, err := repo.ReservePrepared(context.Background(), poolKey, "expired-token", 30*time.Millisecond)
	require.NoError(t, err)
	time.Sleep(50 * time.Millisecond)
	_, err = repo.Transition(context.Background(), expired.RuntimeUID, state.FUSEPoolReserved, state.FUSEPoolPrepared, "expired-token", expired.Revision)
	assert.ErrorIs(t, err, state.ErrFUSEPoolInvalidTransition)
	listed, err := repo.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	for _, record := range listed {
		if record.RuntimeUID == expired.RuntimeUID {
			assert.Equal(t, state.FUSEPoolReserved, record.State)
			assert.Equal(t, expired.Revision, record.Revision)
		}
	}
}

func TestFUSEPoolDuplicateCreateLeavesNoHalfState(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolA, poolB, runtimeUID := poolTestID("pool-a"), poolTestID("pool-b"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolA, poolB}, []string{runtimeUID})
	require.NoError(t, repo.CreatePreparing(context.Background(), preparingRecord(poolA, runtimeUID)))
	duplicate := preparingRecord(poolB, runtimeUID)
	err := repo.CreatePreparing(context.Background(), duplicate)
	assert.ErrorIs(t, err, state.ErrFUSEPoolConflict)

	a, err := repo.ListByPoolKey(context.Background(), poolA)
	require.NoError(t, err)
	assert.Len(t, a, 1)
	b, err := repo.ListByPoolKey(context.Background(), poolB)
	require.NoError(t, err)
	assert.Empty(t, b)
}

func TestFUSEPoolRejectsInvalidCreateAndReportsCorruption(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	record := preparingRecord(poolKey, runtimeUID)
	record.Revision = 0
	assert.ErrorIs(t, repo.CreatePreparing(context.Background(), record), state.ErrFUSEPoolInvalidRecord)
	record.Revision = 1
	record.State = state.FUSEPoolPrepared
	assert.ErrorIs(t, repo.CreatePreparing(context.Background(), record), state.ErrFUSEPoolInvalidRecord)
	listed, err := repo.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	assert.Empty(t, listed)

	digest := poolTestDigest(runtimeUID)
	poolDigest := poolTestDigest(poolKey)
	require.NoError(t, s.client.Set(context.Background(), "fusepool:record:"+digest, "not-json", 0).Err())
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:record-pools", digest, poolDigest).Err())
	require.NoError(t, s.client.SAdd(context.Background(), "fusepool:index:"+poolDigest, digest).Err())
	_, err = repo.ListByPoolKey(context.Background(), poolKey)
	assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
	_, err = repo.CountPreparingAndPrepared(context.Background(), poolKey)
	assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
}

func TestFUSEPoolRejectsIndexPointingAtDifferentRuntimeUID(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, indexedUID, embeddedUID := poolTestID("pool"), poolTestID("indexed-uid"), poolTestID("embedded-uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{indexedUID})
	record := preparingRecord(poolKey, embeddedUID)
	record.State = state.FUSEPoolPrepared
	record.Revision = 2
	record.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(record)
	require.NoError(t, err)
	uidDigest := poolTestDigest(indexedUID)
	poolDigest := poolTestDigest(poolKey)
	require.NoError(t, s.client.Set(context.Background(), "fusepool:record:"+uidDigest, raw, 0).Err())
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:record-pools", uidDigest, poolDigest).Err())
	require.NoError(t, s.client.SAdd(context.Background(), "fusepool:index:"+poolDigest, uidDigest).Err())

	_, err = repo.ListByPoolKey(context.Background(), poolKey)
	assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
	_, err = repo.CountPreparingAndPrepared(context.Background(), poolKey)
	assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
}

func TestFUSEPoolReserveValidatesWholeIndexBeforeMutation(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, preparedUID := poolTestID("pool"), poolTestID("prepared-uid")
	preparedDigest := poolTestDigest(preparedUID)
	corruptUID := poolTestID("corrupt-uid")
	for poolTestDigest(corruptUID) < preparedDigest {
		corruptUID = poolTestID("corrupt-uid")
	}
	cleanupFUSEPool(t, s, []string{poolKey}, []string{preparedUID, corruptUID})
	prepared := prepareWarmRecord(t, repo, preparingRecord(poolKey, preparedUID))
	corruptDigest := poolTestDigest(corruptUID)
	poolDigest := poolTestDigest(poolKey)
	require.NoError(t, s.client.Set(context.Background(), "fusepool:record:"+corruptDigest, "not-json", 0).Err())
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:record-pools", corruptDigest, poolDigest).Err())
	require.NoError(t, s.client.SAdd(context.Background(), "fusepool:index:"+poolDigest, corruptDigest).Err())

	got, err := repo.ReservePrepared(context.Background(), poolKey, "reservation", time.Minute)
	assert.Nil(t, got)
	assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
	raw, err := s.client.Get(context.Background(), "fusepool:record:"+preparedDigest).Bytes()
	require.NoError(t, err)
	var unchanged state.FUSEPoolRecord
	require.NoError(t, json.Unmarshal(raw, &unchanged))
	assert.Equal(t, state.FUSEPoolPrepared, unchanged.State)
	assert.Equal(t, prepared.Revision, unchanged.Revision)
}

func TestFUSEPoolReserveRejectsRecordStoredUnderDifferentUIDDigest(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, indexedUID, embeddedUID := poolTestID("pool"), poolTestID("indexed-uid"), poolTestID("embedded-uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{indexedUID})
	record := preparingRecord(poolKey, embeddedUID)
	record.State = state.FUSEPoolPrepared
	record.Revision = 2
	record.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(record)
	require.NoError(t, err)
	uidDigest := poolTestDigest(indexedUID)
	poolDigest := poolTestDigest(poolKey)
	require.NoError(t, s.client.Set(context.Background(), "fusepool:record:"+uidDigest, raw, 0).Err())
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:record-pools", uidDigest, poolDigest).Err())
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:record-uids", uidDigest, indexedUID).Err())
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:record-pool-values", uidDigest, poolKey).Err())
	require.NoError(t, s.client.SAdd(context.Background(), "fusepool:index:"+poolDigest, uidDigest).Err())

	got, err := repo.ReservePrepared(context.Background(), poolKey, "reservation", time.Minute)
	assert.Nil(t, got)
	assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
}

func TestFUSEPoolNamespaceDoesNotMatchSandboxSessionGlob(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, runtimeUID := "pool:*?["+uuid.NewString(), "uid:/../*"+uuid.NewString()
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	require.NoError(t, repo.CreatePreparing(context.Background(), preparingRecord(poolKey, runtimeUID)))

	keys, err := s.Keys(context.Background(), "sandbox:*")
	require.NoError(t, err)
	for _, key := range keys {
		assert.NotContains(t, key, runtimeUID)
		assert.NotContains(t, key, poolKey)
		assert.False(t, len(key) >= len("fusepool:") && key[:len("fusepool:")] == "fusepool:")
	}
	fuseKeys, err := s.Keys(context.Background(), "fusepool:*")
	require.NoError(t, err)
	require.NotEmpty(t, fuseKeys)
	for _, key := range fuseKeys {
		assert.NotContains(t, key, runtimeUID)
		assert.NotContains(t, key, poolKey)
	}
}

func TestFUSEPoolErrorsDoNotLeakRecordValuesAndContextCancellationPropagates(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("secret-runtime")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	record := preparingRecord(poolKey, runtimeUID)
	record.ReservationToken = "secret-reservation-token"
	record.ReservedUntil = time.Now().Add(time.Minute).UTC()
	require.NoError(t, repo.CreatePreparing(context.Background(), record))

	_, err := repo.Transition(context.Background(), runtimeUID, state.FUSEPoolPreparing, state.FUSEPoolReserved, "wrong", record.Revision)
	require.Error(t, err)
	assert.False(t, errors.Is(err, context.Canceled))
	assert.NotContains(t, err.Error(), record.ReservationToken)
	assert.NotContains(t, err.Error(), runtimeUID)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = repo.ListByPoolKey(ctx, poolKey)
	assert.ErrorIs(t, err, context.Canceled)
}
