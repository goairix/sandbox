//go:build linux && (amd64 || arm64)

package etcd

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestColdCreateAttemptNative(t *testing.T) {
	backends, _ := coldPreparationNativeBackends(t)
	b := backends[0]
	ctx := context.Background()
	coldNativeInitialize(t, b)
	_, claim := coldNativeClaim(t, b, "original-attempt")
	reserved, err := b.ReserveColdPreparation(ctx, claim, 7)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, reserved.Outcome)
	dispatch, err := b.DeclareRuntimeDispatch(ctx, claim, RuntimeDispatchInput{Kind: RuntimeDispatchCreate, Target: "cold-target", Payload: json.RawMessage(`{"pod":"fixed"}`)})
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, dispatch.Outcome)
	trace := &coldAttemptTrace{KV: b.client.KV}
	b.client.KV = trace
	result, err := b.BeginColdCreateAttempt(ctx, claim)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.NotNil(t, result.Permit)
	require.Error(t, backends[1].ConsumeColdCreatePermit(claim, result.Permit), "foreign Backend cannot consume original handle")
	copied := &ColdCreatePermit{self: result.Permit, origin: b, claim: claim}
	require.Error(t, b.ConsumeColdCreatePermit(claim, copied), "copied fields cannot create a handle")
	require.NoError(t, b.ConsumeColdCreatePermit(claim, result.Permit))
	require.Error(t, b.ConsumeColdCreatePermit(claim, result.Permit), "original handle can be consumed only once")
	again, err := b.BeginColdCreateAttempt(ctx, claim)
	require.NoError(t, err)
	require.True(t, again.Replay)
	require.Nil(t, again.Permit, "durable replay is inspect-only")
	uid := uuid.NewString()
	observed, err := b.RecordColdCreateObservation(ctx, claim, uid)
	require.NoError(t, err)
	require.Equal(t, uid, observed.PodUID)
	loaded, err := backends[1].LoadColdCreateObservation(ctx, claim.workspace, claim.reference.IntentID)
	require.NoError(t, err)
	require.Equal(t, observed, loaded)
	_, err = b.RecordColdCreateObservation(ctx, claim, uuid.NewString())
	require.ErrorIs(t, err, ErrIdempotencyConflict, "replacement cannot replace the original UID")
	require.Equal(t, int32(1), trace.nodes[58].Load())
	require.Equal(t, int32(1), trace.nodes[49].Load())

	for _, nodes := range []int32{58, 49} {
		t.Run(fmt.Sprint("lost-ack-", nodes), func(t *testing.T) {
			_, original := coldNativeClaim(t, b, fmt.Sprint("lost-attempt-", nodes))
			_, err := b.ReserveColdPreparation(ctx, original, 7)
			require.NoError(t, err)
			_, err = b.DeclareRuntimeDispatch(ctx, original, RuntimeDispatchInput{Kind: RuntimeDispatchCreate, Target: "cold-target", Payload: json.RawMessage(`{"pod":"fixed"}`)})
			require.NoError(t, err)
			trace.dropNodes.Store(nodes)
			fresh, err := b.BeginColdCreateAttempt(ctx, original)
			if nodes == 58 {
				require.ErrorIs(t, err, ErrOutcomeUnknown)
				require.Nil(t, fresh.Permit)
			} else {
				require.NoError(t, err)
				require.NotNil(t, fresh.Permit)
				_, err = b.RecordColdCreateObservation(ctx, original, uid)
				require.ErrorIs(t, err, ErrOutcomeUnknown)
			}
			require.NoError(t, b.ReleaseCreationClaim(ctx, original))
			if fresh.Permit != nil {
				require.Error(t, b.ConsumeColdCreatePermit(original, fresh.Permit))
			}
			reclaimed, err := backends[1].ClaimCreation(ctx, original.workspace, original.reference.IntentID, "recovery-worker", 30*time.Second)
			require.NoError(t, err)
			defer backends[1].ReleaseCreationClaim(ctx, reclaimed)
			replay, err := backends[1].BeginColdCreateAttempt(ctx, reclaimed)
			require.NoError(t, err)
			require.True(t, replay.Replay)
			require.Nil(t, replay.Permit)
			fixed, err := backends[1].RecordColdCreateObservation(ctx, reclaimed, uid)
			require.NoError(t, err)
			require.Equal(t, uid, fixed.PodUID)
			retained, err := backends[1].LoadColdPreparation(ctx, original.workspace, original.reference.IntentID)
			require.NoError(t, err)
			require.Equal(t, "reserved", retained.Phase)
		})
	}
}

type coldAttemptTrace struct {
	clientv3.KV
	dropNodes atomic.Int32
	nodes     [65]atomic.Int32
}

func TestColdCreateAttemptClosedBackendNative(t *testing.T) {
	backends, _ := coldPreparationNativeBackends(t)
	b := backends[0]
	ctx := context.Background()
	coldNativeInitialize(t, b)
	_, claim := coldNativeClaim(t, b, "closed-backend")
	reserved, err := b.ReserveColdPreparation(ctx, claim, 7)
	require.NoError(t, err)
	_, err = b.DeclareRuntimeDispatch(ctx, claim, RuntimeDispatchInput{Kind: RuntimeDispatchCreate, Target: "cold-target", Payload: json.RawMessage(`{"pod":"fixed"}`)})
	require.NoError(t, err)
	fresh, err := b.BeginColdCreateAttempt(ctx, claim)
	require.NoError(t, err)
	require.NotNil(t, fresh.Permit)
	require.NoError(t, b.Close())
	require.Error(t, b.ConsumeColdCreatePermit(claim, fresh.Permit), "closed original Backend cannot authorize POST")
	retained, err := backends[1].LoadColdPreparation(ctx, claim.workspace, claim.reference.IntentID)
	require.NoError(t, err)
	require.Equal(t, reserved.Record, retained, "Backend close never refunds either permanent slot")
}

type coldAttemptTransaction struct {
	clientv3.Txn
	trace                      *coldAttemptTrace
	compares, success, failure int
}

func (k *coldAttemptTrace) Txn(ctx context.Context) clientv3.Txn {
	return &coldAttemptTransaction{Txn: k.KV.Txn(ctx), trace: k}
}
func (x *coldAttemptTransaction) If(c ...clientv3.Cmp) clientv3.Txn {
	x.compares = len(c)
	x.Txn = x.Txn.If(c...)
	return x
}
func (x *coldAttemptTransaction) Then(ops ...clientv3.Op) clientv3.Txn {
	x.success = len(ops)
	x.Txn = x.Txn.Then(ops...)
	return x
}
func (x *coldAttemptTransaction) Else(ops ...clientv3.Op) clientv3.Txn {
	x.failure = len(ops)
	x.Txn = x.Txn.Else(ops...)
	return x
}
func (x *coldAttemptTransaction) Commit() (*clientv3.TxnResponse, error) {
	nodes := int32(x.compares + x.success + x.failure)
	if nodes >= 0 && nodes < 65 {
		x.trace.nodes[nodes].Add(1)
	}
	r, err := x.Txn.Commit()
	if err == nil && r.Succeeded && x.trace.dropNodes.CompareAndSwap(nodes, 0) {
		return nil, ErrOutcomeUnknown
	}
	return r, err
}
