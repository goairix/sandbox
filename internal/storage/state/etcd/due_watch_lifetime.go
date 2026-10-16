package etcd

import (
	"context"
	"errors"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// One original native-operation owner covers all guarded subscriptions. Sender
// and receiver children are joined before any post-guard or slot release.
type dueWatchOwner struct {
	call               *workerCall
	cache              *dueWorkerCache
	subscriptions      []dueWatchSubscription
	done               chan struct{}
	stop               context.CancelFunc
	stopping           bool // archiveLife.mu
	pre, post          [256]int64
	starts             [256]int64
	stream             pb.Watch_WatchClient
	streamCtx          context.Context
	streamCancel       context.CancelFunc
	sendDone, recvDone chan struct{}
	sendErr, recvErr   error
	created, goal      chan struct{}
	createdCount       int
	progress           bool
	codec              dueWatchCodec
}

func (b *Backend) beginDueWatch(ctx context.Context, cache *dueWorkerCache) (*dueWatchOwner, error) {
	if ctx == nil || !b.validWorkerBackend() || cache == nil || cache.origin != b {
		return nil, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if !cache.validLocked() {
		return nil, ErrInvalidRecord
	}
	s := cache.session
	if s.dueWatch != nil {
		return nil, ErrConflict
	}
	subscriptions := make([]dueWatchSubscription, 0, 256)
	for i := range cache.views {
		v := &cache.views[i]
		if v.sub.Partition == nil {
			continue
		}
		p, share := v.sub.Partition, v.sub.Range
		if !b.validWorkerPartition(p) || !b.validWorkerBudget(share) || p.session != s || share.session != s || share.kind != WorkerBudgetRange || p.release != nil || p.lost || p.released || share.release != nil || share.lost || share.released || share.dueOwner != nil || share.owner != nil {
			return nil, ErrGuardExpired
		}
		subscriptions = append(subscriptions, v.sub)
	}
	if len(subscriptions) == 0 {
		return nil, ErrInvalidRecord
	}
	budget := min(3*time.Second, time.Duration(s.ttlSeconds)*time.Second/3)
	deadline := time.Now().Add(budget)
	if s.deadline.Before(deadline) {
		deadline = s.deadline
	}
	owned, stop := context.WithDeadline(ctx, deadline)
	call, err := b.beginWorkerCallLocked(owned, s)
	if err != nil {
		stop()
		return nil, err
	}
	o := &dueWatchOwner{call: call, cache: cache, subscriptions: subscriptions, done: make(chan struct{}), stop: stop, created: make(chan struct{}), goal: make(chan struct{}, 1)}
	s.dueWatch = o
	cache.published = false
	for _, sub := range subscriptions {
		v := &cache.views[sub.Partition.partition]
		v.created = false
		v.synchronized = false
		o.starts[sub.Partition.partition] = sub.ReplayFrom
	}
	return o, nil
}

// Requires archiveLife.mu. Physical completion is joined by the caller outside
// the lock; cancellation never frees the original session native-operation slot.
func (b *Backend) stopDueWatchOwner(s *WorkerSession, p *WorkerPartitionClaim, share *WorkerBudgetShare) *dueWatchOwner {
	if s == nil || s.dueWatch == nil {
		return nil
	}
	o := s.dueWatch
	match := p == nil && share == nil
	for _, sub := range o.subscriptions {
		if (p == nil || sub.Partition == p) && (share == nil || sub.Range == share) {
			match = true
			break
		}
	}
	if !match {
		return nil
	}
	o.stopping = true
	o.call.cancel()
	return o
}

func (o *dueWatchOwner) finish() {
	if o.streamCancel != nil {
		o.streamCancel()
	}
	if o.sendDone != nil {
		<-o.sendDone
	}
	if o.recvDone != nil {
		<-o.recvDone
	}
	if o.stream != nil {
		// CloseSend never overlaps SendMsg: the sender has physically returned.
		_ = o.stream.CloseSend()
	}
	o.stop()
	o.call.finish()
	b := o.call.session.origin
	b.archiveLife.mu.Lock()
	if o.call.session.dueWatch == o {
		o.call.session.dueWatch = nil
	}
	close(o.done)
	b.archiveLife.mu.Unlock()
}

func (o *dueWatchOwner) locallyStoppedLocked() bool {
	b, s := o.call.session.origin, o.call.session
	if o.stopping || b.archiveLife.closed || s.release != nil {
		return true
	}
	for _, sub := range o.subscriptions {
		if sub.Partition.release != nil || sub.Range.release != nil {
			return true
		}
	}
	return false
}

// A group has at most11 distinct partition/share fences beyond the original
// common12 comparisons: <=56+one read+four failure reads =61 native nodes.
func (b *Backend) dueWatchGuard(o *dueWatchOwner, group []dueWatchSubscription) (int64, error) {
	if err := o.call.ctx.Err(); err != nil {
		return 0, err
	}
	b.archiveLife.mu.Lock()
	if o.locallyStoppedLocked() {
		b.archiveLife.mu.Unlock()
		return 0, context.Canceled
	}
	cmps, err := b.workerComparisons(o.call.session, nil, nil)
	var shares [20]*WorkerBudgetShare
	shareCount := 0
	for _, sub := range group {
		if err != nil {
			break
		}
		p, share := sub.Partition, sub.Range
		if !b.validWorkerPartition(p) || !b.validWorkerBudget(share) || p.session != o.call.session || share.session != o.call.session || p.release != nil || p.lost || p.released || share.release != nil || share.lost || share.released {
			err = ErrGuardExpired
			break
		}
		cmps = append(cmps, p.fence.comparisons()...)
		found := false
		for _, old := range shares[:shareCount] {
			if old == share {
				found = true
				break
			}
		}
		if !found {
			shares[shareCount] = share
			shareCount++
			cmps = append(cmps, share.fence.comparisons()...)
		}
	}
	b.archiveLife.mu.Unlock()
	if err != nil {
		return 0, err
	}
	if len(group) == 0 || len(group)+shareCount > 11 || len(cmps) > 56 {
		return 0, ErrInvalidMutation
	}
	keys := []string{b.identityKey}
	reads := taskProtoPoints(keys)
	expected := taskProtoResponse(keys, []string{b.identityValue})
	if err = b.preflightDueCheckpoint(o.call.session, cmps, reads, expected); err != nil {
		return 0, err
	}
	// Track THIS guard's dispatch, rather than treating a completed earlier
	// guard as an unknown RPC when a later local stop prevents dispatch.
	priorStarted := o.call.started
	o.call.started = false
	r, err := b.workerTxn(o.call, cmps, []clientv3.Op{clientv3.OpGet(b.identityKey)}, reads, expected)
	dispatched := o.call.started
	o.call.started = priorStarted || dispatched
	if err != nil {
		if dispatched {
			o.call.lose()
		}
		return 0, err
	}
	points, err := b.workerPoints(r, keys)
	if err != nil || dueCheckpointHeader(r) != nil || points[0] == nil || points[0].Lease != 0 || string(points[0].Value) != b.identityValue {
		o.call.lose()
		return 0, errors.Join(ErrOutcomeUnknown, err)
	}
	b.archiveLife.mu.Lock()
	stopped := o.locallyStoppedLocked()
	b.archiveLife.mu.Unlock()
	if stopped {
		return 0, context.Canceled
	}
	return r.Header.Revision, nil
}

func dueWatchGroupEnd(subs []dueWatchSubscription, start int) int {
	var shares [20]*WorkerBudgetShare
	count, end := 0, start
	for end < len(subs) {
		share := subs[end].Range
		found := false
		for _, old := range shares[:count] {
			if old == share {
				found = true
				break
			}
		}
		extra := 0
		if !found {
			extra = 1
		}
		if end-start+1+count+extra > 11 {
			break
		}
		if !found {
			shares[count] = share
			count++
		}
		end++
	}
	return end
}
