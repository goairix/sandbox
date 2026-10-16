package etcd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const duePageEntries = (maxMutationBytes - 4096) / (maxKeyBytes + dueRecordBytes + 128)

// DueCursor is a one-use continuation of the original worker's fixed view.
// Its diagnostic contents cannot be reconstructed or adopted by a new owner.
type DueCursor struct {
	origin     *Backend
	self       *DueCursor
	partition  *WorkerPartitionClaim
	share      *WorkerBudgetShare
	start, end string
	revision   int64
	windowEnd  time.Time
	used       bool
}

type DuePage struct {
	Candidates []DueCandidateRecord
	Revision   int64
	WindowEnd  time.Time
	Next       *DueCursor
}

// Discovery uses the original configured trusted observation. It does not
// advance business time by callback duration and never authorizes expiry.
func (b *Backend) observeDueWindow(ctx context.Context) (time.Time, error) {
	if b.publicationVerifier == nil || b.authorityClock == nil {
		return time.Time{}, ErrInvalidConfiguration
	}
	rpc, cancel := b.requestContext(ctx)
	defer cancel()
	started := time.Now()
	observation, err := b.authorityClock.Observe(rpc)
	elapsed := time.Since(started)
	if err != nil || rpc.Err() != nil {
		return time.Time{}, errors.Join(err, rpc.Err())
	}
	if observation.Uncertainty < 0 || observation.Uncertainty > time.Second || elapsed < 0 || elapsed > time.Second-observation.Uncertainty || !validDomainExpiry(observation.UTC) || observation.UTC.Location() != time.UTC {
		return time.Time{}, ErrInvalidRecord
	}
	maximum := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	if observation.UTC.After(maximum.Add(-60 * time.Second)) {
		return maximum, nil
	}
	return observation.UTC.Add(60 * time.Second), nil
}

func dueRangeOp(start, end string, revision int64) clientv3.Op {
	return clientv3.OpGet(start, clientv3.WithRange(end), clientv3.WithRev(revision), clientv3.WithLimit(duePageEntries), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
}

func dueProtoRange(start, end string, revision int64) *pb.RequestOp {
	return &pb.RequestOp{Request: &pb.RequestOp_RequestRange{RequestRange: &pb.RangeRequest{Key: []byte(start), RangeEnd: []byte(end), Revision: revision, Limit: duePageEntries, SortOrder: pb.RangeRequest_NONE, SortTarget: pb.RangeRequest_KEY}}}
}

// Validate maximal key/value/scalar/framing bytes before the real Range. A
// count-only limit cannot bound the native response's payload.
func (b *Backend) preflightDuePage(cmps []clientv3.Cmp, start, end string, revision int64, s *WorkerSession) error {
	if duePageEntries != 118 || duePageEntries > 256 {
		return ErrInvalidMutation
	}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: []*pb.RequestOp{dueProtoRange(start, end, revision)}, Failure: taskProtoPoints(b.workerFailureKeys(s))}
	header := taskProtoPutResponse(0).Header
	rangeReply := &pb.RangeResponse{Header: header, More: true, Count: math.MaxInt64}
	for i := 0; i < duePageEntries; i++ {
		rangeReply.Kvs = append(rangeReply.Kvs, &mvccpb.KeyValue{Key: bytes.Repeat([]byte("k"), maxKeyBytes), Value: bytes.Repeat([]byte("v"), dueRecordBytes), CreateRevision: math.MaxInt64, ModRevision: math.MaxInt64, Version: math.MaxInt64})
	}
	reply := &pb.TxnResponse{Header: header, Succeeded: true, Responses: []*pb.ResponseOp{{Response: &pb.ResponseOp_ResponseRange{ResponseRange: rangeReply}}}}
	return taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{reply, taskProtoResponse(b.workerFailureKeys(s), b.workerFailureValues(s))})
}

// ReadDuePage reads only overdue and the original next60s window. It charges
// the full original 256KiB chunk before any RPC and never refunds a result.
func (b *Backend) ReadDuePage(ctx context.Context, p *WorkerPartitionClaim, share *WorkerBudgetShare, cursor *DueCursor) (page DuePage, resultErr error) {
	o, err := b.beginDuePage(ctx, p, share, cursor)
	if err != nil {
		return page, err
	}
	defer o.finish()
	call := o.call
	knownCompaction := false
	defer func() {
		if resultErr != nil && call.started && !knownCompaction {
			call.lose()
		}
	}()
	var start, end string
	var revision int64
	var windowEnd time.Time
	if cursor == nil {
		windowEnd, err = b.observeDueWindow(call.ctx)
		if err != nil {
			return DuePage{}, err
		}
		prefix := fmt.Sprintf("%sp/%02x/due/", b.namespace.Root(), p.partition)
		start = prefix
		// '0' immediately follows '/' in byte order, so this upper bound
		// includes every canonical identity at the exact endpoint instant.
		end = prefix + windowEnd.Format(dueTimeLayout) + "0"
	} else {
		start, end, revision, windowEnd = cursor.start, cursor.end, cursor.revision, cursor.windowEnd
	}
	b.archiveLife.mu.Lock()
	cmps, err := b.workerComparisons(p.session, p, share)
	b.archiveLife.mu.Unlock()
	if err != nil {
		return DuePage{}, err
	}
	if err = b.preflightDuePage(cmps, start, end, revision, p.session); err != nil {
		return DuePage{}, err
	}
	if err = call.ctx.Err(); err != nil {
		return DuePage{}, err
	}
	rpc, cancel := call.requestContext()
	if err = rpc.Err(); err != nil {
		cancel()
		return DuePage{}, err
	}
	call.started = true
	response, err := b.client.Txn(rpc).If(cmps...).Then(dueRangeOp(start, end, revision)).Else(workspaceSyncPointOps(b.workerFailureKeys(p.session))...).Commit()
	rpcErr := rpc.Err()
	cancel()
	if err != nil || rpcErr != nil {
		if errors.Is(err, rpctypes.ErrCompacted) && rpcErr == nil {
			if guardErr := b.workerGuardCheck(call, p, share); guardErr == nil {
				knownCompaction = true
				return DuePage{}, rpctypes.ErrCompacted
			} else {
				return DuePage{}, errors.Join(err, guardErr)
			}
		}
		return DuePage{}, errors.Join(ErrOutcomeUnknown, err, rpcErr)
	}
	if err = b.operationResponseHeader(response); err != nil {
		return DuePage{}, errors.Join(ErrOutcomeUnknown, err)
	}
	if !enrolledResponseBound(response) {
		return DuePage{}, ErrCorruptRecord
	}
	if !response.Succeeded {
		return DuePage{}, b.workerFailedFences(p.session, response)
	}
	if revision == 0 {
		revision = response.Header.Revision
	}
	if response.Header.Revision < revision || len(response.Responses) != 1 || response.Responses[0] == nil {
		return DuePage{}, ErrCorruptRecord
	}
	r := response.Responses[0].GetResponseRange()
	if r == nil || !dueRangeHeader(r.Header, response.Header, revision) || len(r.Kvs) > duePageEntries || r.Count < int64(len(r.Kvs)) || !r.More && r.Count != int64(len(r.Kvs)) || r.More && (len(r.Kvs) != duePageEntries || r.Count <= int64(len(r.Kvs))) {
		return DuePage{}, ErrCorruptRecord
	}
	page = DuePage{Revision: revision, WindowEnd: windowEnd}
	var previous []byte
	for _, kv := range r.Kvs {
		if kv == nil || bytes.Compare(kv.Key, []byte(start)) < 0 || bytes.Compare(kv.Key, []byte(end)) >= 0 || previous != nil && bytes.Compare(kv.Key, previous) <= 0 {
			return DuePage{}, ErrCorruptRecord
		}
		candidate, err := decodeDueCandidate(b.namespace, kv, revision)
		if err != nil || candidate.Partition != p.partition || candidate.RestoreEpoch != b.restoreEpoch || candidate.ExpiresAt.After(windowEnd) {
			return DuePage{}, ErrCorruptRecord
		}
		page.Candidates = append(page.Candidates, candidate)
		previous = kv.Key
	}
	if err = o.postcheck(); err != nil {
		return DuePage{}, err
	}
	if r.More {
		next := &DueCursor{origin: b, partition: p, share: share, start: string(previous) + "\x00", end: end, revision: revision, windowEnd: windowEnd}
		next.self = next
		page.Next = next
	}
	return page, nil
}

// Historical etcd Range replies may report the requested revision or the
// current transaction revision. Accept only that bounded same-cluster view;
// every actual KV is separately checked against the requested fixed R.
func dueRangeHeader(nested, outer *pb.ResponseHeader, revision int64) bool {
	return nested != nil && (nested.ClusterId == 0 || nested.ClusterId == outer.ClusterId) && nested.Revision >= revision && nested.Revision <= outer.Revision && (nested.MemberId == 0 || nested.MemberId == outer.MemberId) && (nested.RaftTerm == 0 || nested.RaftTerm == outer.RaftTerm)
}
