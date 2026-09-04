package redis

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/google/uuid"
	redisclient "github.com/redis/go-redis/v9"
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
		_, err := s.client.Pipelined(ctx, func(pipe redisclient.Pipeliner) error {
			for _, poolKey := range poolKeys {
				digest := poolTestDigest(poolKey)
				keys := []string{"fusepool:index:" + digest, "fusepool:lock:" + digest}
				for _, poolState := range []state.FUSEPoolState{state.FUSEPoolPreparing, state.FUSEPoolPrepared, state.FUSEPoolReserved, state.FUSEPoolBinding, state.FUSEPoolConsumed} {
					keys = append(keys, "fusepool:state:"+string(poolState)+":"+digest)
					pipe.HDel(ctx, "fusepool:state-counts:"+string(poolState), digest)
				}
				pipe.Del(ctx, keys...)
				pipe.HDel(ctx, "fusepool:pool-counts", digest)
				pipe.HDel(ctx, "fusepool:membership-generations", digest)
			}
			for _, runtimeUID := range runtimeUIDs {
				digest := poolTestDigest(runtimeUID)
				pipe.Del(ctx, "fusepool:record:"+digest)
				pipe.HDel(ctx, "fusepool:record-pools", digest)
				pipe.HDel(ctx, "fusepool:reservation-deadlines", digest)
				pipe.HDel(ctx, "fusepool:record-uids", digest)
				pipe.HDel(ctx, "fusepool:record-pool-values", digest)
			}
			return nil
		})
		require.NoError(t, err)
	})
}

type commandCountHook struct {
	sscan atomic.Int32
	max   atomic.Int32
}

func (h *commandCountHook) DialHook(next redisclient.DialHook) redisclient.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) { return next(ctx, network, addr) }
}

func (h *commandCountHook) ProcessHook(next redisclient.ProcessHook) redisclient.ProcessHook {
	return func(ctx context.Context, cmd redisclient.Cmder) error {
		err := next(ctx, cmd)
		if strings.EqualFold(cmd.Name(), "sscan") && err == nil {
			h.sscan.Add(1)
			if scan, ok := cmd.(*redisclient.ScanCmd); ok {
				members, _ := scan.Val()
				for size := int32(len(members)); size > h.max.Load(); {
					if h.max.CompareAndSwap(h.max.Load(), size) {
						break
					}
				}
			}
		}
		return err
	}
}

func (h *commandCountHook) ProcessPipelineHook(next redisclient.ProcessPipelineHook) redisclient.ProcessPipelineHook {
	return next
}

type delayScriptHook struct {
	delay time.Duration
	once  sync.Once
}

func (h *delayScriptHook) DialHook(next redisclient.DialHook) redisclient.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return next(ctx, network, addr)
	}
}

func (h *delayScriptHook) ProcessHook(next redisclient.ProcessHook) redisclient.ProcessHook {
	return func(ctx context.Context, cmd redisclient.Cmder) error {
		name := strings.ToLower(cmd.Name())
		if name == "eval" || name == "evalsha" {
			h.once.Do(func() { time.Sleep(h.delay) })
		}
		return next(ctx, cmd)
	}
}

func (h *delayScriptHook) ProcessPipelineHook(next redisclient.ProcessPipelineHook) redisclient.ProcessPipelineHook {
	return next
}

type sscanActionHook struct {
	mu     sync.Mutex
	action func() error
	once   bool
	done   bool
	err    error
	calls  atomic.Int32
}

func (h *sscanActionHook) DialHook(next redisclient.DialHook) redisclient.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) { return next(ctx, network, addr) }
}

func (h *sscanActionHook) ProcessHook(next redisclient.ProcessHook) redisclient.ProcessHook {
	return func(ctx context.Context, cmd redisclient.Cmder) error {
		if err := next(ctx, cmd); err != nil {
			return err
		}
		if !strings.EqualFold(cmd.Name(), "sscan") {
			return nil
		}
		h.calls.Add(1)
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.err != nil || (h.once && h.done) {
			return nil
		}
		h.done = true
		h.err = h.action()
		return nil
	}
}

func (h *sscanActionHook) ProcessPipelineHook(next redisclient.ProcessPipelineHook) redisclient.ProcessPipelineHook {
	return next
}

func (h *sscanActionHook) actionError() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
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

func waitForRedisDeadline(t *testing.T, s *Store, deadline time.Time) {
	t.Helper()
	require.Eventually(t, func() bool {
		now, err := s.client.Time(context.Background()).Result()
		return err == nil && !now.Before(deadline)
	}, time.Second, 5*time.Millisecond)
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

func TestFUSEPoolReserveDeadlineUsesRedisServerTime(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	prepareWarmRecord(t, repo, preparingRecord(poolKey, runtimeUID))

	const ttl = 2 * time.Second
	delay := 350 * time.Millisecond
	before, err := s.client.Time(context.Background()).Result()
	require.NoError(t, err)
	s.client.AddHook(&delayScriptHook{delay: delay})
	reserved, err := repo.ReservePrepared(context.Background(), poolKey, "reservation", ttl)
	require.NoError(t, err)
	after, err := s.client.Time(context.Background()).Result()
	require.NoError(t, err)

	deadlineMillis := reserved.ReservedUntil.UnixMilli()
	assert.GreaterOrEqual(t, deadlineMillis, before.UnixMilli()+ttl.Milliseconds())
	assert.LessOrEqual(t, deadlineMillis, after.UnixMilli()+ttl.Milliseconds())
	assert.GreaterOrEqual(t, deadlineMillis-before.UnixMilli(), (ttl + delay - 100*time.Millisecond).Milliseconds())
	storedDeadline, err := s.client.HGet(context.Background(), "fusepool:reservation-deadlines", poolTestDigest(runtimeUID)).Int64()
	require.NoError(t, err)
	assert.Equal(t, deadlineMillis, storedDeadline)
	raw, err := s.client.Get(context.Background(), "fusepool:record:"+poolTestDigest(runtimeUID)).Bytes()
	require.NoError(t, err)
	var stored state.FUSEPoolRecord
	require.NoError(t, json.Unmarshal(raw, &stored))
	assert.Equal(t, reserved.ReservedUntil, stored.ReservedUntil)
}

func TestFUSEPoolCreateColdUsesRedisServerTimeAfterQueueing(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	const ttl = 2 * time.Second
	record := preparingRecord(poolKey, runtimeUID)
	record.ReservationToken = "cold-token"
	record.ReservedUntil = time.Now().Add(ttl)
	delay := 350 * time.Millisecond
	before, err := s.client.Time(context.Background()).Result()
	require.NoError(t, err)
	s.client.AddHook(&delayScriptHook{delay: delay})
	require.NoError(t, repo.CreatePreparing(context.Background(), record))
	after, err := s.client.Time(context.Background()).Result()
	require.NoError(t, err)
	listed, err := repo.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	created := listed[0]

	assert.GreaterOrEqual(t, created.UpdatedAt.UnixMilli(), before.UnixMilli()+(delay-100*time.Millisecond).Milliseconds())
	assert.LessOrEqual(t, created.UpdatedAt.UnixMilli(), after.UnixMilli())
	assert.GreaterOrEqual(t, created.ReservedUntil.UnixMilli()-before.UnixMilli(), (ttl + delay - 100*time.Millisecond).Milliseconds())
	storedDeadline, err := s.client.HGet(context.Background(), "fusepool:reservation-deadlines", poolTestDigest(runtimeUID)).Int64()
	require.NoError(t, err)
	assert.Equal(t, created.ReservedUntil.UnixMilli(), storedDeadline)
}

func TestFUSEPoolCreateColdTreatsPositiveTTLAsAuthoritativeAfterLongQueue(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	record := preparingRecord(poolKey, runtimeUID)
	record.ReservationToken = "cold-token"
	record.ReservedUntil = time.Now().Add(60 * time.Millisecond)
	s.client.AddHook(&delayScriptHook{delay: 180 * time.Millisecond})
	require.NoError(t, repo.CreatePreparing(context.Background(), record))
	serverAfter, err := s.client.Time(context.Background()).Result()
	require.NoError(t, err)
	listed, err := repo.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.GreaterOrEqual(t, listed[0].ReservedUntil.UnixMilli(), serverAfter.UnixMilli())
	assert.LessOrEqual(t, listed[0].ReservedUntil.UnixMilli(), serverAfter.Add(80*time.Millisecond).UnixMilli())
}

func TestFUSEPoolTransitionUpdatedAtUsesRedisServerTime(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	record := preparingRecord(poolKey, runtimeUID)
	require.NoError(t, repo.CreatePreparing(context.Background(), record))
	delay := 350 * time.Millisecond
	before, err := s.client.Time(context.Background()).Result()
	require.NoError(t, err)
	s.client.AddHook(&delayScriptHook{delay: delay})
	prepared, err := repo.Transition(context.Background(), runtimeUID, state.FUSEPoolPreparing, state.FUSEPoolPrepared, record.MaintainerToken, 1)
	require.NoError(t, err)
	after, err := s.client.Time(context.Background()).Result()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, prepared.UpdatedAt.UnixMilli(), before.UnixMilli()+(delay-100*time.Millisecond).Milliseconds())
	assert.LessOrEqual(t, prepared.UpdatedAt.UnixMilli(), after.UnixMilli())
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

func TestFUSEPoolStateIndexesFollowEveryMutation(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	digest := poolTestDigest(poolKey)
	uidDigest := poolTestDigest(runtimeUID)
	assertState := func(want state.FUSEPoolState, present bool) {
		t.Helper()
		totalCount, err := s.client.HGet(context.Background(), "fusepool:pool-counts", digest).Int64()
		if present {
			require.NoError(t, err)
			assert.Equal(t, int64(1), totalCount)
		} else {
			assert.ErrorIs(t, err, redisclient.Nil)
		}
		for _, poolState := range []state.FUSEPoolState{state.FUSEPoolPreparing, state.FUSEPoolPrepared, state.FUSEPoolReserved, state.FUSEPoolBinding, state.FUSEPoolConsumed} {
			member, memberErr := s.client.SIsMember(context.Background(), "fusepool:state:"+string(poolState)+":"+digest, uidDigest).Result()
			require.NoError(t, memberErr)
			assert.Equal(t, present && poolState == want, member, poolState)
			count, countErr := s.client.HGet(context.Background(), "fusepool:state-counts:"+string(poolState), digest).Int64()
			if present && poolState == want {
				require.NoError(t, countErr)
				assert.Equal(t, int64(1), count)
			} else {
				assert.ErrorIs(t, countErr, redisclient.Nil)
			}
		}
	}

	record := preparingRecord(poolKey, runtimeUID)
	require.NoError(t, repo.CreatePreparing(context.Background(), record))
	assertState(state.FUSEPoolPreparing, true)
	assert.Equal(t, "1", s.client.HGet(context.Background(), "fusepool:membership-generations", digest).Val())
	prepared, err := repo.Transition(context.Background(), runtimeUID, state.FUSEPoolPreparing, state.FUSEPoolPrepared, record.MaintainerToken, 1)
	require.NoError(t, err)
	assertState(state.FUSEPoolPrepared, true)
	assert.Equal(t, "1", s.client.HGet(context.Background(), "fusepool:membership-generations", digest).Val())
	reserved, err := repo.ReservePrepared(context.Background(), poolKey, "reservation", time.Minute)
	require.NoError(t, err)
	assertState(state.FUSEPoolReserved, true)
	assert.Equal(t, "1", s.client.HGet(context.Background(), "fusepool:membership-generations", digest).Val())
	binding, err := repo.Transition(context.Background(), runtimeUID, state.FUSEPoolReserved, state.FUSEPoolBinding, "reservation", reserved.Revision)
	require.NoError(t, err)
	assertState(state.FUSEPoolBinding, true)
	assert.Equal(t, "1", s.client.HGet(context.Background(), "fusepool:membership-generations", digest).Val())
	consumed, err := repo.Transition(context.Background(), runtimeUID, state.FUSEPoolBinding, state.FUSEPoolConsumed, "reservation", binding.Revision)
	require.NoError(t, err)
	assertState(state.FUSEPoolConsumed, true)
	assert.Equal(t, "1", s.client.HGet(context.Background(), "fusepool:membership-generations", digest).Val())
	deleted, err := repo.ConditionalDelete(context.Background(), runtimeUID, consumed.State, consumed.MaintainerToken, consumed.ReservationToken, consumed.Revision)
	require.NoError(t, err)
	assert.True(t, deleted)
	assertState("", false)
	assert.Equal(t, "2", s.client.HGet(context.Background(), "fusepool:membership-generations", digest).Val())
	assert.Equal(t, prepared.Revision+3, consumed.Revision)
}

func TestFUSEPoolStateIndexesCoverColdAndReturnTransitions(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey := poolTestID("pool")
	coldUID, warmUID := poolTestID("cold"), poolTestID("warm")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{coldUID, warmUID})
	poolDigest := poolTestDigest(poolKey)
	assertOnlyState := func(runtimeUID string, want state.FUSEPoolState) {
		t.Helper()
		uidDigest := poolTestDigest(runtimeUID)
		for _, poolState := range []state.FUSEPoolState{state.FUSEPoolPreparing, state.FUSEPoolPrepared, state.FUSEPoolReserved, state.FUSEPoolBinding, state.FUSEPoolConsumed} {
			present, err := s.client.SIsMember(context.Background(), "fusepool:state:"+string(poolState)+":"+poolDigest, uidDigest).Result()
			require.NoError(t, err)
			assert.Equal(t, poolState == want, present, poolState)
		}
	}

	cold := preparingRecord(poolKey, coldUID)
	cold.ReservationToken = "cold-reservation"
	cold.ReservedUntil = time.Now().Add(time.Minute)
	require.NoError(t, repo.CreatePreparing(context.Background(), cold))
	assertOnlyState(coldUID, state.FUSEPoolPreparing)
	_, err := repo.Transition(context.Background(), coldUID, state.FUSEPoolPreparing, state.FUSEPoolReserved, cold.ReservationToken, 1)
	require.NoError(t, err)
	assertOnlyState(coldUID, state.FUSEPoolReserved)

	prepareWarmRecord(t, repo, preparingRecord(poolKey, warmUID))
	reserved, err := repo.ReservePrepared(context.Background(), poolKey, "warm-reservation", time.Minute)
	require.NoError(t, err)
	assertOnlyState(warmUID, state.FUSEPoolReserved)
	_, err = repo.Transition(context.Background(), warmUID, state.FUSEPoolReserved, state.FUSEPoolPrepared, reserved.ReservationToken, reserved.Revision)
	require.NoError(t, err)
	assertOnlyState(warmUID, state.FUSEPoolPrepared)
	preparedCount, err := s.client.HGet(context.Background(), "fusepool:state-counts:prepared", poolDigest).Int64()
	require.NoError(t, err)
	reservedCount, err := s.client.HGet(context.Background(), "fusepool:state-counts:reserved", poolDigest).Int64()
	require.NoError(t, err)
	assert.Equal(t, int64(1), preparedCount)
	assert.Equal(t, int64(1), reservedCount)
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

func TestFUSEPoolListUsesIncrementalSSCAN(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey := poolTestID("pool")
	const records = 140
	runtimeUIDs := make([]string, 0, records)
	for range records {
		runtimeUID := poolTestID("uid")
		runtimeUIDs = append(runtimeUIDs, runtimeUID)
		require.NoError(t, repo.CreatePreparing(context.Background(), preparingRecord(poolKey, runtimeUID)))
	}
	cleanupFUSEPool(t, s, []string{poolKey}, runtimeUIDs)
	hook := &commandCountHook{}
	s.client.AddHook(hook)

	listed, err := repo.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	assert.Len(t, listed, records)
	assert.Greater(t, hook.sscan.Load(), int32(1))
	assert.Less(t, hook.max.Load(), int32(records))
}

func TestFUSEPoolListSucceedsDuringDeterministicStateChurn(t *testing.T) {
	skipIfNoRedis(t)
	listStore, mutationStore := testStore(t), testStore(t)
	listRepo, mutationRepo := NewFUSEPoolRepository(listStore), NewFUSEPoolRepository(mutationStore)
	poolKey := poolTestID("pool")
	const preparingRecords = 140
	runtimeUIDs := make([]string, 0, preparingRecords+1)
	for range preparingRecords {
		runtimeUID := poolTestID("preparing")
		runtimeUIDs = append(runtimeUIDs, runtimeUID)
		require.NoError(t, listRepo.CreatePreparing(context.Background(), preparingRecord(poolKey, runtimeUID)))
	}
	targetUID := poolTestID("churn")
	runtimeUIDs = append(runtimeUIDs, targetUID)
	prepareWarmRecord(t, mutationRepo, preparingRecord(poolKey, targetUID))
	reserved, err := mutationRepo.ReservePrepared(context.Background(), poolKey, "churn-token", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, reserved)
	cleanupFUSEPool(t, listStore, []string{poolKey}, runtimeUIDs)

	hook := &sscanActionHook{}
	hook.action = func() error {
		returned, transitionErr := mutationRepo.Transition(context.Background(), targetUID, state.FUSEPoolReserved, state.FUSEPoolPrepared, "churn-token", reserved.Revision)
		if transitionErr != nil {
			return transitionErr
		}
		reserved, transitionErr = mutationRepo.ReservePrepared(context.Background(), poolKey, "churn-token", time.Minute)
		if transitionErr != nil {
			return transitionErr
		}
		if reserved == nil || reserved.Revision != returned.Revision+1 {
			return fmt.Errorf("state churn did not re-reserve target")
		}
		return nil
	}
	listStore.client.AddHook(hook)
	listed, err := listRepo.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	require.NoError(t, hook.actionError())
	assert.Len(t, listed, len(runtimeUIDs))
	assert.Greater(t, hook.calls.Load(), int32(1))
	got := make(map[string]struct{}, len(listed))
	for _, record := range listed {
		got[record.RuntimeUID] = struct{}{}
	}
	for _, runtimeUID := range runtimeUIDs {
		assert.Contains(t, got, runtimeUID)
	}
}

func TestFUSEPoolListRetriesMembershipReplacementWithoutLosingProtectedMembers(t *testing.T) {
	skipIfNoRedis(t)
	listStore, mutationStore := testStore(t), testStore(t)
	listRepo, mutationRepo := NewFUSEPoolRepository(listStore), NewFUSEPoolRepository(mutationStore)
	poolKey := poolTestID("pool")
	const records = 140
	runtimeUIDs := make([]string, 0, records+1)
	created := make(map[string]state.FUSEPoolRecord, records)
	for range records {
		runtimeUID := poolTestID("protected")
		record := preparingRecord(poolKey, runtimeUID)
		runtimeUIDs = append(runtimeUIDs, runtimeUID)
		created[runtimeUID] = record
		require.NoError(t, listRepo.CreatePreparing(context.Background(), record))
	}
	victimUID := runtimeUIDs[0]
	replacementUID := poolTestID("replacement")
	runtimeUIDs = append(runtimeUIDs, replacementUID)
	cleanupFUSEPool(t, listStore, []string{poolKey}, runtimeUIDs)

	hook := &sscanActionHook{once: true}
	hook.action = func() error {
		victim := created[victimUID]
		deleted, deleteErr := mutationRepo.ConditionalDelete(context.Background(), victimUID, victim.State, victim.MaintainerToken, "", victim.Revision)
		if deleteErr != nil {
			return deleteErr
		}
		if !deleted {
			return fmt.Errorf("victim was not deleted")
		}
		return mutationRepo.CreatePreparing(context.Background(), preparingRecord(poolKey, replacementUID))
	}
	listStore.client.AddHook(hook)
	listed, err := listRepo.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	require.NoError(t, hook.actionError())
	require.Len(t, listed, records)
	got := make(map[string]struct{}, len(listed))
	for _, record := range listed {
		got[record.RuntimeUID] = struct{}{}
	}
	assert.NotContains(t, got, victimUID)
	assert.Contains(t, got, replacementUID)
	for _, runtimeUID := range runtimeUIDs[1:records] {
		assert.Contains(t, got, runtimeUID)
	}
}

func TestFUSEPoolStateMutationIgnoresMaxMembershipGeneration(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	prepareWarmRecord(t, repo, preparingRecord(poolKey, runtimeUID))
	poolDigest := poolTestDigest(poolKey)
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:membership-generations", poolDigest, "9007199254740991").Err())

	reserved, err := repo.ReservePrepared(context.Background(), poolKey, "reservation", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, reserved)
	binding, err := repo.Transition(context.Background(), runtimeUID, state.FUSEPoolReserved, state.FUSEPoolBinding, "reservation", reserved.Revision)
	require.NoError(t, err)
	assert.Equal(t, state.FUSEPoolBinding, binding.State)
	assert.Equal(t, "9007199254740991", s.client.HGet(context.Background(), "fusepool:membership-generations", poolDigest).Val())
}

func TestFUSEPoolMembershipGenerationOverflowRejectsCreateAndDeleteBeforeWrites(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	deletePool, createPool := poolTestID("delete-pool"), poolTestID("create-pool")
	deleteUID, createUID := poolTestID("delete-uid"), poolTestID("create-uid")
	cleanupFUSEPool(t, s, []string{deletePool, createPool}, []string{deleteUID, createUID})
	deleteRecord := preparingRecord(deletePool, deleteUID)
	require.NoError(t, repo.CreatePreparing(context.Background(), deleteRecord))
	deleteDigest := poolTestDigest(deletePool)
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:membership-generations", deleteDigest, "9007199254740991").Err())
	rawBefore, err := s.client.Get(context.Background(), "fusepool:record:"+poolTestDigest(deleteUID)).Bytes()
	require.NoError(t, err)
	deleted, err := repo.ConditionalDelete(context.Background(), deleteUID, deleteRecord.State, deleteRecord.MaintainerToken, "", deleteRecord.Revision)
	assert.False(t, deleted)
	assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
	rawAfter, err := s.client.Get(context.Background(), "fusepool:record:"+poolTestDigest(deleteUID)).Bytes()
	require.NoError(t, err)
	assert.Equal(t, rawBefore, rawAfter)

	createDigest := poolTestDigest(createPool)
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:membership-generations", createDigest, "9007199254740991").Err())
	err = repo.CreatePreparing(context.Background(), preparingRecord(createPool, createUID))
	assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
	assert.Equal(t, int64(0), s.client.Exists(context.Background(), "fusepool:record:"+poolTestDigest(createUID)).Val())
	assert.Equal(t, int64(0), s.client.SCard(context.Background(), "fusepool:index:"+createDigest).Val())
}

func TestFUSEPoolRecordRevisionMustBeSafelyIncrementable(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	prepared := prepareWarmRecord(t, repo, preparingRecord(poolKey, runtimeUID))
	recordKey := "fusepool:record:" + poolTestDigest(runtimeUID)

	unsafe := prepared
	unsafe.Revision = 9007199254740992
	unsafeRaw, err := json.Marshal(unsafe)
	require.NoError(t, err)
	require.NoError(t, s.client.Set(context.Background(), recordKey, unsafeRaw, 0).Err())
	_, err = repo.ReservePrepared(context.Background(), poolKey, "reservation", time.Minute)
	assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
	unchanged, err := s.client.Get(context.Background(), recordKey).Bytes()
	require.NoError(t, err)
	assert.Equal(t, unsafeRaw, unchanged)

	lastMutable := prepared
	lastMutable.Revision = 99999999999998
	lastMutableRaw, err := json.Marshal(lastMutable)
	require.NoError(t, err)
	require.NoError(t, s.client.Set(context.Background(), recordKey, lastMutableRaw, 0).Err())
	reserved, err := repo.ReservePrepared(context.Background(), poolKey, "reservation", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, reserved)
	assert.Equal(t, uint64(99999999999999), reserved.Revision)
	listed, err := repo.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	rawBefore, err := s.client.Get(context.Background(), recordKey).Bytes()
	require.NoError(t, err)
	_, err = repo.Transition(context.Background(), runtimeUID, state.FUSEPoolReserved, state.FUSEPoolBinding, "reservation", reserved.Revision)
	assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
	rawAfter, err := s.client.Get(context.Background(), recordKey).Bytes()
	require.NoError(t, err)
	assert.Equal(t, rawBefore, rawAfter)
}

func TestFUSEPoolHotPathsIgnoreLargeConsumedInventory(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, preparedUID := poolTestID("pool"), poolTestID("prepared")
	const consumedRecords = 1000
	runtimeUIDs := make([]string, consumedRecords+1)
	runtimeUIDs[0] = preparedUID
	for i := range consumedRecords {
		runtimeUIDs[i+1] = fmt.Sprintf("consumed:%04d:%s", i, uuid.NewString())
	}
	cleanupFUSEPool(t, s, []string{poolKey}, runtimeUIDs)
	prepareWarmRecord(t, repo, preparingRecord(poolKey, preparedUID))
	poolDigest := poolTestDigest(poolKey)
	updatedAt := time.Now().UTC()
	reservedUntil := updatedAt.Add(-time.Minute)
	var corruptUID string
	_, err := s.client.Pipelined(context.Background(), func(pipe redisclient.Pipeliner) error {
		for i := range consumedRecords {
			runtimeUID := runtimeUIDs[i+1]
			if i == consumedRecords-1 {
				corruptUID = runtimeUID
			}
			uidDigest := poolTestDigest(runtimeUID)
			record := state.FUSEPoolRecord{
				RuntimeID: "runtime-" + runtimeUID, RuntimeUID: runtimeUID, PoolKey: poolKey,
				State: state.FUSEPoolConsumed, MaintainerToken: "maintainer-a", ReservationToken: "reservation",
				ReservedUntil: reservedUntil, UpdatedAt: updatedAt, Revision: 5,
			}
			raw, marshalErr := json.Marshal(record)
			if marshalErr != nil {
				return marshalErr
			}
			pipe.Set(context.Background(), "fusepool:record:"+uidDigest, raw, 0)
			pipe.HSet(context.Background(), "fusepool:record-pools", uidDigest, poolDigest)
			pipe.HSet(context.Background(), "fusepool:reservation-deadlines", uidDigest, reservedUntil.UnixMilli())
			pipe.HSet(context.Background(), "fusepool:record-uids", uidDigest, runtimeUID)
			pipe.HSet(context.Background(), "fusepool:record-pool-values", uidDigest, poolKey)
			pipe.SAdd(context.Background(), "fusepool:index:"+poolDigest, uidDigest)
			pipe.SAdd(context.Background(), "fusepool:state:consumed:"+poolDigest, uidDigest)
		}
		pipe.HSet(context.Background(), "fusepool:pool-counts", poolDigest, consumedRecords+1)
		pipe.HSet(context.Background(), "fusepool:state-counts:consumed", poolDigest, consumedRecords)
		return nil
	})
	require.NoError(t, err)
	// A poison consumed record proves Reserve/Count do not inspect that inventory.
	require.NoError(t, s.client.Set(context.Background(), "fusepool:record:"+poolTestDigest(corruptUID), "not-json", 0).Err())

	count, err := repo.CountPreparingAndPrepared(context.Background(), poolKey)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	reserved, err := repo.ReservePrepared(context.Background(), poolKey, "hot-reservation", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, reserved)
	assert.Equal(t, preparedUID, reserved.RuntimeUID)
}

func TestFUSEPoolDetectsRecordMissingFromPoolIndex(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	prepareWarmRecord(t, repo, preparingRecord(poolKey, runtimeUID))
	require.NoError(t, s.client.SRem(context.Background(), "fusepool:index:"+poolTestDigest(poolKey), poolTestDigest(runtimeUID)).Err())

	_, err := repo.ListByPoolKey(context.Background(), poolKey)
	assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
	_, err = repo.CountPreparingAndPrepared(context.Background(), poolKey)
	assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
	got, err := repo.ReservePrepared(context.Background(), poolKey, "reservation", time.Minute)
	assert.Nil(t, got)
	assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
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
	waitForRedisDeadline(t, s, reserved.ReservedUntil)

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

func TestFUSEPoolRejectsExpiredColdPreparationWithoutHalfState(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	record := preparingRecord(poolKey, runtimeUID)
	record.ReservationToken = "reservation"
	record.ReservedUntil = time.Now().Add(-time.Second).UTC()
	err := repo.CreatePreparing(context.Background(), record)
	assert.ErrorIs(t, err, state.ErrFUSEPoolInvalidRecord)
	listed, err := repo.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	assert.Empty(t, listed)
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
	waitForRedisDeadline(t, s, expired.ReservedUntil)
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
	record.State = state.FUSEPoolPreparing
	record.MaintainerToken = ""
	assert.ErrorIs(t, repo.CreatePreparing(context.Background(), record), state.ErrFUSEPoolInvalidRecord)
	record.MaintainerToken = "maintainer-a"
	record.ReservationToken = "cold-token"
	record.ReservedUntil = time.Unix(0, 0).UTC()
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

func TestFUSEPoolCreateRejectsInvalidUTF8BeforeRedisWrites(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	invalid := string([]byte{0xff, 0xfe})
	tests := []struct {
		name   string
		mutate func(*state.FUSEPoolRecord)
	}{
		{"runtime id", func(record *state.FUSEPoolRecord) { record.RuntimeID = invalid }},
		{"runtime uid", func(record *state.FUSEPoolRecord) { record.RuntimeUID = invalid }},
		{"pool key", func(record *state.FUSEPoolRecord) { record.PoolKey = invalid }},
		{"maintainer token", func(record *state.FUSEPoolRecord) { record.MaintainerToken = invalid }},
		{"reservation token", func(record *state.FUSEPoolRecord) {
			record.ReservationToken = invalid
			record.ReservedUntil = time.Now().Add(time.Minute)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := preparingRecord(poolTestID("pool"), poolTestID("uid"))
			tt.mutate(&record)
			cleanupFUSEPool(t, s, []string{record.PoolKey}, []string{record.RuntimeUID})
			err := repo.CreatePreparing(context.Background(), record)
			assert.ErrorIs(t, err, state.ErrFUSEPoolInvalidRecord)
			uidDigest, poolDigest := poolTestDigest(record.RuntimeUID), poolTestDigest(record.PoolKey)
			assert.Equal(t, int64(0), s.client.Exists(context.Background(), "fusepool:record:"+uidDigest).Val())
			assert.Equal(t, int64(0), s.client.SCard(context.Background(), "fusepool:index:"+poolDigest).Val())
			assert.False(t, s.client.HExists(context.Background(), "fusepool:record-pools", uidDigest).Val())
			assert.False(t, s.client.HExists(context.Background(), "fusepool:reservation-deadlines", uidDigest).Val())
			assert.False(t, s.client.HExists(context.Background(), "fusepool:record-uids", uidDigest).Val())
			assert.False(t, s.client.HExists(context.Background(), "fusepool:record-pool-values", uidDigest).Val())
			assert.False(t, s.client.HExists(context.Background(), "fusepool:pool-counts", poolDigest).Val())
			assert.False(t, s.client.HExists(context.Background(), "fusepool:membership-generations", poolDigest).Val())
			assert.Equal(t, int64(0), s.client.SCard(context.Background(), "fusepool:state:preparing:"+poolDigest).Val())
			assert.False(t, s.client.HExists(context.Background(), "fusepool:state-counts:preparing", poolDigest).Val())
		})
	}
}

func TestFUSEPoolCreateLuaRejectsPoisonBeforeAnyWrite(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	poolDigest, uidDigest := poolTestDigest(poolKey), poolTestDigest(runtimeUID)
	record := preparingRecord(poolKey, runtimeUID)
	record.RuntimeID = ""
	record.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(record)
	require.NoError(t, err)

	result, err := createPreparingScript.Run(context.Background(), s.client, []string{
		"fusepool:record:" + uidDigest,
		"fusepool:index:" + poolDigest,
		"fusepool:record-pools",
		"fusepool:reservation-deadlines",
		"fusepool:record-uids",
		"fusepool:record-pool-values",
		"fusepool:pool-counts",
		"fusepool:state:preparing:" + poolDigest,
		"fusepool:state-counts:preparing",
		"fusepool:membership-generations",
	}, uidDigest, poolDigest, raw, 0, runtimeUID, poolKey).Int64()
	require.NoError(t, err)
	assert.Equal(t, poolResultCorrupt, result)
	assert.Equal(t, int64(0), s.client.Exists(context.Background(), "fusepool:record:"+uidDigest).Val())
	assert.Equal(t, int64(0), s.client.SCard(context.Background(), "fusepool:index:"+poolDigest).Val())
	assert.Equal(t, int64(0), s.client.SCard(context.Background(), "fusepool:state:preparing:"+poolDigest).Val())
}

func TestFUSEPoolLuaRFC3339MillisRoundTripsCalendarBoundaries(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	script := redisclient.NewScript(fusePoolLuaHelpers + `
local parsed = parseRFC3339Millis(ARGV[1])
if not parsed then return {-4} end
return {1, parsed, formatRFC3339Millis(parsed)}
`)
	tests := []string{
		"1969-12-31T23:59:59.999Z",
		"1970-01-01T00:00:00Z",
		"1970-01-01T00:00:00.1Z",
		"1970-01-01T00:00:00.123Z",
		"1970-01-01T00:00:00.123456789Z",
		"2000-02-29T23:59:59.999Z",
		"2024-04-30T23:59:59.001Z",
		"1999-12-31T23:59:59.999Z",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			parsedByGo, err := time.Parse(time.RFC3339Nano, input)
			require.NoError(t, err)
			result, err := script.Run(context.Background(), s.client, nil, input).Slice()
			require.NoError(t, err)
			require.Len(t, result, 3)
			assert.Equal(t, poolResultOK, result[0])
			assert.Equal(t, parsedByGo.UnixMilli(), result[1])
			assert.Equal(t, parsedByGo.UTC().Format("2006-01-02T15:04:05.000Z"), result[2])
		})
	}
}

func TestFUSEPoolRejectsStateInvariantCorruptionOnEveryPath(t *testing.T) {
	skipIfNoRedis(t)
	tests := []struct {
		name   string
		mutate func(*state.FUSEPoolRecord, *int64)
	}{
		{"empty runtime id", func(record *state.FUSEPoolRecord, _ *int64) { record.RuntimeID = "" }},
		{"empty maintainer token", func(record *state.FUSEPoolRecord, _ *int64) { record.MaintainerToken = "" }},
		{"zero updated at", func(record *state.FUSEPoolRecord, _ *int64) { record.UpdatedAt = time.Time{} }},
		{"prepared carries reservation", func(record *state.FUSEPoolRecord, deadline *int64) {
			record.ReservationToken = "impossible-token"
			record.ReservedUntil = time.Now().Add(time.Minute).UTC()
			*deadline = record.ReservedUntil.UnixMilli()
		}},
		{"deadline disagrees with json", func(record *state.FUSEPoolRecord, deadline *int64) {
			record.State = state.FUSEPoolReserved
			record.ReservationToken = "reservation"
			record.ReservedUntil = time.Now().Add(time.Minute).UTC()
			*deadline = record.ReservedUntil.UnixMilli() + 1
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testStore(t)
			repo := NewFUSEPoolRepository(s)
			poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
			cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
			prepared := prepareWarmRecord(t, repo, preparingRecord(poolKey, runtimeUID))
			record := prepared
			deadline := int64(0)
			tt.mutate(&record, &deadline)
			raw, err := json.Marshal(record)
			require.NoError(t, err)
			uidDigest := poolTestDigest(runtimeUID)
			require.NoError(t, s.client.Set(context.Background(), "fusepool:record:"+uidDigest, raw, 0).Err())
			require.NoError(t, s.client.HSet(context.Background(), "fusepool:reservation-deadlines", uidDigest, deadline).Err())

			_, err = repo.ListByPoolKey(context.Background(), poolKey)
			assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
			_, err = repo.CountPreparingAndPrepared(context.Background(), poolKey)
			assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
			got, err := repo.ReservePrepared(context.Background(), poolKey, "other", time.Minute)
			assert.Nil(t, got)
			assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
			deleted, err := repo.ConditionalDelete(context.Background(), runtimeUID, record.State, record.MaintainerToken, record.ReservationToken, record.Revision)
			assert.False(t, deleted)
			assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
		})
	}
}

func TestFUSEPoolLuaRejectsInvalidOrZeroUpdatedAtBeforeMutation(t *testing.T) {
	skipIfNoRedis(t)
	for _, updatedAt := range []string{"2e02-01-01T00:00:00Z", "0001-01-01T00:00:00.000Z"} {
		t.Run(updatedAt, func(t *testing.T) {
			s := testStore(t)
			repo := NewFUSEPoolRepository(s)
			poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
			cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
			prepareWarmRecord(t, repo, preparingRecord(poolKey, runtimeUID))
			recordKey := "fusepool:record:" + poolTestDigest(runtimeUID)
			raw, err := s.client.Get(context.Background(), recordKey).Bytes()
			require.NoError(t, err)
			var fields map[string]any
			require.NoError(t, json.Unmarshal(raw, &fields))
			fields["updated_at"] = updatedAt
			raw, err = json.Marshal(fields)
			require.NoError(t, err)
			require.NoError(t, s.client.Set(context.Background(), recordKey, raw, 0).Err())

			got, err := repo.ReservePrepared(context.Background(), poolKey, "reservation", time.Minute)
			assert.Nil(t, got)
			assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
			deleted, err := repo.ConditionalDelete(context.Background(), runtimeUID, state.FUSEPoolPrepared, "maintainer-a", "", 2)
			assert.False(t, deleted)
			assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
		})
	}
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
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:record-uids", uidDigest, embeddedUID).Err())
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:record-pool-values", uidDigest, poolKey).Err())
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:reservation-deadlines", uidDigest, 0).Err())
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:pool-counts", poolDigest, 1).Err())
	require.NoError(t, s.client.SAdd(context.Background(), "fusepool:index:"+poolDigest, uidDigest).Err())
	require.NoError(t, s.client.SAdd(context.Background(), "fusepool:state:prepared:"+poolDigest, uidDigest).Err())
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:state-counts:prepared", poolDigest, 1).Err())
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:membership-generations", poolDigest, 1).Err())

	_, err = repo.ListByPoolKey(context.Background(), poolKey)
	assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
	stored, err := s.client.Get(context.Background(), "fusepool:record:"+uidDigest).Bytes()
	require.NoError(t, err)
	assert.Equal(t, raw, stored)
	assert.True(t, s.client.SIsMember(context.Background(), "fusepool:state:prepared:"+poolDigest, uidDigest).Val())
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
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:record-uids", corruptDigest, corruptUID).Err())
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:record-pool-values", corruptDigest, poolKey).Err())
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:reservation-deadlines", corruptDigest, 0).Err())
	require.NoError(t, s.client.SAdd(context.Background(), "fusepool:index:"+poolDigest, corruptDigest).Err())
	require.NoError(t, s.client.SAdd(context.Background(), "fusepool:state:prepared:"+poolDigest, corruptDigest).Err())
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:pool-counts", poolDigest, 2).Err())
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:state-counts:prepared", poolDigest, 2).Err())

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
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:reservation-deadlines", uidDigest, 0).Err())
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:pool-counts", poolDigest, 1).Err())
	require.NoError(t, s.client.SAdd(context.Background(), "fusepool:index:"+poolDigest, uidDigest).Err())
	require.NoError(t, s.client.SAdd(context.Background(), "fusepool:state:prepared:"+poolDigest, uidDigest).Err())
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:state-counts:prepared", poolDigest, 1).Err())
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:membership-generations", poolDigest, 1).Err())

	got, err := repo.ReservePrepared(context.Background(), poolKey, "reservation", time.Minute)
	assert.Nil(t, got)
	assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
	stored, err := s.client.Get(context.Background(), "fusepool:record:"+uidDigest).Bytes()
	require.NoError(t, err)
	assert.Equal(t, raw, stored)
	assert.True(t, s.client.SIsMember(context.Background(), "fusepool:state:prepared:"+poolDigest, uidDigest).Val())
	assert.False(t, s.client.SIsMember(context.Background(), "fusepool:state:reserved:"+poolDigest, uidDigest).Val())
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
