package etcd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"math"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This capability is private to the enrolled publication driver. It is never a
// generic Stage and cannot pass through CommitStage's fourteen reserved nodes.
type enrolledNativeStage struct {
	reference                                   StageReference
	mutation                                    Mutation
	lease                                       clientv3.LeaseID
	guardKey, receiptKey, guardValue, committed string
	guardRevision                               int64
}

func enrolledResponseBound(response *clientv3.TxnResponse) bool {
	if response == nil {
		return false
	}
	native := pb.TxnResponse(*response)
	return native.Size() <= maxMutationBytes
}

// Compact Else deliberately contains no identity evidence. Its receipt is only
// shape-checked here; outcome arbitration always re-reads current base fences.
func (b *Backend) enrolledCompactReceipt(response *clientv3.TxnResponse, ref StageReference) (*mvccpb.KeyValue, error) {
	if err := b.operationResponseHeader(response); err != nil {
		return nil, err
	}
	if !enrolledResponseBound(response) || response.Succeeded || len(response.Responses) != 1 || response.Responses[0] == nil {
		return nil, ErrOutcomeUnknown
	}
	r := response.Responses[0].GetResponseRange()
	if r == nil || r.More || r.Count != int64(len(r.Kvs)) || len(r.Kvs) > 1 || !operationNestedHeader(r.Header, response.Header) {
		return nil, ErrOutcomeUnknown
	}
	if len(r.Kvs) == 0 {
		return nil, nil
	}
	kv := r.Kvs[0]
	if !enrolledImmutableKV(kv) || kv.ModRevision > response.Header.Revision {
		return nil, ErrCorruptReceipt
	}
	outcome, err := decodeReceipt(kv, ref)
	if err != nil {
		return nil, err
	}
	canonical, err := encodeReceipt(ref, outcome)
	if err != nil || !bytes.Equal(kv.Value, []byte(canonical)) {
		return nil, ErrCorruptReceipt
	}
	return kv, nil
}

func enrolledImmutableKV(kv *mvccpb.KeyValue) bool {
	return immutableDispatchKV(kv) && kv.Version == 1 && len(kv.Value) <= maxRecordBytes
}

func (b *Backend) commitEnrolledStage(ctx context.Context, stage *enrolledNativeStage) (Outcome, error) {
	if err := b.preflightEnrolledStage(stage.reference, stage.mutation, int64(stage.lease), stage.guardRevision); err != nil {
		return OutcomeUnknown, err
	}
	compares := append(b.baseComparisons(), stage.mutation.Comparisons...)
	compares = append(compares, clientv3.Compare(clientv3.Value(stage.guardKey), "=", stage.guardValue), clientv3.Compare(clientv3.LeaseValue(stage.guardKey), "=", int64(stage.lease)), clientv3.Compare(clientv3.CreateRevision(stage.guardKey), "=", stage.guardRevision), clientv3.Compare(clientv3.CreateRevision(stage.receiptKey), "=", 0))
	ops := make([]clientv3.Op, 0, len(stage.mutation.Writes)+1)
	for _, write := range stage.mutation.Writes {
		ops = append(ops, clientv3.OpPut(write.Key, string(write.Value)))
	}
	ops = append(ops, clientv3.OpPut(stage.receiptKey, stage.committed))
	bounded, cancel := b.requestContext(ctx)
	transaction := b.client.Txn(bounded).If(compares...).Then(ops...)
	if stage.reference.StageID == enrolledPublicationStage {
		transaction = transaction.Else(clientv3.OpGet(stage.receiptKey))
	}
	response, err := transaction.Commit()
	err = errors.Join(err, bounded.Err())
	cancel()
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("%w: enrolled commit: %w", ErrOutcomeUnknown, err)
	}
	if err := b.operationResponseHeader(response); err != nil {
		return OutcomeUnknown, errors.Join(ErrOutcomeUnknown, err)
	}
	if !enrolledResponseBound(response) {
		return OutcomeUnknown, ErrOutcomeUnknown
	}
	if response.Succeeded {
		if !operationPutResponses(response, len(stage.mutation.Writes)+1) {
			return OutcomeUnknown, ErrOutcomeUnknown
		}
		return OutcomeCommitted, nil
	}
	if stage.reference.StageID == enrolledPublicationStage {
		if _, err := b.enrolledCompactReceipt(response, stage.reference); err != nil {
			return OutcomeUnknown, err
		}
	} else if len(response.Responses) != 0 {
		return OutcomeUnknown, ErrOutcomeUnknown
	}
	return b.readRuntimeEnrolledAttempt(ctx, stage.reference)
}

func (b *Backend) ReadRuntimeEnrolledPublicationAttempt(ctx context.Context, ref StageReference) (Outcome, error) {
	o, err := b.registerEnrolledPublication(ctx)
	if err != nil {
		return OutcomeUnknown, err
	}
	defer o.finish()
	return b.readRuntimeEnrolledAttempt(o.context, ref)
}

func (b *Backend) readRuntimeEnrolledAttempt(ctx context.Context, ref StageReference) (Outcome, error) {
	if !supportedEnrolledPublicationStage(ref.StageID) {
		return OutcomeUnknown, ErrInvalidMutation
	}
	guard, receipt, err := b.stageKeys(ref)
	if err != nil {
		return OutcomeUnknown, err
	}
	keys := []string{b.identityKey, b.restoreKey, receipt, guard}
	ops := []clientv3.Op{clientv3.OpGet(keys[0]), clientv3.OpGet(keys[1]), clientv3.OpGet(keys[2]), clientv3.OpGet(keys[3])}
	bounded, cancel := b.requestContext(ctx)
	response, err := b.client.Txn(bounded).If(b.baseComparisons()...).Then(ops...).Else(ops...).Commit()
	err = errors.Join(err, bounded.Err())
	cancel()
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("%w: enrolled arbitration: %w", ErrOutcomeUnknown, err)
	}
	if !enrolledResponseBound(response) {
		return OutcomeUnknown, ErrOutcomeUnknown
	}
	points, err := b.stageEvidencePoints(response, keys)
	if err != nil {
		return OutcomeUnknown, err
	}
	if !response.Succeeded {
		return OutcomeUnknown, ErrIdentityMismatch
	}
	if points[2] != nil {
		if !enrolledImmutableKV(points[2]) {
			return OutcomeUnknown, ErrCorruptReceipt
		}
		outcome, err := decodeReceipt(points[2], ref)
		if err != nil {
			return OutcomeUnknown, err
		}
		canonical, err := encodeReceipt(ref, outcome)
		if err != nil || string(points[2].Value) != canonical {
			return OutcomeUnknown, ErrCorruptReceipt
		}
		return outcome, nil
	}
	if points[3] == nil {
		return OutcomeUnknown, nil
	}
	g := points[3]
	var value struct {
		StageReference
		LeaseID int64 `json:"lease_id"`
	}
	if g.Version != 1 || g.Lease <= 0 || g.CreateRevision != g.ModRevision || json.Unmarshal(g.Value, &value) != nil || value.StageReference != ref || value.LeaseID != g.Lease || string(g.Value) != enrolledGuard(ref, g.Lease) {
		return OutcomeUnknown, ErrCorruptRecord
	}
	return OutcomeUnknown, nil
}

func (b *Backend) revokeEnrolledStage(lease clientv3.LeaseID) error {
	ctx, cancel := b.requestContext(context.Background())
	defer cancel()
	reply, err := b.client.Revoke(ctx, lease)
	if errors.Is(err, rpctypes.ErrLeaseNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: enrolled guard cleanup: %w", ErrOutcomeUnknown, err)
	}
	if reply == nil || reply.Header == nil || reply.Header.ClusterId != b.clusterID || reply.Header.Revision <= 0 {
		return ErrOutcomeUnknown
	}
	return nil
}

func (b *Backend) beginEnrolledStage(ctx context.Context, ref StageReference, mutation Mutation) (*enrolledNativeStage, error, error) {
	if err := b.preflightEnrolledMaximum(ref, mutation); err != nil {
		return nil, nil, err
	}
	guardKey, receiptKey, err := b.stageKeys(ref)
	if err != nil {
		return nil, nil, err
	}
	committed, err := encodeReceipt(ref, OutcomeCommitted)
	if err != nil {
		return nil, nil, err
	}
	bounded, cancel := b.requestContext(ctx)
	reply, err := b.client.Grant(bounded, 30)
	err = errors.Join(err, bounded.Err())
	cancel()
	known := reply != nil && reply.ResponseHeader != nil && reply.ClusterId == b.clusterID && reply.Revision > 0 && reply.ID > 0
	fail := func(cause error) (*enrolledNativeStage, error, error) {
		if known {
			return nil, b.revokeEnrolledStage(reply.ID), cause
		}
		return nil, nil, cause
	}
	if err != nil {
		return fail(fmt.Errorf("%w: enrolled stage grant: %w", ErrOutcomeUnknown, err))
	}
	if !known || reply.TTL <= 0 || reply.TTL > 30 || reply.Error != "" || ctx.Err() != nil {
		return fail(errors.Join(ErrOutcomeUnknown, ctx.Err()))
	}
	if err := b.preflightEnrolledStage(ref, mutation, int64(reply.ID), math.MaxInt64); err != nil {
		return fail(err)
	}
	s := &enrolledNativeStage{reference: ref, mutation: mutation, lease: reply.ID, guardKey: guardKey, receiptKey: receiptKey, guardValue: enrolledGuard(ref, int64(reply.ID)), committed: committed}
	compares := append(b.baseComparisons(), clientv3.Compare(clientv3.CreateRevision(guardKey), "=", 0), clientv3.Compare(clientv3.CreateRevision(receiptKey), "=", 0))
	bounded, cancel = b.requestContext(ctx)
	response, err := b.client.Txn(bounded).If(compares...).Then(clientv3.OpPut(guardKey, s.guardValue, clientv3.WithLease(reply.ID))).Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey)).Commit()
	err = errors.Join(err, bounded.Err())
	cancel()
	if err != nil {
		return fail(fmt.Errorf("%w: enrolled guard creation: %w", ErrOutcomeUnknown, err))
	}
	if err := b.operationResponseHeader(response); err != nil {
		return fail(errors.Join(ErrOutcomeUnknown, err))
	}
	if !response.Succeeded {
		if _, err := b.stageEvidencePoints(response, []string{b.identityKey, b.restoreKey}); err != nil {
			return fail(err)
		}
		return fail(ErrConflict)
	}
	if !operationPutResponses(response, 1) {
		return fail(ErrOutcomeUnknown)
	}
	s.guardRevision = response.Header.Revision
	if err := b.preflightEnrolledStage(ref, mutation, int64(s.lease), s.guardRevision); err != nil {
		return fail(err)
	}
	return s, nil, nil
}
