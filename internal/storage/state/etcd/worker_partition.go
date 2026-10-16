package etcd

import (
	"context"
	"errors"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// WorkerPartitionClaim is the original immutable claim under one session
// Lease. A diagnostic copy/reference cannot reconstruct scheduling authority.
type WorkerPartitionClaim struct {
	origin         *Backend
	self           *WorkerPartitionClaim
	session        *WorkerSession
	partition      uint8
	fence          taskFence
	lost, released bool
	release        *workerRelease
}

func (p *WorkerPartitionClaim) Reference() WorkerPartitionReference {
	if p == nil || p.origin == nil || p.origin.archiveLife == nil || p.session == nil {
		return WorkerPartitionReference{}
	}
	p.origin.archiveLife.mu.Lock()
	defer p.origin.archiveLife.mu.Unlock()
	return WorkerPartitionReference{Session: p.session.reference, Partition: p.partition, CreateRevision: p.fence.create}
}

// Requires archiveLife.mu.
func (b *Backend) validWorkerPartition(p *WorkerPartitionClaim) bool {
	return p != nil && p.origin == b && p.self == p && b.validWorkerSession(p.session) && p.session.partitions[p.partition] == p && p.fence.create > 0
}

func (b *Backend) ClaimWorkerPartition(ctx context.Context, s *WorkerSession, partition uint8) (result *WorkerPartitionClaim, resultErr error) {
	call, err := b.beginWorkerCall(ctx, s)
	if err != nil {
		return nil, err
	}
	var reserved *WorkerPartitionClaim
	committed := false
	defer func() {
		if resultErr != nil && call.started && (committed || !errors.Is(resultErr, ErrConflict)) {
			call.lose()
		}
		if resultErr != nil && reserved != nil {
			b.archiveLife.mu.Lock()
			if !call.started || !committed && errors.Is(resultErr, ErrConflict) {
				s.partitions[partition] = nil
			} else {
				reserved.lost = true
			}
			b.archiveLife.mu.Unlock()
		}
		call.finish()
	}()
	b.archiveLife.mu.Lock()
	if previous := s.partitions[partition]; previous != nil {
		if previous.released || previous.lost || previous.release != nil {
			b.archiveLife.mu.Unlock()
			return nil, ErrConflict
		}
		b.archiveLife.mu.Unlock()
		return previous, nil
	}
	cmps, err := b.workerComparisons(s, nil, nil)
	if err != nil {
		b.archiveLife.mu.Unlock()
		return nil, err
	}
	key, err := b.namespace.workerPartitionKey(partition)
	if err != nil {
		b.archiveLife.mu.Unlock()
		return nil, err
	}
	value, err := encodeWorkerPartitionRecord(workerPartitionRecord{workerSessionRecord: s.record(s.reference.LeaseID), Partition: partition})
	if err != nil {
		b.archiveLife.mu.Unlock()
		return nil, err
	}
	p := &WorkerPartitionClaim{origin: b, session: s, partition: partition, fence: taskFence{key: key, value: value, lease: s.reference.LeaseID}}
	p.self = p
	s.partitions[partition], reserved = p, p
	cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	b.archiveLife.mu.Unlock()
	r, err := b.workerTxn(call, cmps, []clientv3.Op{clientv3.OpPut(key, value, clientv3.WithLease(clientv3.LeaseID(p.fence.lease)))}, []*pb.RequestOp{taskProtoPut(key, value, p.fence.lease)}, taskProtoPutResponse(1))
	if err != nil {
		return nil, err
	}
	if !operationPutResponses(r, 1) || r.Header.Revision <= s.reference.CreateRevision {
		return nil, ErrOutcomeUnknown
	}
	committed = true
	b.archiveLife.mu.Lock()
	p.fence.create, p.fence.mod = r.Header.Revision, r.Header.Revision
	b.archiveLife.mu.Unlock()
	if err = b.workerGuardCheck(call, p, nil); err != nil {
		return nil, err
	}
	return p, nil
}

func (b *Backend) ReleaseWorkerPartition(ctx context.Context, p *WorkerPartitionClaim) (resultErr error) {
	if ctx == nil || !b.validWorkerBackend() {
		return ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b.archiveLife.mu.Lock()
	if !b.validWorkerPartition(p) {
		b.archiveLife.mu.Unlock()
		return ErrInvalidRecord
	}
	if p.release != nil {
		r := p.release
		b.archiveLife.mu.Unlock()
		<-r.done
		return r.err
	}
	if b.archiveLife.closed || p.session.release != nil {
		b.archiveLife.mu.Unlock()
		return ErrGuardExpired
	}
	if p.released || p.lost {
		b.archiveLife.mu.Unlock()
		return ErrConflict
	}
	r := &workerRelease{done: make(chan struct{})}
	p.release = r
	owners := b.stopWorkerRelists(p.session, p, nil)
	dueOwners := b.stopWorkerDueOwners(p.session, p, nil)
	b.archiveLife.mu.Unlock()
	defer func() { b.finishWorkerChildRelease(r, resultErr) }()
	for _, owner := range owners {
		<-owner.done
	}
	for _, owner := range dueOwners {
		<-owner.done
	}
	call, err := b.beginWorkerCall(ctx, p.session)
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
	if p.released || p.lost {
		b.archiveLife.mu.Unlock()
		return ErrConflict
	}
	cmps, err := b.workerComparisons(p.session, nil, nil)
	if err != nil {
		b.archiveLife.mu.Unlock()
		return err
	}
	cmps = append(cmps, p.fence.comparisons()...)
	p.released = true // Tombstone retained for this original session.
	b.archiveLife.mu.Unlock()
	if err = b.workerDelete(call, cmps, p.fence.key); err != nil {
		return err
	}
	return b.workerGuardCheck(call, nil, nil)
}
