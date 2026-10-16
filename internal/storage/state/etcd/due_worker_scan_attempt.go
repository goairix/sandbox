package etcd

import (
	"context"
	"errors"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This private binding is installed only by the finite original consumer.
// There is no exported callback, caller-supplied comparison or Stage carrier.
type dueWorkerAdmission struct {
	origin     *Backend
	partition  *WorkerPartitionClaim
	claims     *WorkerBudgetShare
	checkpoint *dueCheckpoint
	early      bool
	rejected   bool
}

func (pending *dueWorkerAdmission) persist(call *workerCall, candidate DueCandidateRecord, result ExpiredTaskResult) error {
	if pending == nil || pending.origin == nil || pending.partition == nil || pending.claims == nil || pending.checkpoint == nil || pending.checkpoint.record.Pending != nil {
		return ErrInvalidRecord
	}
	next := pending.checkpoint.record
	next.Pending = &duePendingRecord{Candidate: candidate, Reference: result.Reference, Task: result.Task}
	saved, err := pending.origin.writeDueCheckpoint(call, pending.partition, pending.claims, pending.checkpoint, next)
	if err != nil {
		return err
	}
	pending.checkpoint = saved
	return nil
}

// Resolving a reference creates at most its original receipt, never a new
// admission. It holds the same original share's physical due owner without
// charging a second admission token, and inherits the immutable lease deadline.
func (b *Backend) beginDueResolution(ctx context.Context, p *WorkerPartitionClaim, claims *WorkerBudgetShare) (*workerDueOwner, context.CancelFunc, error) {
	if ctx == nil || !b.validWorkerBackend() {
		return nil, nil, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if !b.validWorkerPartition(p) || !b.validWorkerBudget(claims) || p.session != claims.session || claims.kind != WorkerBudgetClaim {
		return nil, nil, ErrInvalidRecord
	}
	if p.release != nil || p.released || p.lost || claims.release != nil || claims.released || claims.lost || claims.dueOwner != nil {
		return nil, nil, ErrGuardExpired
	}
	bounded, stop := context.WithDeadline(ctx, p.session.deadline)
	call, err := b.beginWorkerCallLocked(bounded, p.session)
	if err != nil {
		stop()
		return nil, nil, err
	}
	owner := &workerDueOwner{call: call, partition: p, share: claims, done: make(chan struct{})}
	claims.dueOwner = owner
	return owner, stop, nil
}

func (b *Backend) resolveDuePending(ctx context.Context, p *WorkerPartitionClaim, claims *WorkerBudgetShare, saved *dueCheckpoint) (outcome Outcome, resultErr error) {
	outcome = OutcomeUnknown
	if p == nil || !b.validDueCheckpoint(saved, p) || saved.record.Pending == nil {
		return outcome, ErrInvalidRecord
	}
	o, stop, err := b.beginDueResolution(ctx, p, claims)
	if err != nil {
		return outcome, err
	}
	defer stop()
	defer o.finish()
	defer func() {
		if resultErr != nil && o.call.started {
			o.call.lose()
		}
	}()
	ref := saved.record.Pending.Reference
	_, receipt, err := b.stageKeys(ref)
	if err != nil {
		return outcome, err
	}
	aborted, err := encodeReceipt(ref, OutcomeAborted)
	if err != nil {
		return outcome, err
	}
	committed, err := encodeReceipt(ref, OutcomeCommitted)
	if err != nil {
		return outcome, err
	}
	keys := []string{b.identityKey, b.restoreKey, receipt}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(append(b.baseComparisons(), clientv3.Compare(clientv3.CreateRevision(receipt), "=", 0))), Success: []*pb.RequestOp{taskProtoPut(receipt, aborted, 0)}, Failure: taskProtoPoints(keys)}
	if err = taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{taskProtoPutResponse(1), taskProtoResponse(keys, []string{b.identityValue, b.restoreEpoch, committed}), taskProtoResponse(keys, []string{b.identityValue, b.restoreEpoch, aborted})}); err != nil {
		return outcome, err
	}
	if err = b.workerGuardCheck(o.call, p, claims); err != nil {
		return outcome, err
	}
	if err = o.call.ctx.Err(); err != nil {
		return outcome, err
	}
	o.call.started = true
	outcome, err = b.ResolveStage(o.call.ctx, ref)
	if err != nil || outcome == OutcomeUnknown {
		return OutcomeUnknown, errors.Join(ErrOutcomeUnknown, err)
	}
	if err = b.workerGuardCheck(o.call, p, claims); err != nil {
		return OutcomeUnknown, err
	}
	if err = o.postcheck(); err != nil {
		return OutcomeUnknown, err
	}
	return outcome, nil
}

func (scan *dueWorkerScan) save(ctx context.Context, next dueCheckpointRecord) error {
	call, err := scan.backend.beginWorkerCall(ctx, scan.partition.session)
	if err != nil {
		return err
	}
	defer call.finish()
	saved, err := scan.backend.writeDueCheckpoint(call, scan.partition, nil, scan.checkpoint, next)
	if err == nil {
		scan.checkpoint = saved
	}
	return err
}

func (scan *dueWorkerScan) resolvePending(ctx context.Context) (Outcome, error) {
	outcome, err := scan.backend.resolveDuePending(ctx, scan.partition, scan.claims, scan.checkpoint)
	if err != nil {
		return OutcomeUnknown, err
	}
	next := scan.checkpoint.record
	next.Pending = nil
	if err = scan.save(ctx, next); err != nil {
		return OutcomeUnknown, err
	}
	scan.pending = StageReference{}
	return outcome, nil
}

func (scan *dueWorkerScan) settleCandidate(ctx context.Context, candidate DueCandidateRecord, listCandidate bool) error {
	if listCandidate {
		return scan.settle(ctx, candidate)
	}
	// Watch work may arrive below the list frontier. Clearing the same pending
	// request must never imply that a missing portion of fixed R was enumerated.
	if scan.checkpoint.record.Pending != nil {
		next := scan.checkpoint.record
		next.Pending = nil
		if err := scan.save(ctx, next); err != nil {
			return err
		}
	}
	scan.pending = StageReference{}
	return nil
}

// One page is retained at a time. The resident actor supplies the global
// cache/accounting and timers; it must not keep 256 uncharged copies of pages.
func (scan *dueWorkerScan) settle(ctx context.Context, candidate DueCandidateRecord) error {
	key, err := scan.backend.namespace.dueCandidateKey(candidate)
	if err != nil {
		return err
	}
	next := scan.checkpoint.record
	next.Pending = nil
	if !scan.frontierBlocked {
		next.Frontier = key
	}
	if scan.index+1 == len(scan.page) && scan.cursor == nil {
		next.Phase = "watch"
	}
	if err = scan.save(ctx, next); err != nil {
		return err
	}
	scan.index++
	scan.pending = StageReference{}
	return nil
}
