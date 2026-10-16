package etcd

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// WorkerSession owns one original native Lease and at most 256 logical
// partitions. Only the original backend member can use it. Explicit renewal
// never reconstructs a lost session or starts a resident goroutine.
type WorkerSession struct {
	origin                  *Backend
	self                    *WorkerSession
	ctx                     context.Context
	cancel                  context.CancelFunc
	slot                    int
	reference               WorkerSessionReference
	guard                   taskFence
	ttlSeconds              int64
	deadline                time.Time
	lost, released, cleaned bool
	cleanupErr              error
	operation               *workerCall
	release                 *workerRelease
	partitions              [256]*WorkerPartitionClaim
	budgets                 [124]*WorkerBudgetShare
	dueCache                *dueWorkerCache
	dueWatch                *dueWatchOwner
}

func (s *WorkerSession) Reference() WorkerSessionReference {
	if s == nil || s.origin == nil || s.origin.archiveLife == nil {
		return WorkerSessionReference{}
	}
	s.origin.archiveLife.mu.Lock()
	defer s.origin.archiveLife.mu.Unlock()
	return s.reference
}

func (s *WorkerSession) record(lease int64) workerSessionRecord {
	r := s.reference
	return workerSessionRecord{Version: 1, Namespace: r.Namespace, RestoreEpoch: r.RestoreEpoch, IdentityDigest: r.IdentityDigest, ProfileID: r.ProfileID, WorkerID: r.WorkerID, SessionID: r.SessionID, LeaseID: lease}
}

func (b *Backend) OpenWorkerSession(ctx context.Context, workerID string) (result *WorkerSession, resultErr error) {
	if ctx == nil || !validDomainSegment(workerID) || !b.validWorkerBackend() {
		return nil, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.archiveLife.mu.Lock()
	if b.archiveLife.closed {
		b.archiveLife.mu.Unlock()
		return nil, context.Canceled
	}
	slot := -1
	for i, s := range b.workerControl.sessions {
		if s == nil {
			slot = i
			break
		}
	}
	if slot < 0 {
		b.archiveLife.mu.Unlock()
		return nil, ErrConflict
	}
	owned, cancel := context.WithCancel(ctx)
	s := &WorkerSession{origin: b, ctx: owned, cancel: cancel, slot: slot, reference: WorkerSessionReference{Namespace: b.namespace.Root(), RestoreEpoch: b.restoreEpoch, IdentityDigest: workerIdentityDigest(b.identityValue), ProfileID: workerProfileID, WorkerID: workerID, SessionID: uuid.NewString()}, ttlSeconds: int64(b.workerControl.options.SessionTTL / time.Second)}
	s.self = s
	key, err := b.namespace.workerSessionKey(s.reference.SessionID)
	if err != nil {
		cancel()
		b.archiveLife.mu.Unlock()
		return nil, err
	}
	s.guard.key = key
	callCtx, callCancel := context.WithCancel(owned)
	call := &workerCall{session: s, ctx: callCtx, cancel: callCancel, done: make(chan struct{})}
	s.operation = call
	b.workerControl.sessions[slot] = s // Includes initializing sessions before Grant.
	b.archiveLife.mu.Unlock()
	defer func() {
		if result == nil {
			b.archiveLife.mu.Lock()
			s.lost = true
			s.cancel()
			lease := s.reference.LeaseID
			b.archiveLife.mu.Unlock()
			var cleanupErr error
			if lease > 0 {
				cleanupErr = b.workerRevoke(context.Background(), lease)
			}
			b.archiveLife.mu.Lock()
			s.cleaned, s.cleanupErr = true, cleanupErr
			// A concurrent Release/Close already owns removal after this call.
			if s.release == nil && b.workerControl.sessions[slot] == s {
				b.workerControl.sessions[slot] = nil
			}
			b.archiveLife.mu.Unlock()
			resultErr = errors.Join(resultErr, cleanupErr)
		}
		call.finish()
	}()
	if err = b.preflightWorker(s); err != nil {
		return nil, err
	}
	if err = call.ctx.Err(); err != nil {
		return nil, err
	}
	rpc, stop := call.requestContext()
	if err = rpc.Err(); err != nil {
		stop()
		return nil, err
	}
	call.started = true
	sent := time.Now()
	grant, grantErr := b.client.Grant(rpc, s.ttlSeconds)
	rpcErr := rpc.Err()
	stop()
	known := grant != nil && grant.ResponseHeader != nil && grant.ClusterId == b.clusterID && grant.Revision > 0 && grant.ID > 0
	if known {
		b.archiveLife.mu.Lock()
		s.reference.LeaseID = int64(grant.ID)
		b.archiveLife.mu.Unlock()
	}
	if grantErr != nil || rpcErr != nil {
		return nil, errors.Join(ErrOutcomeUnknown, grantErr, rpcErr)
	}
	if !known || grant.Error != "" || grant.TTL < s.ttlSeconds || grant.TTL > 60 {
		return nil, ErrOutcomeUnknown
	}
	value, err := encodeWorkerSessionRecord(s.record(int64(grant.ID)))
	if err != nil {
		return nil, err
	}
	b.archiveLife.mu.Lock()
	s.ttlSeconds = grant.TTL
	s.deadline = sent.Add(time.Duration(grant.TTL) * time.Second)
	s.guard.value, s.guard.lease = value, int64(grant.ID)
	err = s.live()
	cmps := append(b.baseComparisons(), b.workerControl.profile.comparisons()...)
	cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	b.archiveLife.mu.Unlock()
	if err != nil {
		return nil, err
	}
	r, err := b.workerTxn(call, cmps, []clientv3.Op{clientv3.OpPut(key, value, clientv3.WithLease(grant.ID))}, []*pb.RequestOp{taskProtoPut(key, value, int64(grant.ID))}, taskProtoPutResponse(1))
	if err != nil {
		return nil, err
	}
	if !operationPutResponses(r, 1) || r.Header.Revision <= grant.Revision {
		return nil, ErrOutcomeUnknown
	}
	b.archiveLife.mu.Lock()
	s.guard.create, s.guard.mod = r.Header.Revision, r.Header.Revision
	s.reference.CreateRevision = r.Header.Revision
	b.archiveLife.mu.Unlock()
	if err = b.workerGuardCheck(call, nil, nil); err != nil {
		return nil, err
	}
	if err = call.postcheck(); err != nil {
		return nil, err
	}
	return s, nil
}

func (b *Backend) RenewWorkerSession(ctx context.Context, s *WorkerSession) (resultErr error) {
	call, err := b.beginWorkerCall(ctx, s)
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil && call.started {
			call.lose()
		}
		call.finish()
	}()
	if err = b.workerGuardCheck(call, nil, nil); err != nil {
		return err
	}
	rpc, stop := call.requestContext()
	if err = rpc.Err(); err != nil {
		stop()
		return err
	}
	call.started = true
	sent := time.Now()
	renewed, renewErr := b.client.KeepAliveOnce(rpc, clientv3.LeaseID(s.reference.LeaseID))
	rpcErr := rpc.Err()
	stop()
	if renewErr != nil || rpcErr != nil {
		return errors.Join(ErrOutcomeUnknown, renewErr, rpcErr)
	}
	if renewed == nil || renewed.ResponseHeader == nil || renewed.ClusterId != b.clusterID || renewed.Revision <= 0 || int64(renewed.ID) != s.reference.LeaseID || renewed.TTL <= 0 || renewed.TTL > s.ttlSeconds {
		return ErrOutcomeUnknown
	}
	if err = call.postcheck(); err != nil {
		return err
	}
	if err = b.workerGuardCheck(call, nil, nil); err != nil {
		return err
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if err = s.live(); err != nil {
		return err
	}
	if err = call.ctx.Err(); err != nil {
		return err
	}
	next := sent.Add(time.Duration(renewed.TTL) * time.Second)
	if !time.Now().Before(next) {
		return ErrGuardExpired
	}
	s.deadline = next
	return nil
}

func (b *Backend) ReleaseWorkerSession(ctx context.Context, s *WorkerSession) error {
	if ctx == nil || !b.validWorkerBackend() {
		return ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b.archiveLife.mu.Lock()
	valid := b.validWorkerSession(s)
	if !valid && s != nil && s.origin == b && s.self == s && s.release != nil {
		valid = true
	}
	b.archiveLife.mu.Unlock()
	if !valid {
		return ErrInvalidRecord
	}
	return b.releaseWorkerSession(ctx, s)
}
