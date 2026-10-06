package etcd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// RenewOperation extends only the capability's original Lease, under its
// original context, monotone deadline, immutable envelopes and control fences.
// Business expiry does not prevent already admitted work from draining.
func (b *Backend) RenewOperation(ctx context.Context, c *OperationCapability) (err error) {
	if c == nil || c.origin != b || c.parentCtx == nil {
		return ErrInvalidRecord
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Every failed renewal irrevocably loses authority, including unknown RPCs.
	defer func() {
		if err != nil {
			c.lost = true
		}
	}()
	if err = c.admissionLive(); err != nil {
		return err
	}
	if ctx == nil {
		return ErrInvalidRecord
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	receipt, err := encodeOperationReceipt(operationReceipt{Version: 1, Reference: c.record.Reference, Outcome: OperationCommitted})
	if err != nil {
		return err
	}
	comparisons := b.baseComparisons()
	for _, f := range c.fences {
		comparisons = append(comparisons, clientv3.Compare(clientv3.ModRevision(f.Key), "=", f.ModRevision), clientv3.Compare(clientv3.LeaseValue(f.Key), "=", 0))
	}
	comparisons = append(comparisons, operationEnvelopeComparisons(c.guardKey, c.value, c.record.Reference.LeaseID, c.guardRevision)...)
	comparisons = append(comparisons, operationEnvelopeComparisons(c.tokenKey, c.value, c.record.Reference.LeaseID, c.admitRevision)...)
	comparisons = append(comparisons, operationEnvelopeComparisons(c.receiptKey, receipt, c.record.Reference.LeaseID, c.admitRevision)...)
	if c.record.Reference.Kind == OperationMutation {
		comparisons = append(comparisons, operationEnvelopeComparisons(c.mutationKey, c.value, c.record.Reference.LeaseID, c.admitRevision)...)
	}
	// No evidence is adopted from a reread. All comparisons are against the
	// already bounded private producer state (30 comparisons at most).
	request, cancel := b.requestContext(ctx)
	response, err := b.client.Txn(request).If(comparisons...).Commit()
	cancel()
	if err != nil {
		return fmt.Errorf("%w: fence operation renewal: %w", ErrOutcomeUnknown, err)
	}
	if err = b.operationResponseHeader(response); err != nil {
		return err
	}
	if len(response.Responses) != 0 {
		return ErrOutcomeUnknown
	}
	if !response.Succeeded {
		return ErrConflict
	}
	if err = c.admissionLive(); err != nil {
		return err
	}
	request, cancel = b.requestContext(ctx)
	sent := time.Now()
	renewed, err := b.client.KeepAliveOnce(request, clientv3.LeaseID(c.record.Reference.LeaseID))
	cancel()
	if err != nil {
		return fmt.Errorf("%w: renew original operation Lease: %w", ErrOutcomeUnknown, err)
	}
	if renewed == nil || renewed.ResponseHeader == nil || renewed.ClusterId != b.clusterID || renewed.Revision <= 0 || int64(renewed.ID) != c.record.Reference.LeaseID {
		return ErrOutcomeUnknown
	}
	if renewed.TTL <= 0 || renewed.TTL > operationLeaseTTL {
		return ErrGuardExpired
	}
	// Validate the OLD deadline after delivery; a late success cannot revive it.
	if err = c.admissionLive(); err != nil {
		return err
	}
	deadline := sent.Add(time.Duration(renewed.TTL) * time.Second)
	if !time.Now().Before(deadline) {
		return ErrGuardExpired
	}
	c.deadline = deadline
	return nil
}

// CancelOperationCapability fences the capability before the bounded Revoke.
// It can retry that original Revoke, even after loss. This cancels admission
// metadata only: successful End still needs trusted target terminal evidence.
func (b *Backend) CancelOperationCapability(ctx context.Context, c *OperationCapability) error {
	if c == nil || c.origin != b || c.parentCtx == nil {
		return ErrInvalidRecord
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lost = true
	if ctx == nil {
		return ErrInvalidRecord
	}
	request, cancel := b.requestContext(ctx)
	defer cancel()
	response, err := b.client.Revoke(request, clientv3.LeaseID(c.record.Reference.LeaseID))
	if errors.Is(err, rpctypes.ErrLeaseNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: cancel original operation Lease: %w", ErrOutcomeUnknown, err)
	}
	if response == nil || response.Header == nil || response.Header.ClusterId != b.clusterID || response.Header.Revision <= 0 {
		return ErrOutcomeUnknown
	}
	return nil
}
