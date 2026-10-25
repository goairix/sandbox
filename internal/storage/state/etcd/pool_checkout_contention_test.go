package etcd

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/goairix/sandbox/internal/storage/state"
)

// poolAggregateBarrierKV lets two independent operations read the same pool
// aggregate revision before either conditional transaction commits. Only their
// first commits wait; a later retry can proceed against the new revision.
type poolAggregateBarrierKV struct {
	clientv3.KV
	arrivals atomic.Int32
	gate     chan struct{}
}

func (kv *poolAggregateBarrierKV) Txn(ctx context.Context) clientv3.Txn {
	return &poolAggregateBarrierTxn{Txn: kv.KV.Txn(ctx), owner: kv, ctx: ctx}
}

type poolAggregateBarrierTxn struct {
	clientv3.Txn
	owner       *poolAggregateBarrierKV
	ctx         context.Context
	conditional bool
}

// checkoutLostReplyKV simulates a committed transaction whose reply is lost.
// The caller must report uncertainty and must not create a new attempt.
type checkoutLostReplyKV struct {
	clientv3.KV
	commits atomic.Int32
}

func (kv *checkoutLostReplyKV) Txn(ctx context.Context) clientv3.Txn {
	return &checkoutLostReplyTxn{Txn: kv.KV.Txn(ctx), owner: kv}
}

type checkoutLostReplyTxn struct {
	clientv3.Txn
	owner       *checkoutLostReplyKV
	conditional bool
}

func (tx *checkoutLostReplyTxn) If(conditions ...clientv3.Cmp) clientv3.Txn {
	tx.conditional = true
	tx.Txn.If(conditions...)
	return tx
}

func (tx *checkoutLostReplyTxn) Then(operations ...clientv3.Op) clientv3.Txn {
	tx.Txn.Then(operations...)
	return tx
}

func (tx *checkoutLostReplyTxn) Else(operations ...clientv3.Op) clientv3.Txn {
	tx.Txn.Else(operations...)
	return tx
}

func (tx *checkoutLostReplyTxn) Commit() (*clientv3.TxnResponse, error) {
	result, err := tx.Txn.Commit()
	if err != nil {
		return result, err
	}
	if !tx.conditional {
		return result, nil
	}
	tx.owner.commits.Add(1)
	return nil, errors.New("reply lost after commit")
}

func (tx *poolAggregateBarrierTxn) If(conditions ...clientv3.Cmp) clientv3.Txn {
	tx.conditional = true
	tx.Txn.If(conditions...)
	return tx
}

func (tx *poolAggregateBarrierTxn) Then(operations ...clientv3.Op) clientv3.Txn {
	tx.Txn.Then(operations...)
	return tx
}

func (tx *poolAggregateBarrierTxn) Else(operations ...clientv3.Op) clientv3.Txn {
	tx.Txn.Else(operations...)
	return tx
}

func (tx *poolAggregateBarrierTxn) Commit() (*clientv3.TxnResponse, error) {
	if !tx.conditional {
		return tx.Txn.Commit()
	}
	arrival := tx.owner.arrivals.Add(1)
	if arrival == 2 {
		close(tx.owner.gate)
	}
	if arrival <= 2 {
		select {
		case <-tx.owner.gate:
		case <-tx.ctx.Done():
			return nil, tx.ctx.Err()
		}
	}
	return tx.Txn.Commit()
}

func TestPoolCheckoutRetriesOnlyIndependentAggregateContention(t *testing.T) {
	store := testStore(t)
	repository := NewFUSEPoolRepository(store)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", 10*time.Second)
	require.NoError(t, err)
	require.True(t, won)

	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("preparation-%d", i)
		require.NoError(t, repository.CreatePreparingWithAdmission(ctx,
			preparingPoolRecord(id), "holder", 2, 10*time.Second))
		_, err := repository.BindPreparingRuntime(ctx, id, id+"-runtime", id+"-uid", "holder", 1)
		require.NoError(t, err)
		_, err = repository.Transition(ctx, id, state.FUSEPoolPreparing,
			state.FUSEPoolPrepared, "maintainer", 2)
		require.NoError(t, err)
	}

	reserved := make([]*state.FUSEPoolRecord, 0, 2)
	for i := 0; i < 2; i++ {
		record, err := repository.ReservePrepared(ctx, "shared-pool",
			fmt.Sprintf("reservation-%d", i), time.Minute)
		require.NoError(t, err)
		reserved = append(reserved, record)
	}

	// The exact record revisions remain unchanged while another sandbox moves
	// the shared count. Both transitions must succeed, without weakening any
	// record, token, owner, or reservation-deadline comparison.
	realKV := store.client.KV
	barrier := &poolAggregateBarrierKV{KV: realKV, gate: make(chan struct{})}
	store.client.KV = barrier
	t.Cleanup(func() { store.client.KV = realKV })

	results := make(chan error, len(reserved))
	var workers sync.WaitGroup
	for _, record := range reserved {
		workers.Add(1)
		go func(record *state.FUSEPoolRecord) {
			defer workers.Done()
			_, transitionErr := repository.Transition(ctx, record.PreparationID,
				state.FUSEPoolReserved, state.FUSEPoolBinding,
				record.ReservationToken, record.Revision)
			results <- transitionErr
		}(record)
	}
	workers.Wait()
	close(results)
	for transitionErr := range results {
		require.NoError(t, transitionErr)
	}

	inventory, err := repository.ReadInventory(ctx, "shared-pool")
	require.NoError(t, err)
	require.Equal(t, int64(2), inventory.Binding)
}

func TestPoolCheckoutDoesNotRetryLostCommitReply(t *testing.T) {
	store := testStore(t)
	repository := NewFUSEPoolRepository(store)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", 10*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	require.NoError(t, repository.CreatePreparingWithAdmission(ctx,
		preparingPoolRecord("preparation"), "holder", 1, 10*time.Second))
	_, err = repository.BindPreparingRuntime(ctx, "preparation", "runtime", "uid", "holder", 1)
	require.NoError(t, err)
	_, err = repository.Transition(ctx, "preparation", state.FUSEPoolPreparing,
		state.FUSEPoolPrepared, "maintainer", 2)
	require.NoError(t, err)
	reserved, err := repository.ReservePrepared(ctx, "shared-pool", "reservation", time.Minute)
	require.NoError(t, err)

	realKV := store.client.KV
	lostReply := &checkoutLostReplyKV{KV: realKV}
	store.client.KV = lostReply
	t.Cleanup(func() { store.client.KV = realKV })
	_, err = repository.Transition(ctx, "preparation", state.FUSEPoolReserved,
		state.FUSEPoolBinding, "reservation", reserved.Revision)
	require.ErrorContains(t, err, "reply lost after commit")
	require.EqualValues(t, 1, lostReply.commits.Load())

	record, err := repository.ReadByPreparationID(ctx, "preparation")
	require.NoError(t, err)
	require.Equal(t, state.FUSEPoolBinding, record.State)
}
