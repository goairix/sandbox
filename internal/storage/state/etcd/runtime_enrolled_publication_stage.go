package etcd

import (
	"context"
	"errors"
	"fmt"
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
