package etcd

import (
	"context"
	"errors"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// Relist owners occupy both their original share and one of four backend
// entries until the callback and its bounded guard calls actually return.
type workerRelistOwner struct {
	session    *WorkerSession
	partition  *WorkerPartitionClaim
	share      *WorkerBudgetShare
	index      int
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	stopParent func() bool
	parentDone chan struct{}
}

// RunWorkerRelist runs bounded local rebuilding under the original session,
// partition and relist slot. Cancellation cannot pretend that work returned.
// Success is a publication gate for this original owner only; callers must not
// publish canceled/lost results into a replacement session's cache.
// work must return cooperatively and must not synchronously release its own
// share/partition/session or call Backend.Close, nor wait for a goroutine doing
// such a join: it must return before an external owner releases or closes it.
func (b *Backend) RunWorkerRelist(ctx context.Context, p *WorkerPartitionClaim, share *WorkerBudgetShare, work func(context.Context) error) error {
	if ctx == nil || work == nil || !b.validWorkerBackend() {
		return ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b.archiveLife.mu.Lock()
	if !b.validWorkerPartition(p) || !b.validWorkerBudget(share) || p.session != share.session || share.kind != WorkerBudgetRelist {
		b.archiveLife.mu.Unlock()
		return ErrInvalidRecord
	}
	s := p.session
	if b.archiveLife.closed {
		b.archiveLife.mu.Unlock()
		return context.Canceled
	}
	if s.release != nil || p.release != nil || share.release != nil || p.lost || p.released || share.lost || share.released {
		b.archiveLife.mu.Unlock()
		return ErrGuardExpired
	}
	if err := s.live(); err != nil {
		b.archiveLife.mu.Unlock()
		return err
	}
	if share.owner != nil {
		b.archiveLife.mu.Unlock()
		return ErrConflict
	}
	index := -1
	for i, owner := range b.workerControl.relists {
		if owner == nil {
			index = i
			break
		}
	}
	if index < 0 {
		b.archiveLife.mu.Unlock()
		return ErrConflict
	}
	owned, cancel := context.WithDeadline(ctx, s.deadline)
	parentDone := make(chan struct{})
	stop := context.AfterFunc(s.ctx, func() { cancel(); close(parentDone) })
	o := &workerRelistOwner{session: s, partition: p, share: share, index: index, ctx: owned, cancel: cancel, done: make(chan struct{}), stopParent: stop, parentDone: parentDone}
	b.workerControl.relists[index], share.owner = o, o
	b.archiveLife.mu.Unlock()
	defer o.finish()
	if err := b.workerRelistGuard(o); err != nil {
		return err
	}
	if err := o.ctx.Err(); err != nil {
		return err
	}
	err := work(o.ctx) // The real callback executes on this original owner.
	if err != nil {
		return err
	}
	if err = o.ctx.Err(); err != nil {
		return err
	}
	if err = b.workerRelistGuard(o); err != nil {
		return err
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if err = o.ctx.Err(); err != nil {
		return err
	}
	if b.archiveLife.closed || s.release != nil || p.release != nil || share.release != nil {
		return context.Canceled
	}
	return s.live()
}

func (o *workerRelistOwner) finish() {
	b := o.session.origin
	o.cancel()
	if !o.stopParent() {
		<-o.parentDone
	}
	b.archiveLife.mu.Lock()
	if o.share.owner == o {
		o.share.owner = nil
	}
	if b.workerControl.relists[o.index] == o {
		b.workerControl.relists[o.index] = nil
	}
	close(o.done)
	b.archiveLife.mu.Unlock()
}

func (b *Backend) workerRelistGuard(o *workerRelistOwner) error {
	call, err := b.beginWorkerCall(o.ctx, o.session)
	if err != nil {
		return err
	}
	defer call.finish()
	b.archiveLife.mu.Lock()
	cmps, err := b.workerComparisons(o.session, o.partition, o.share)
	b.archiveLife.mu.Unlock()
	if err != nil {
		return err
	}
	keys := []string{b.identityKey}
	r, err := b.workerTxn(call, cmps, []clientv3.Op{clientv3.OpGet(b.identityKey)}, taskProtoPoints(keys), taskProtoResponse(keys, []string{b.identityValue}))
	if err != nil {
		if call.started {
			call.lose()
		}
		return err
	}
	points, err := b.workerPoints(r, keys)
	if err != nil {
		call.lose()
		return errors.Join(ErrOutcomeUnknown, err)
	}
	if points[0] == nil || points[0].Lease != 0 || string(points[0].Value) != b.identityValue {
		call.lose()
		return ErrIdentityMismatch
	}
	// This guard RPC is already complete. A subsequent local callback
	// cancellation is not evidence that its known native result was unknown.
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if err = o.ctx.Err(); err != nil {
		return err
	}
	if b.archiveLife.closed || o.session.release != nil || o.partition.release != nil || o.share.release != nil {
		return context.Canceled
	}
	return o.session.live()
}

// Requires archiveLife.mu; only the actual returned owners are joined later.
func (b *Backend) stopWorkerRelists(s *WorkerSession, p *WorkerPartitionClaim, share *WorkerBudgetShare) []*workerRelistOwner {
	var owners []*workerRelistOwner
	for _, owner := range b.workerControl.relists {
		if owner != nil && owner.session == s && (p == nil || owner.partition == p) && (share == nil || owner.share == share) {
			owner.cancel()
			owners = append(owners, owner)
		}
	}
	return owners
}

func (b *Backend) finishWorkerChildRelease(r *workerRelease, err error) {
	b.archiveLife.mu.Lock()
	r.err = err
	close(r.done)
	b.archiveLife.mu.Unlock()
}
