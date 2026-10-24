package etcd

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	endpoint := os.Getenv("TEST_ETCD_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_ETCD_ENDPOINT is not set")
	}
	s, err := New(context.Background(), Options{
		Endpoints: []string{endpoint},
		Prefix:    "/sandbox-test/" + uuid.NewString() + "/",
		Timeout:   3 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func TestAtomicStoreAcrossClients(t *testing.T) {
	a := testStore(t)
	b, err := New(context.Background(), Options{
		Endpoints: []string{os.Getenv("TEST_ETCD_ENDPOINT")},
		Prefix:    a.prefix,
		Timeout:   3 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	ctx := context.Background()

	require.NoError(t, a.Set(ctx, "owner", []byte("first"), 0))
	got, err := b.Get(ctx, "owner")
	require.NoError(t, err)
	require.Equal(t, []byte("first"), got)

	created, err := b.SetNX(ctx, "owner", []byte("second"), 0)
	require.NoError(t, err)
	require.False(t, created)

	swapped, err := a.CompareAndSwap(ctx, "owner", []byte("wrong"), []byte("second"), 0)
	require.NoError(t, err)
	require.False(t, swapped)
	swapped, err = b.CompareAndSwap(ctx, "owner", []byte("first"), []byte("second"), 0)
	require.NoError(t, err)
	require.True(t, swapped)

	deleted, err := a.CompareAndDeleteIfAbsent(ctx, "owner", []byte("second"), "lease")
	require.NoError(t, err)
	require.True(t, deleted)
	absent, err := b.ConfirmAbsence(ctx, "owner")
	require.NoError(t, err)
	require.True(t, absent)

	created, err = a.SetNX(ctx, "lease", []byte("live"), 0)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, b.Set(ctx, "owner", []byte("second"), 0))
	deleted, err = a.CompareAndDeleteIfAbsent(ctx, "owner", []byte("second"), "lease")
	require.NoError(t, err)
	require.False(t, deleted)
	require.NoError(t, a.Delete(ctx, "lease"))
	deleted, err = b.CompareAndDelete(ctx, "owner", []byte("second"))
	require.NoError(t, err)
	require.True(t, deleted)
}

func TestLeaseExpiryFencesOldValue(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	created, err := s.SetNX(ctx, "lease", []byte("old"), time.Second)
	require.NoError(t, err)
	require.True(t, created)
	require.Eventually(t, func() bool {
		absent, err := s.ConfirmAbsence(ctx, "lease")
		return err == nil && absent
	}, 4*time.Second, 50*time.Millisecond)
	created, err = s.SetNX(ctx, "lease", []byte("new"), 0)
	require.NoError(t, err)
	require.True(t, created)
	swapped, err := s.CompareAndSwap(ctx, "lease", []byte("old"), []byte("stale"), 0)
	require.NoError(t, err)
	require.False(t, swapped)
	got, err := s.Get(ctx, "lease")
	require.NoError(t, err)
	require.Equal(t, []byte("new"), got)
}

func TestIncrementContentionAndOverflow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const workers = 8
	const perWorker = 20
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				if _, err := s.Increment(ctx, "counter"); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
	got, err := s.Get(ctx, "counter")
	require.NoError(t, err)
	require.Equal(t, []byte(fmt.Sprint(workers*perWorker)), got)

	require.NoError(t, s.Set(ctx, "max", []byte(fmt.Sprint(int64(math.MaxInt64))), 0))
	_, err = s.Increment(ctx, "max")
	require.ErrorIs(t, err, state.ErrIncrementOverflow)
}

func TestKeysRejectsEmbeddedGlob(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	require.NoError(t, s.Set(ctx, "scope:a", []byte("a"), 0))
	require.NoError(t, s.Set(ctx, "scope:b", []byte("b"), 0))
	keys, err := s.Keys(ctx, "scope:*")
	require.NoError(t, err)
	require.Equal(t, []string{"scope:a", "scope:b"}, keys)
	_, err = s.Keys(ctx, "scope:*:owner:*")
	require.Error(t, err)
	require.False(t, errors.Is(err, context.Canceled))
}

func TestConfirmAbsenceNeverConfirmsOnReadError(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	absent, err := s.ConfirmAbsence(ctx, "owner")
	require.Error(t, err)
	require.False(t, absent)
}

func TestGetDistinguishesEmptyValueFromMissing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	require.NoError(t, s.Set(ctx, "empty", []byte{}, 0))
	got, err := s.Get(ctx, "empty")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)
	missing, err := s.Get(ctx, "missing")
	require.NoError(t, err)
	require.Nil(t, missing)
}

// lostReplyKV commits against the real server, then discards the first reply.
// It verifies that an ambiguous write is surfaced to its caller rather than
// retried locally as a second effect.
type lostReplyKV struct {
	clientv3.KV
	drop atomic.Bool
}

func (kv *lostReplyKV) Txn(ctx context.Context) clientv3.Txn {
	return &lostReplyTxn{Txn: kv.KV.Txn(ctx), owner: kv}
}

type lostReplyTxn struct {
	clientv3.Txn
	owner *lostReplyKV
}

func (tx *lostReplyTxn) If(comparisons ...clientv3.Cmp) clientv3.Txn {
	tx.Txn.If(comparisons...)
	return tx
}

func (tx *lostReplyTxn) Then(operations ...clientv3.Op) clientv3.Txn {
	tx.Txn.Then(operations...)
	return tx
}

func (tx *lostReplyTxn) Commit() (*clientv3.TxnResponse, error) {
	response, err := tx.Txn.Commit()
	if err == nil && tx.owner.drop.CompareAndSwap(true, false) {
		return nil, errors.New("reply lost after commit")
	}
	return response, err
}

func TestIncrementLostReplyDoesNotRetry(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	realKV := s.client.KV
	fault := &lostReplyKV{KV: realKV}
	fault.drop.Store(true)
	s.client.KV = fault
	_, err := s.Increment(ctx, "counter")
	s.client.KV = realKV
	require.ErrorContains(t, err, "reply lost after commit")
	got, err := s.Get(ctx, "counter")
	require.NoError(t, err)
	require.Equal(t, []byte("1"), got)
}

func TestSetNXLostReplyLeavesQueryableResult(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	realKV := s.client.KV
	fault := &lostReplyKV{KV: realKV}
	fault.drop.Store(true)
	s.client.KV = fault
	created, err := s.SetNX(ctx, "claim", []byte("owner"), 0)
	s.client.KV = realKV
	require.ErrorContains(t, err, "reply lost after commit")
	require.False(t, created)
	got, err := s.Get(ctx, "claim")
	require.NoError(t, err)
	require.Equal(t, []byte("owner"), got)
}

type insertAfterFirstPageKV struct {
	clientv3.KV
	insertKey string
	once      sync.Once
}

func (kv *insertAfterFirstPageKV) Get(ctx context.Context, key string, options ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	response, err := kv.KV.Get(ctx, key, options...)
	if err == nil && len(response.Kvs) == 256 {
		kv.once.Do(func() {
			_, _ = kv.KV.Put(ctx, kv.insertKey, "new")
		})
	}
	return response, err
}

func TestKeysUsesOneSnapshotAcrossPages(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := range 300 {
		require.NoError(t, s.Set(ctx, fmt.Sprintf("scope:%03d", i), []byte("v"), 0))
	}
	realKV := s.client.KV
	s.client.KV = &insertAfterFirstPageKV{KV: realKV, insertKey: s.prefix + "scope:zzz"}
	keys, err := s.Keys(ctx, "scope:*")
	s.client.KV = realKV
	require.NoError(t, err)
	require.Len(t, keys, 300)
	for _, key := range keys {
		require.NotEqual(t, "scope:zzz", key)
	}
}

func TestSubsecondTTLExpiresWithoutEarlyDeletion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	require.NoError(t, s.Set(ctx, "short", []byte("v"), 500*time.Millisecond))
	got, err := s.Get(ctx, "short")
	require.NoError(t, err)
	require.Equal(t, []byte("v"), got)
	require.Eventually(t, func() bool {
		got, err := s.Get(ctx, "short")
		return err == nil && got == nil
	}, 4*time.Second, 50*time.Millisecond)
}

func TestIncrementPreservesExistingLease(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	require.NoError(t, s.Set(ctx, "expiring-counter", []byte("0"), time.Second))
	value, err := s.Increment(ctx, "expiring-counter")
	require.NoError(t, err)
	require.Equal(t, int64(1), value)
	require.Eventually(t, func() bool {
		got, err := s.Get(ctx, "expiring-counter")
		return err == nil && got == nil
	}, 4*time.Second, 50*time.Millisecond)
}
