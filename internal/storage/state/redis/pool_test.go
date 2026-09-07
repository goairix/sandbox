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
				for _, poolState := range fusePoolStates {
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
				pipe.HDel(ctx, fusePoolRuntimeUIDOwners, digest)
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

func preparingIntent(poolKey, preparationID string) state.FUSEPoolRecord {
	return state.FUSEPoolRecord{
		PreparationID: preparationID,
		PoolKey:       poolKey, State: state.FUSEPoolPreparing,
		MaintainerToken: "maintainer-a", Revision: 1,
	}
}

func TestFUSEPoolAdmissionRegistersIntentBeforeRuntimeAndUsesRedisDeadline(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, preparationID := poolTestID("pool"), poolTestID("preparation")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{preparationID})
	require.True(t, mustRefillLock(t, repo, poolKey, "controller", time.Second))

	before, err := repo.ServerTime(context.Background())
	require.NoError(t, err)
	require.NoError(t, repo.CreatePreparingWithAdmission(context.Background(), preparingIntent(poolKey, preparationID), "controller", 1, 2*time.Second))
	after, err := repo.ServerTime(context.Background())
	require.NoError(t, err)
	records, err := repo.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Empty(t, records[0].RuntimeID)
	assert.Empty(t, records[0].RuntimeUID)
	assert.GreaterOrEqual(t, records[0].PrepareUntil.UnixMilli(), before.Add(2*time.Second).UnixMilli())
	assert.LessOrEqual(t, records[0].PrepareUntil.UnixMilli(), after.Add(2*time.Second).UnixMilli())

	err = repo.CreatePreparingWithAdmission(context.Background(), preparingIntent(poolKey, poolTestID("second")), "controller", 1, time.Second)
	assert.ErrorIs(t, err, state.ErrFUSEPoolConflict)
}

func TestFUSEPoolListPoolKeysReturnsEveryDistinctConfiguration(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolA, poolB := poolTestID("pool-a"), poolTestID("pool-b")
	uidA, uidB, uidC := poolTestID("uid-a"), poolTestID("uid-b"), poolTestID("uid-c")
	cleanupFUSEPool(t, s, []string{poolA, poolB}, []string{uidA, uidB, uidC})
	require.NoError(t, repo.createPreparingForTest(context.Background(), preparingRecord(poolA, uidA)))
	require.NoError(t, repo.createPreparingForTest(context.Background(), preparingRecord(poolA, uidB)))
	require.NoError(t, repo.createPreparingForTest(context.Background(), preparingRecord(poolB, uidC)))

	keys, err := repo.ListPoolKeys(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{poolA, poolB}, keys)
}

func TestFUSEPoolBindPreparingRuntimeFencesLockAndRuntimeUID(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey := poolTestID("pool")
	first, second := poolTestID("preparation"), poolTestID("preparation")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{first, second, "uid-a", "uid-b"})
	require.True(t, mustRefillLock(t, repo, poolKey, "controller", time.Second))
	for _, id := range []string{first, second} {
		require.NoError(t, repo.CreatePreparingWithAdmission(context.Background(), preparingIntent(poolKey, id), "controller", 2, time.Second))
	}
	bound, err := repo.BindPreparingRuntime(context.Background(), first, "runtime-a", "uid-a", "controller", 1)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), bound.Revision)
	_, err = repo.BindPreparingRuntime(context.Background(), second, "runtime-b", "uid-a", "controller", 1)
	assert.ErrorIs(t, err, state.ErrFUSEPoolConflict)
	_, err = repo.BindPreparingRuntime(context.Background(), first, "runtime-changed", "uid-changed", "controller", 2)
	assert.ErrorIs(t, err, state.ErrFUSEPoolConflict)
	_, err = repo.BindPreparingRuntime(context.Background(), second, "runtime-b", "uid-b", "stale-controller", 1)
	assert.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)
}

func TestFUSEPoolCleanupClaimIsExclusiveRetryableAndRecoverable(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, id := poolTestID("pool"), poolTestID("preparation")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{id, "uid-cleanup"})
	require.True(t, mustRefillLock(t, repo, poolKey, "controller", time.Second))
	require.NoError(t, repo.CreatePreparingWithAdmission(context.Background(), preparingIntent(poolKey, id), "controller", 1, time.Second))
	claimed, err := repo.ClaimCleanup(context.Background(), id, state.FUSEPoolPreparing, "maintainer-a", "", 1, "runtime-cleanup", "uid-cleanup", "cleaner-a", 80*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, state.FUSEPoolCleanup, claimed.State)
	assert.NotEmpty(t, claimed.CleanupUntil)
	assert.Equal(t, "runtime-cleanup", claimed.RuntimeID)
	assert.Equal(t, "uid-cleanup", claimed.RuntimeUID)

	same, err := repo.ClaimCleanup(context.Background(), id, state.FUSEPoolCleanup, "maintainer-a", "", claimed.Revision, "runtime-cleanup", "uid-cleanup", "cleaner-a", time.Second)
	require.NoError(t, err)
	assert.Equal(t, claimed.Revision, same.Revision, "same owner retry must be idempotent")
	_, err = repo.ClaimCleanup(context.Background(), id, state.FUSEPoolCleanup, "maintainer-a", "", claimed.Revision, "runtime-cleanup", "uid-cleanup", "cleaner-b", time.Second)
	assert.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)
	waitForRedisDeadline(t, s, claimed.CleanupUntil)
	taken, err := repo.ClaimCleanup(context.Background(), id, state.FUSEPoolCleanup, "maintainer-a", "", claimed.Revision, "runtime-cleanup", "uid-cleanup", "cleaner-b", time.Second)
	require.NoError(t, err)
	assert.Greater(t, taken.Revision, claimed.Revision)
	deleted, err := repo.DeleteCleanup(context.Background(), id, "cleaner-b", taken.Revision)
	require.NoError(t, err)
	assert.True(t, deleted)
}

func TestFUSEPoolLateCleanupEvidenceRemainsListableAndTakeoverDeletes(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, id, runtimeUID := poolTestID("pool"), poolTestID("preparation"), poolTestID("runtime-uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{id, runtimeUID})
	require.True(t, mustRefillLock(t, repo, poolKey, "controller", time.Second))
	require.NoError(t, repo.CreatePreparingWithAdmission(context.Background(), preparingIntent(poolKey, id), "controller", 1, time.Second))
	first, err := repo.ClaimCleanup(context.Background(), id, state.FUSEPoolPreparing, "maintainer-a", "", 1, "", "", "cleaner-a", 80*time.Millisecond)
	require.NoError(t, err)
	late, err := repo.ClaimCleanup(context.Background(), id, state.FUSEPoolCleanup, "maintainer-a", "", first.Revision, "runtime-late", runtimeUID, "cleaner-a", time.Second)
	require.NoError(t, err)
	assert.Equal(t, "runtime-late", late.RuntimeID)
	assert.Equal(t, runtimeUID, late.RuntimeUID)
	assert.Equal(t, first.CleanupUntil, late.CleanupUntil, "late evidence must not silently renew cleanup ownership")
	assert.Equal(t, first.Revision+1, late.Revision)
	_, err = repo.ClaimCleanup(context.Background(), id, state.FUSEPoolCleanup, "maintainer-a", "", late.Revision, "runtime-other", "uid-other", "cleaner-a", time.Second)
	assert.ErrorIs(t, err, state.ErrFUSEPoolConflict, "same-token retry must not overwrite established runtime evidence")
	listed, err := repo.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	require.Len(t, listed, 1, "failed runtime removal must leave a readable cleanup tombstone")
	assert.Equal(t, runtimeUID, listed[0].RuntimeUID)
	waitForRedisDeadline(t, s, late.CleanupUntil)
	taken, err := repo.ClaimCleanup(context.Background(), id, state.FUSEPoolCleanup, "maintainer-a", "", late.Revision, "runtime-late", runtimeUID, "cleaner-b", time.Second)
	require.NoError(t, err)
	deleted, err := repo.DeleteCleanup(context.Background(), id, "cleaner-b", taken.Revision)
	require.NoError(t, err)
	assert.True(t, deleted)
}

func TestFUSEPoolPublishReservedStartsTTLAtRedisTransitionAndChecksLock(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, id := poolTestID("pool"), poolTestID("preparation")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{id, "uid-a"})
	require.True(t, mustRefillLock(t, repo, poolKey, "controller", time.Second))
	require.NoError(t, repo.CreatePreparingWithAdmission(context.Background(), preparingIntent(poolKey, id), "controller", 1, time.Second))
	bound, err := repo.BindPreparingRuntime(context.Background(), id, "runtime-a", "uid-a", "controller", 1)
	require.NoError(t, err)
	require.NoError(t, repo.UnlockRefill(context.Background(), poolKey, "controller"))
	require.True(t, mustRefillLock(t, repo, poolKey, "controller-new", time.Second))
	_, err = repo.TransitionWithRefillLock(context.Background(), id, state.FUSEPoolPreparing, state.FUSEPoolReserved, "reservation", "controller", bound.Revision, 2*time.Second)
	assert.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)
	before, err := repo.ServerTime(context.Background())
	require.NoError(t, err)
	reserved, err := repo.TransitionWithRefillLock(context.Background(), id, state.FUSEPoolPreparing, state.FUSEPoolReserved, "reservation", "controller-new", bound.Revision, 2*time.Second)
	require.NoError(t, err)
	after, err := repo.ServerTime(context.Background())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, reserved.ReservedUntil.UnixMilli(), before.Add(2*time.Second).UnixMilli())
	assert.LessOrEqual(t, reserved.ReservedUntil.UnixMilli(), after.Add(2*time.Second).UnixMilli())
}

func TestFUSEPoolRenewRefillLockVerifiesOwnership(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey := poolTestID("pool")
	cleanupFUSEPool(t, s, []string{poolKey}, nil)
	require.True(t, mustRefillLock(t, repo, poolKey, "controller-a", 100*time.Millisecond))
	ok, err := repo.RenewRefillLock(context.Background(), poolKey, "controller-b", time.Second)
	require.NoError(t, err)
	assert.False(t, ok)
	ok, err = repo.RenewRefillLock(context.Background(), poolKey, "controller-a", time.Second)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestFUSEPoolPreparationIDValidationDoesNotCreateRawRedisKey(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey := poolTestID("pool")
	cleanupFUSEPool(t, s, []string{poolKey}, nil)
	require.True(t, mustRefillLock(t, repo, poolKey, "controller", time.Second))
	for _, invalid := range []string{"", string([]byte{0xff}), strings.Repeat("x", 1025)} {
		err := repo.CreatePreparingWithAdmission(context.Background(), preparingIntent(poolKey, invalid), "controller", 1, time.Second)
		assert.ErrorIs(t, err, state.ErrFUSEPoolInvalidRecord)
	}
	keys, err := s.client.Keys(context.Background(), "fusepool:record:*x*").Result()
	require.NoError(t, err)
	assert.Empty(t, keys)
}

func mustRefillLock(t *testing.T, repo *FUSEPoolRepository, poolKey, token string, ttl time.Duration) bool {
	t.Helper()
	locked, err := repo.TryRefillLock(context.Background(), poolKey, token, ttl)
	require.NoError(t, err)
	return locked
}

func prepareWarmRecord(t *testing.T, repo *FUSEPoolRepository, record state.FUSEPoolRecord) state.FUSEPoolRecord {
	t.Helper()
	require.NoError(t, repo.createPreparingForTest(context.Background(), record))
	got, err := repo.Transition(context.Background(), record.RuntimeUID, state.FUSEPoolPreparing, state.FUSEPoolPrepared, record.MaintainerToken, record.Revision)
	require.NoError(t, err)
	return *got
}

func cleanupRecordForTest(ctx context.Context, repo *FUSEPoolRepository, record state.FUSEPoolRecord, token string) (bool, error) {
	preparationID := record.PreparationID
	if preparationID == "" {
		preparationID = record.RuntimeUID
	}
	claimed, err := repo.ClaimCleanup(ctx, preparationID, record.State, record.MaintainerToken, record.ReservationToken, record.Revision, record.RuntimeID, record.RuntimeUID, token, time.Minute)
	if err != nil {
		return false, err
	}
	return repo.DeleteCleanup(ctx, preparationID, token, claimed.Revision)
}

func waitForRedisDeadline(t *testing.T, s *Store, deadline time.Time) {
	t.Helper()
	require.Eventually(t, func() bool {
		now, err := s.client.Time(context.Background()).Result()
		return err == nil && !now.Before(deadline)
	}, time.Second, 5*time.Millisecond)
}

type fusePoolReservationSnapshot struct {
	raw                  string
	deadline             string
	totalMember          bool
	totalCount           string
	membershipGeneration string
	stateMembers         map[state.FUSEPoolState]bool
	stateCounts          map[state.FUSEPoolState]string
}

func reservationSnapshot(t *testing.T, s *Store, poolKey, runtimeUID string) fusePoolReservationSnapshot {
	t.Helper()
	ctx := context.Background()
	poolDigest, uidDigest := poolTestDigest(poolKey), poolTestDigest(runtimeUID)
	hashValue := func(key, field string) string {
		value, err := s.client.HGet(ctx, key, field).Result()
		if errors.Is(err, redisclient.Nil) {
			return ""
		}
		require.NoError(t, err)
		return value
	}
	raw, err := s.client.Get(ctx, "fusepool:record:"+uidDigest).Result()
	require.NoError(t, err)
	totalMember, err := s.client.SIsMember(ctx, "fusepool:index:"+poolDigest, uidDigest).Result()
	require.NoError(t, err)
	snapshot := fusePoolReservationSnapshot{
		raw:                  raw,
		deadline:             hashValue("fusepool:reservation-deadlines", uidDigest),
		totalMember:          totalMember,
		totalCount:           hashValue("fusepool:pool-counts", poolDigest),
		membershipGeneration: hashValue("fusepool:membership-generations", poolDigest),
		stateMembers:         make(map[state.FUSEPoolState]bool, len(fusePoolStates)),
		stateCounts:          make(map[state.FUSEPoolState]string, len(fusePoolStates)),
	}
	for _, poolState := range fusePoolStates {
		member, memberErr := s.client.SIsMember(ctx, "fusepool:state:"+string(poolState)+":"+poolDigest, uidDigest).Result()
		require.NoError(t, memberErr)
		snapshot.stateMembers[poolState] = member
		snapshot.stateCounts[poolState] = hashValue("fusepool:state-counts:"+string(poolState), poolDigest)
	}
	return snapshot
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

func TestFUSEPoolReserveRejectsInvalidUTF8WithoutMutation(t *testing.T) {
	skipIfNoRedis(t)
	invalid := string([]byte{0xff, 0xfe})
	tests := []struct {
		name    string
		poolKey func(string) string
		token   string
	}{
		{name: "pool key", poolKey: func(string) string { return invalid }, token: "reservation"},
		{name: "reservation token", poolKey: func(poolKey string) string { return poolKey }, token: invalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testStore(t)
			repo := NewFUSEPoolRepository(s)
			poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
			cleanupFUSEPool(t, s, []string{poolKey, invalid}, []string{runtimeUID})
			prepareWarmRecord(t, repo, preparingRecord(poolKey, runtimeUID))
			before := reservationSnapshot(t, s, poolKey, runtimeUID)

			got, err := repo.ReservePrepared(context.Background(), tt.poolKey(poolKey), tt.token, time.Minute)
			assert.Nil(t, got)
			assert.ErrorIs(t, err, state.ErrFUSEPoolInvalidRecord)
			after := reservationSnapshot(t, s, poolKey, runtimeUID)
			assert.Equal(t, before, after)
			invalidPoolDigest := poolTestDigest(invalid)
			assert.Equal(t, int64(0), s.client.SCard(context.Background(), "fusepool:index:"+invalidPoolDigest).Val())
			assert.False(t, s.client.HExists(context.Background(), "fusepool:pool-counts", invalidPoolDigest).Val())
			assert.False(t, s.client.HExists(context.Background(), "fusepool:membership-generations", invalidPoolDigest).Val())
		})
	}
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
	require.NoError(t, repo.createPreparingForTest(context.Background(), record))
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
	require.NoError(t, repo.createPreparingForTest(context.Background(), record))
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
	require.NoError(t, repo.createPreparingForTest(context.Background(), record))
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
	require.NoError(t, repoA.createPreparingForTest(context.Background(), record))

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
	require.NoError(t, repo.createPreparingForTest(context.Background(), record))

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
	require.NoError(t, repo.createPreparingForTest(context.Background(), record))
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
	deleted, err := cleanupRecordForTest(context.Background(), repo, *consumed, "cleanup-state-index")
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
	require.NoError(t, repo.createPreparingForTest(context.Background(), cold))
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

func TestFUSEPoolCleanupClaimRejectsStaleRecord(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	prepared := prepareWarmRecord(t, repo, preparingRecord(poolKey, runtimeUID))

	stale := prepared
	stale.Revision--
	deleted, err := cleanupRecordForTest(context.Background(), repo, stale, "cleanup-stale")
	assert.False(t, deleted)
	assert.ErrorIs(t, err, state.ErrFUSEPoolCASMismatch)
	listed, err := repo.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	require.Len(t, listed, 1)

	deleted, err = cleanupRecordForTest(context.Background(), repo, prepared, "cleanup-current")
	require.NoError(t, err)
	assert.True(t, deleted)
	listed, err = repo.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	assert.Empty(t, listed)
}

func TestFUSEPoolCleanupClaimChecksEveryExpectedField(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, runtimeUID := poolTestID("pool"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{runtimeUID})
	prepareWarmRecord(t, repo, preparingRecord(poolKey, runtimeUID))
	reserved, err := repo.ReservePrepared(context.Background(), poolKey, "reservation", time.Minute)
	require.NoError(t, err)

	wrongState := *reserved
	wrongState.State = state.FUSEPoolPrepared
	deleted, err := cleanupRecordForTest(context.Background(), repo, wrongState, "cleanup-wrong-state")
	assert.False(t, deleted)
	assert.ErrorIs(t, err, state.ErrFUSEPoolConflict)
	wrongMaintainer := *reserved
	wrongMaintainer.MaintainerToken = "wrong-maintainer"
	deleted, err = cleanupRecordForTest(context.Background(), repo, wrongMaintainer, "cleanup-wrong-maintainer")
	assert.False(t, deleted)
	assert.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)
	wrongReservation := *reserved
	wrongReservation.ReservationToken = "wrong-reservation"
	deleted, err = cleanupRecordForTest(context.Background(), repo, wrongReservation, "cleanup-wrong-reservation")
	assert.False(t, deleted)
	assert.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)

	listed, err := repo.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, reserved.Revision, listed[0].Revision)
}

func TestFUSEPoolCleanupClaimRejectsCorruptPoolMetadata(t *testing.T) {
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

	deleted, err := cleanupRecordForTest(context.Background(), repo, prepared, "cleanup-corrupt")
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
	deleted, err := cleanupRecordForTest(context.Background(), repo, state.FUSEPoolRecord{PreparationID: runtimeUID, State: state.FUSEPoolPreparing, Revision: 1}, "cleanup-missing")
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
	require.NoError(t, repoA.createPreparingForTest(context.Background(), preparingRecord(poolKey, runtimeUIDs[0])))
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
		require.NoError(t, repo.createPreparingForTest(context.Background(), preparingRecord(poolKey, runtimeUID)))
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
		require.NoError(t, listRepo.createPreparingForTest(context.Background(), preparingRecord(poolKey, runtimeUID)))
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
		require.NoError(t, listRepo.createPreparingForTest(context.Background(), record))
	}
	victimUID := runtimeUIDs[0]
	replacementUID := poolTestID("replacement")
	runtimeUIDs = append(runtimeUIDs, replacementUID)
	cleanupFUSEPool(t, listStore, []string{poolKey}, runtimeUIDs)

	hook := &sscanActionHook{once: true}
	hook.action = func() error {
		victim := created[victimUID]
		deleted, deleteErr := cleanupRecordForTest(context.Background(), mutationRepo, victim, "cleanup-replace")
		if deleteErr != nil {
			return deleteErr
		}
		if !deleted {
			return fmt.Errorf("victim was not deleted")
		}
		return mutationRepo.createPreparingForTest(context.Background(), preparingRecord(poolKey, replacementUID))
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

func TestFUSEPoolMembershipGenerationOverflowPreservesCleanupTombstoneAndRejectsCreate(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	deletePool, createPool := poolTestID("delete-pool"), poolTestID("create-pool")
	deleteUID, createUID := poolTestID("delete-uid"), poolTestID("create-uid")
	cleanupFUSEPool(t, s, []string{deletePool, createPool}, []string{deleteUID, createUID})
	deleteRecord := preparingRecord(deletePool, deleteUID)
	require.NoError(t, repo.createPreparingForTest(context.Background(), deleteRecord))
	deleteDigest := poolTestDigest(deletePool)
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:membership-generations", deleteDigest, "9007199254740991").Err())
	claimed, err := repo.ClaimCleanup(context.Background(), deleteUID, deleteRecord.State, deleteRecord.MaintainerToken, "", deleteRecord.Revision, deleteRecord.RuntimeID, deleteRecord.RuntimeUID, "cleanup-overflow", time.Minute)
	require.NoError(t, err)
	rawBeforeDelete, err := s.client.Get(context.Background(), "fusepool:record:"+poolTestDigest(deleteUID)).Bytes()
	require.NoError(t, err)
	deleted, err := repo.DeleteCleanup(context.Background(), deleteUID, claimed.CleanupToken, claimed.Revision)
	assert.False(t, deleted)
	assert.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
	rawAfter, err := s.client.Get(context.Background(), "fusepool:record:"+poolTestDigest(deleteUID)).Bytes()
	require.NoError(t, err)
	assert.Equal(t, rawBeforeDelete, rawAfter, "failed physical deletion must preserve the retryable cleanup tombstone")

	createDigest := poolTestDigest(createPool)
	require.NoError(t, s.client.HSet(context.Background(), "fusepool:membership-generations", createDigest, "9007199254740991").Err())
	err = repo.createPreparingForTest(context.Background(), preparingRecord(createPool, createUID))
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
	err := repo.createPreparingForTest(context.Background(), record)
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

func TestFUSEPoolReturnPreparedAdmissionIsAtomicAtCapacity(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey, firstUID, secondUID := poolTestID("pool"), poolTestID("first"), poolTestID("second")
	cleanupFUSEPool(t, s, []string{poolKey}, []string{firstUID, secondUID})
	prepareWarmRecord(t, repo, preparingRecord(poolKey, firstUID))
	prepareWarmRecord(t, repo, preparingRecord(poolKey, secondUID))
	reserved, err := repo.ReservePrepared(context.Background(), poolKey, "return-capacity", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, reserved)

	returned, err := repo.ReturnPreparedWithAdmission(context.Background(), reserved.PreparationID, reserved.ReservationToken, reserved.Revision, 1)
	assert.Nil(t, returned)
	assert.ErrorIs(t, err, state.ErrFUSEPoolConflict)
	listed, err := repo.ListByPoolKey(context.Background(), poolKey)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	states := map[state.FUSEPoolState]int{}
	for _, record := range listed {
		states[record.State]++
	}
	assert.Equal(t, 1, states[state.FUSEPoolPrepared])
	assert.Equal(t, 1, states[state.FUSEPoolReserved], "capacity rejection must not partially publish the reservation")
}

func TestFUSEPoolDuplicateCreateLeavesNoHalfState(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolA, poolB, runtimeUID := poolTestID("pool-a"), poolTestID("pool-b"), poolTestID("uid")
	cleanupFUSEPool(t, s, []string{poolA, poolB}, []string{runtimeUID})
	require.NoError(t, repo.createPreparingForTest(context.Background(), preparingRecord(poolA, runtimeUID)))
	duplicate := preparingRecord(poolB, runtimeUID)
	err := repo.createPreparingForTest(context.Background(), duplicate)
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
	assert.ErrorIs(t, repo.createPreparingForTest(context.Background(), record), state.ErrFUSEPoolInvalidRecord)
	record.Revision = 1
	record.State = state.FUSEPoolPrepared
	assert.ErrorIs(t, repo.createPreparingForTest(context.Background(), record), state.ErrFUSEPoolInvalidRecord)
	record.State = state.FUSEPoolPreparing
	record.MaintainerToken = ""
	assert.ErrorIs(t, repo.createPreparingForTest(context.Background(), record), state.ErrFUSEPoolInvalidRecord)
	record.MaintainerToken = "maintainer-a"
	record.ReservationToken = "cold-token"
	record.ReservedUntil = time.Unix(0, 0).UTC()
	assert.ErrorIs(t, repo.createPreparingForTest(context.Background(), record), state.ErrFUSEPoolInvalidRecord)
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
			err := repo.createPreparingForTest(context.Background(), record)
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
			_, err = repo.ClaimCleanup(context.Background(), prepared.PreparationID, prepared.State, prepared.MaintainerToken, prepared.ReservationToken, prepared.Revision, prepared.RuntimeID, prepared.RuntimeUID, "cleanup-corrupt-record", time.Minute)
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
			deleted, err := cleanupRecordForTest(context.Background(), repo, state.FUSEPoolRecord{
				PreparationID: runtimeUID, RuntimeID: "runtime-" + runtimeUID, RuntimeUID: runtimeUID,
				State: state.FUSEPoolPrepared, MaintainerToken: "maintainer-a", Revision: 2,
			}, "cleanup-invalid-time")
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
	require.NoError(t, repo.createPreparingForTest(context.Background(), preparingRecord(poolKey, runtimeUID)))

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
	require.NoError(t, repo.createPreparingForTest(context.Background(), record))

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
