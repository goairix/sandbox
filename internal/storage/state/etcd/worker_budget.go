package etcd

import (
	"context"
	"errors"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// WorkerBudgetShare carries one original leased fixed slot. Its private local
// bucket is scheduling admission only, never task/runtime or native authority.
type WorkerBudgetShare struct {
	origin         *Backend
	self           *WorkerBudgetShare
	session        *WorkerSession
	kind           WorkerBudgetKind
	slot           uint16
	index          int
	fence          taskFence
	bucket         workerBucket
	lost, released bool
	release        *workerRelease
	owner          *workerRelistOwner
	dueOwner       *workerDueOwner
}

func (share *WorkerBudgetShare) Reference() WorkerBudgetReference {
	if share == nil || share.origin == nil || share.origin.archiveLife == nil || share.session == nil {
		return WorkerBudgetReference{}
	}
	share.origin.archiveLife.mu.Lock()
	defer share.origin.archiveLife.mu.Unlock()
	return WorkerBudgetReference{Session: share.session.reference, Kind: share.kind, Slot: share.slot, CreateRevision: share.fence.create}
}

// Requires archiveLife.mu.
func (b *Backend) validWorkerBudget(share *WorkerBudgetShare) bool {
	if share == nil || share.origin != b || share.self != share || !b.validWorkerSession(share.session) || share.index < 0 || share.index >= len(share.session.budgets) || share.session.budgets[share.index] != share || share.fence.create <= 0 {
		return false
	}
	limits, ok := workerBudgetSettings(share.kind)
	return ok && share.slot < limits.count && share.index == limits.offset+int(share.slot)
}

// AcquireWorkerBudget tries exactly the specified fixed slot once. Consumer
// allocation may choose a bounded candidate set; this API never scans/retries.
// Successful duplicate acquisition returns the original object and bucket.
func (b *Backend) AcquireWorkerBudget(ctx context.Context, s *WorkerSession, kind WorkerBudgetKind, slot uint16) (result *WorkerBudgetShare, resultErr error) {
	limits, ok := workerBudgetSettings(kind)
	if !ok || slot >= limits.count {
		return nil, ErrInvalidRecord
	}
	call, err := b.beginWorkerCall(ctx, s)
	if err != nil {
		return nil, err
	}
	index := limits.offset + int(slot)
	var reserved *WorkerBudgetShare
	committed := false
	defer func() {
		if resultErr != nil && call.started && (committed || !errors.Is(resultErr, ErrConflict)) {
			call.lose()
		}
		if resultErr != nil && reserved != nil {
			b.archiveLife.mu.Lock()
			if !call.started || !committed && errors.Is(resultErr, ErrConflict) {
				s.budgets[index] = nil
			} else {
				reserved.lost = true
			}
			b.archiveLife.mu.Unlock()
		}
		call.finish()
	}()
	b.archiveLife.mu.Lock()
	if old := s.budgets[index]; old != nil {
		if old.released || old.lost || old.release != nil {
			b.archiveLife.mu.Unlock()
			return nil, ErrConflict
		}
		b.archiveLife.mu.Unlock()
		return old, nil
	}
	cmps, err := b.workerComparisons(s, nil, nil)
	if err != nil {
		b.archiveLife.mu.Unlock()
		return nil, err
	}
	key, err := b.namespace.workerBudgetKey(kind, slot)
	if err != nil {
		b.archiveLife.mu.Unlock()
		return nil, err
	}
	value, err := encodeOperationMetadata(workerBudgetRecord{workerSessionRecord: s.record(s.reference.LeaseID), Kind: limits.name, Slot: slot}, workerRecordBytes)
	if err != nil {
		b.archiveLife.mu.Unlock()
		return nil, err
	}
	share := &WorkerBudgetShare{origin: b, session: s, kind: kind, slot: slot, index: index, fence: taskFence{key: key, value: value, lease: s.reference.LeaseID}}
	share.self = share
	s.budgets[index], reserved = share, share
	cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	b.archiveLife.mu.Unlock()
	r, err := b.workerTxn(call, cmps, []clientv3.Op{clientv3.OpPut(key, value, clientv3.WithLease(clientv3.LeaseID(share.fence.lease)))}, []*pb.RequestOp{taskProtoPut(key, value, share.fence.lease)}, taskProtoPutResponse(1))
	if err != nil {
		return nil, err
	}
	if !operationPutResponses(r, 1) || r.Header.Revision <= s.reference.CreateRevision {
		return nil, ErrOutcomeUnknown
	}
	committed = true
	b.archiveLife.mu.Lock()
	share.fence.create, share.fence.mod = r.Header.Revision, r.Header.Revision
	b.archiveLife.mu.Unlock()
	if err = b.workerGuardCheck(call, nil, share); err != nil {
		return nil, err
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if err = call.ctx.Err(); err != nil {
		return nil, err
	}
	if err = s.live(); err != nil {
		return nil, err
	}
	// Start earning only after the actual successful CAS and original
	// postguard, immediately before publication. Slow acquisition cannot
	// prefill a replacement bucket. Duplicate acquisition never touches it.
	share.bucket = newWorkerBucket(time.Now(), limits.rate, limits.capacity)
	return share, nil
}

// ConsumeWorkerBudget charges local earned credit without a network counter
// or refund. Future consumers must still hold an actual bounded RPC owner and
// use original session/partition/share comparisons in their real transaction.
func (b *Backend) ConsumeWorkerBudget(ctx context.Context, share *WorkerBudgetShare, amount uint32) error {
	if ctx == nil || !b.validWorkerBackend() {
		return ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if !b.validWorkerBudget(share) {
		return ErrInvalidRecord
	}
	limits, _ := workerBudgetSettings(share.kind)
	if share.kind == WorkerBudgetRelist || amount == 0 || uint64(amount) > limits.capacity {
		return ErrInvalidRecord
	}
	if b.archiveLife.closed {
		return context.Canceled
	}
	if share.lost || share.released || share.release != nil || share.session.release != nil {
		return ErrGuardExpired
	}
	if err := share.session.live(); err != nil {
		return err
	}
	if !share.bucket.take(time.Now(), amount) {
		return ErrConflict
	}
	return nil
}

// ReleaseWorkerBudget cancels and physically joins any original local relist
// before exact-CAS deletion. Its tombstone prevents same-session reborrowing.
func (b *Backend) ReleaseWorkerBudget(ctx context.Context, share *WorkerBudgetShare) (resultErr error) {
	if ctx == nil || !b.validWorkerBackend() {
		return ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b.archiveLife.mu.Lock()
	var worker *DueWorker
	var child *workerRelease
	if share != nil && share.origin == b && share.self == share && share.session != nil {
		worker = b.stopDueWorker(share.session, nil, share)
	}
	defer func() {
		if child != nil {
			b.finishDueWorkerChildRelease(child, resultErr, worker)
		} else if worker != nil {
			<-worker.done
		}
	}()
	if !b.validWorkerBudget(share) {
		b.archiveLife.mu.Unlock()
		return ErrInvalidRecord
	}
	if share.release != nil {
		r := share.release
		b.archiveLife.mu.Unlock()
		<-r.done
		return r.err
	}
	if b.archiveLife.closed || share.session.release != nil {
		b.archiveLife.mu.Unlock()
		return ErrGuardExpired
	}
	if share.lost || share.released {
		b.archiveLife.mu.Unlock()
		return ErrConflict
	}
	r := &workerRelease{done: make(chan struct{})}
	share.release = r
	child = r
	owners := b.stopWorkerRelists(share.session, nil, share)
	dueOwners := b.stopWorkerDueOwners(share.session, nil, share)
	watchOwner := b.stopDueWatchOwner(share.session, nil, share)
	b.archiveLife.mu.Unlock()
	if worker != nil {
		<-worker.workDone
	}
	for _, owner := range owners {
		<-owner.done
	}
	for _, owner := range dueOwners {
		<-owner.done
	}
	if watchOwner != nil {
		<-watchOwner.done
	}
	call, err := b.beginWorkerCall(ctx, share.session)
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil && call.started {
			call.lose()
		}
		call.finish()
	}()
	b.archiveLife.mu.Lock()
	cmps, err := b.workerComparisons(share.session, nil, nil)
	if err != nil {
		b.archiveLife.mu.Unlock()
		return err
	}
	cmps = append(cmps, share.fence.comparisons()...)
	share.released = true
	b.archiveLife.mu.Unlock()
	if err = b.workerDelete(call, cmps, share.fence.key); err != nil {
		return err
	}
	return b.workerGuardCheck(call, nil, nil)
}
