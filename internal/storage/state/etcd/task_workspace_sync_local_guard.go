package etcd

import (
	"archive/tar"
	"context"
	"errors"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func (s *PreparedTaskWorkspaceSyncSource) originalLocalBody(body *taskWorkspaceSyncBody) bool {
	return body != nil && body.self == body && body.source == s && s.body == body && body.entry != nil && body.entry.self == body.entry && body.entry.origin == s && s.current == body.entry && body.entry.kind == tar.TypeReg && !s.broken
}

func (s *PreparedTaskWorkspaceSyncSource) bindLocalPut(ctx context.Context, body *taskWorkspaceSyncBody, target *taskWorkspaceSyncLocalTarget) error {
	if err := s.begin(ctx); err != nil {
		return err
	}
	defer s.end()
	if !s.originalLocalBody(body) || target == nil || target.self != target || target.plan.validate() != nil || target.plan.digest != s.plan.digest || target.plan.workspace != s.plan.workspace {
		return ErrInvalidRecord
	}
	if err := target.begin(ctx); err != nil {
		return err
	}
	defer target.ops.Done()
	if err := errors.Join(s.local(ctx), target.check(ctx)); err != nil {
		return err
	}
	body.mu.Lock()
	defer body.mu.Unlock()
	if body.closed || body.active || body.bytes != 0 || body.complete || body.localConsumed || body.ctx.Err() != nil || body.intent != nil || body.localTarget != nil {
		return ErrInvalidRecord
	}
	body.localTarget = target
	return nil
}

// Used only by the original successful reserve branch before ACK authority.
func (s *PreparedTaskWorkspaceSyncSource) workspaceSyncBoundLocal(ctx context.Context, body *taskWorkspaceSyncBody) error {
	target := body.localTarget
	if target == nil {
		return ErrInvalidRecord
	}
	if err := target.begin(ctx); err != nil {
		return err
	}
	defer target.ops.Done()
	return target.check(ctx)
}

func (s *PreparedTaskWorkspaceSyncSource) prepareLocalPut(ctx context.Context, body *taskWorkspaceSyncBody, target *taskWorkspaceSyncLocalTarget) (*taskWorkspaceSyncLocalPut, error) {
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	if !s.originalLocalBody(body) || target == nil || target.self != target {
		s.end()
		return nil, ErrInvalidRecord
	}
	body.mu.Lock()
	draft := body.intent
	if body.closed || body.active || body.bytes != 0 || body.complete || body.ctx.Err() != nil || body.localConsumed || body.localTarget != target || draft == nil || !draft.acknowledged || draft.revision <= 0 || draft.record.Version != 2 || draft.record.Local == nil || *draft.record.Local != target.pin || draft.record.PlanDigest != s.plan.digest || target.plan.digest != s.plan.digest || target.plan.workspace != s.plan.workspace {
		body.mu.Unlock()
		s.end()
		return nil, ErrInvalidRecord
	}
	body.localConsumed = true // never refunded, even if registration/gate fails
	body.active = true
	body.done = make(chan struct{})
	body.mu.Unlock()
	if err := target.begin(ctx); err != nil {
		body.mu.Lock()
		body.active = false
		close(body.done)
		body.mu.Unlock()
		s.end()
		return nil, err
	}
	owned, cancel := context.WithCancel(ctx)
	p := &taskWorkspaceSyncLocalPut{source: s, body: body, target: target, draft: draft, ctx: owned, cancel: cancel}
	p.self = p
	p.stops = []func() bool{context.AfterFunc(s.ctx, cancel), context.AfterFunc(body.ctx, cancel), context.AfterFunc(target.ctx, cancel)}
	if err := p.guard(); err != nil {
		return nil, errors.Join(err, p.Close())
	}
	return p, nil
}

func (p *taskWorkspaceSyncLocalPut) guard() error {
	if p == nil || p.self != p {
		return ErrInvalidRecord
	}
	p.mu.Lock()
	if p.sealed || p.guarding || p.ctx.Err() != nil || p.body.ctx.Err() != nil {
		p.mu.Unlock()
		return ErrGuardExpired
	}
	p.guards.Add(1)
	p.guarding = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.guarding = false
		p.mu.Unlock()
		p.guards.Done()
	}()
	s, d, target := p.source, p.draft, p.target
	if err := errors.Join(s.local(p.ctx), target.check(p.ctx)); err != nil {
		return err
	}
	o, b := s.archive.owner, s.archive.owner.backend
	o.nativeMu.Lock()
	nativeLocked := true
	defer func() {
		if nativeLocked {
			o.nativeMu.Unlock()
		}
	}()
	if err := s.local(p.ctx); err != nil {
		return err
	}
	cmps := append([]clientv3.Cmp(nil), o.comparisons...)
	cmps = append(cmps, clientv3.Compare(clientv3.Version(o.prefix), "=", 0).WithRange(clientv3.GetPrefixRangeEnd(o.prefix)))
	cmps = append(cmps, b.workspaceSyncBudget.comparisons()...)
	keys := []string{b.identityKey, b.restoreKey, b.workspaceSyncBudget.key, d.intentKey, d.slotKey}
	values := []string{b.identityValue, b.restoreEpoch, b.workspaceSyncBudget.value, string(make([]byte, b.workspaceSyncBudget.options.MaxIntentBytes)), string(make([]byte, 512))}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoPoints(keys[3:]), Failure: taskProtoPoints(keys[:3])}
	success, failure := taskProtoResponse(keys[3:], values[3:]), taskProtoResponse(keys[:3], values[:3])
	p.nodes, p.requestBytes, p.successBytes, p.failureBytes = len(request.Compare)+len(request.Success)+len(request.Failure), request.Size(), success.Size(), failure.Size()
	if err := taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{success, failure}); err != nil {
		return err
	}
	rpcCtx, cancel := b.requestContext(p.ctx)
	r, err := b.client.Txn(rpcCtx).If(cmps...).Then(workspaceSyncPointOps(keys[3:])...).Else(workspaceSyncPointOps(keys[:3])...).Commit()
	rpcErr := rpcCtx.Err()
	cancel()
	if err != nil || rpcErr != nil {
		return errors.Join(ErrOutcomeUnknown, err, rpcErr)
	}
	if err := b.operationResponseHeader(r); err != nil {
		return err
	}
	p.responseBytes = (*pb.TxnResponse)(r).Size()
	if err := taskCheckWire(b.namespace, nil, []*pb.TxnResponse{(*pb.TxnResponse)(r)}); err != nil {
		return err
	}
	if !r.Succeeded {
		points, err := workspaceSyncPointResponse(b, r, keys[:3])
		if err != nil {
			return err
		}
		if !b.workspaceSyncBudgetMatches(points[2]) {
			return ErrIdentityMismatch
		}
		return ErrConflict
	}
	points, err := workspaceSyncPointResponse(b, r, keys[3:])
	if err != nil {
		return err
	}
	intent, err := decodeWorkspaceSyncIntent(points[0], int(b.workspaceSyncBudget.options.MaxIntentBytes))
	if err != nil {
		return err
	}
	slot, err := decodeWorkspaceSyncSlot(points[1])
	if err != nil {
		return err
	}
	if string(points[0].Value) != d.value || string(points[1].Value) != d.slotValue || points[0].CreateRevision != d.revision || points[1].CreateRevision != d.revision || intent.Version != 2 || intent.Local == nil || *intent.Local != target.pin || intent.Reference != d.record.Reference || intent.BudgetRevision != b.workspaceSyncBudget.create || slot.IntentDigest != workspaceSyncIntentDigest(d.value) {
		return ErrCorruptRecord
	}
	o.nativeMu.Unlock() // fully validated native response; physical postguard is outside
	nativeLocked = false
	postErr := errors.Join(s.local(p.ctx), target.check(p.ctx))
	p.mu.Lock()
	probeComplete := p.probeComplete
	p.mu.Unlock()
	if probeComplete != nil {
		probeComplete()
	}
	return postErr
}
