package etcd

import (
	"context"
	"time"
)

// One original share retains its physical call until native return and all
// validation/cleanup finish. Cancellation alone cannot free this owner.
type workerDueOwner struct {
	call      *workerCall
	partition *WorkerPartitionClaim
	share     *WorkerBudgetShare
	done      chan struct{}
}

func (b *Backend) beginDuePage(ctx context.Context, p *WorkerPartitionClaim, share *WorkerBudgetShare, cursor *DueCursor) (*workerDueOwner, error) {
	if ctx == nil || !b.validWorkerBackend() {
		return nil, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if !b.validWorkerPartition(p) || !b.validWorkerBudget(share) || p.session != share.session || share.kind != WorkerBudgetRange {
		return nil, ErrInvalidRecord
	}
	if cursor != nil && (cursor.self != cursor || cursor.origin != b || cursor.partition != p || cursor.share != share || cursor.used || cursor.revision <= 0) {
		return nil, ErrInvalidRecord
	}
	if b.archiveLife.closed {
		return nil, context.Canceled
	}
	if p.release != nil || p.released || p.lost || share.release != nil || share.released || share.lost || p.session.release != nil {
		return nil, ErrGuardExpired
	}
	if err := p.session.live(); err != nil {
		return nil, err
	}
	if share.dueOwner != nil || p.session.operation != nil {
		return nil, ErrConflict
	}
	// No local admission failure below this point can strand a cursor after
	// consuming its credit; ownership and one-use transition are atomic.
	if !share.bucket.take(time.Now(), maxMutationBytes) {
		return nil, ErrConflict
	}
	call, err := b.beginWorkerCallLocked(ctx, p.session)
	if err != nil {
		return nil, err
	}
	o := &workerDueOwner{call: call, partition: p, share: share, done: make(chan struct{})}
	share.dueOwner = o
	if cursor != nil {
		cursor.used = true
	}
	return o, nil
}

func (o *workerDueOwner) finish() {
	o.call.finish()
	b := o.share.origin
	b.archiveLife.mu.Lock()
	if o.share.dueOwner == o {
		o.share.dueOwner = nil
	}
	close(o.done)
	b.archiveLife.mu.Unlock()
}

func (o *workerDueOwner) postcheck() error {
	b := o.share.origin
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if err := o.call.ctx.Err(); err != nil {
		return err
	}
	if b.archiveLife.closed || o.call.session.release != nil || o.partition.release != nil || o.share.release != nil || o.partition.lost || o.partition.released || o.share.lost || o.share.released {
		return ErrGuardExpired
	}
	return o.call.session.live()
}

// Requires archiveLife.mu; joins happen outside this single lifecycle lock.
func (b *Backend) stopWorkerDueOwners(s *WorkerSession, p *WorkerPartitionClaim, share *WorkerBudgetShare) []*workerDueOwner {
	var owners []*workerDueOwner
	for _, candidate := range s.budgets {
		if candidate == nil || candidate.dueOwner == nil {
			continue
		}
		o := candidate.dueOwner
		if (p == nil || o.partition == p) && (share == nil || o.share == share) {
			o.call.cancel()
			owners = append(owners, o)
		}
	}
	return owners
}
