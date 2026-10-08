package etcd

import (
	"context"
	"errors"
	"strings"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func (o *taskArchiveDelivery) local(ctx context.Context, start bool) error {
	if ctx.Err() != nil || o.ctx.Err() != nil || o.claim.parentCtx.Err() != nil {
		return errors.Join(ErrGuardExpired, ctx.Err(), o.ctx.Err(), o.claim.parentCtx.Err())
	}
	if !time.Now().Before(*o.deadline.Load()) || !time.Now().Before(o.sessionAfter) {
		return ErrGuardExpired
	}
	c := o.claim
	c.mu.Lock()
	d := o.prepared.draft
	valid := c.archiveDelivery == o && o.backend.validPreparedTaskWorkspaceArchive(o.prepared) && !c.lost && !d.lost && !d.finalSync.lost
	if start {
		valid = valid && time.Now().Before(d.deadline) && time.Now().Before(d.windowDeadline)
	}
	c.mu.Unlock()
	if !valid {
		return ErrGuardExpired
	}
	return errors.Join(ctx.Err(), o.ctx.Err())
}

// Immutable comparisons were copied under the original claim mutex. This
// transaction checks them and the entire operations prefix at one fresh
// linearizable revision; neither branch reuses preparation's empty revision.
func (o *taskArchiveDelivery) currentGuard(ctx context.Context) error {
	if err := o.local(ctx, false); err != nil {
		return err
	}
	o.nativeMu.Lock()
	defer o.nativeMu.Unlock()
	if err := o.local(ctx, false); err != nil {
		return err
	}
	if err := o.nativeGuard(ctx); err != nil {
		return err
	}
	return o.local(ctx, false)
}

// nativeGuard has only immutable comparisons and native wire validation.
// Its callers serialize with nativeMu and supply their own lifetime fences.
func (o *taskArchiveDelivery) nativeGuard(ctx context.Context) error {
	b := o.backend
	op := taskProtoRange(o.prefix)
	op.GetRequestRange().RangeEnd = []byte(clientv3.GetPrefixRangeEnd(o.prefix))
	op.GetRequestRange().Limit = 1
	op.GetRequestRange().SortOrder = pb.RangeRequest_ASCEND
	op.GetRequestRange().SortTarget = pb.RangeRequest_KEY
	request := &pb.TxnRequest{Compare: taskProtoComparisons(o.comparisons), Success: []*pb.RequestOp{op}, Failure: []*pb.RequestOp{op}}
	reply := taskProtoResponse([]string{o.prefix + strings.Repeat("x", 256)}, []string{strings.Repeat("x", maxRecordBytes)})
	if err := taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{reply}); err != nil {
		return err
	}
	bounded, cancel := b.requestContext(ctx)
	response, err := b.client.Txn(bounded).If(o.comparisons...).Then(clientv3.OpGet(o.prefix, clientv3.WithPrefix(), clientv3.WithLimit(1), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))).Else(clientv3.OpGet(o.prefix, clientv3.WithPrefix(), clientv3.WithLimit(1), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))).Commit()
	requestErr := bounded.Err()
	cancel()
	if err != nil || requestErr != nil {
		return errors.Join(ErrOutcomeUnknown, err, requestErr)
	}
	if err = b.operationResponseHeader(response); err != nil {
		return err
	}
	if len(response.Responses) != 1 || response.Responses[0] == nil {
		return ErrOutcomeUnknown
	}
	r := response.Responses[0].GetResponseRange()
	if r == nil || !operationNestedHeader(r.Header, response.Header) {
		return ErrOutcomeUnknown
	}
	if !response.Succeeded || r.Count != 0 || len(r.Kvs) != 0 || r.More {
		return ErrConflict
	}
	return nil
}

func (o *taskArchiveDelivery) startGuard(ctx context.Context) error {
	if err := o.local(ctx, true); err != nil {
		return err
	}
	if err := o.currentGuard(ctx); err != nil {
		return err
	}
	now, err := o.backend.observePublicationClock(ctx)
	if err != nil {
		return err
	}
	if err = o.local(ctx, true); err != nil {
		return err
	}
	d := o.prepared.draft
	ticket, err := o.backend.taskWorkspaceArchiveVerifier.VerifyTaskWorkspaceArchiveTicket(d.ticket.Wire(), d.certificate, d.claims.Context, now)
	if err != nil {
		return err
	}
	if ticket.Digest() != d.ticket.Digest() || !ticket.NotBefore().Equal(d.claims.NotBefore) || !ticket.NotAfter().Equal(d.claims.NotAfter) || !ticket.SessionNotAfter().Equal(o.sessionAfter) {
		return ErrInvalidRecord
	}
	// The finite trusted clock callback cannot carry old native evidence across
	// a delay. This final guard is immediately before Open or Send.
	if err = o.currentGuard(ctx); err != nil {
		return err
	}
	return o.local(ctx, true)
}
