package etcd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Only a current fenced read/write produces this original local handle. Its
// record is permanent diagnostic state; its pointer cannot cross partitions
// or sessions even when all copied fields happen to match.
type dueCheckpoint struct {
	origin    *Backend
	self      *dueCheckpoint
	partition *WorkerPartitionClaim
	record    dueCheckpointRecord
	fence     taskFence
}

func (b *Backend) ownDueCheckpoint(p *WorkerPartitionClaim, r dueCheckpointRecord, fence taskFence) *dueCheckpoint {
	if r.Pending != nil {
		pending := *r.Pending
		r.Pending = &pending
	}
	owned := &dueCheckpoint{origin: b, partition: p, record: r, fence: fence}
	owned.self = owned
	return owned
}

func (b *Backend) validDueCheckpoint(saved *dueCheckpoint, p *WorkerPartitionClaim) bool {
	return saved != nil && saved.origin == b && saved.self == saved && saved.partition == p && saved.record.Partition == p.partition && saved.fence.create > 0
}

func (b *Backend) preflightDueCheckpoint(s *WorkerSession, cmps []clientv3.Cmp, success []*pb.RequestOp, reply *pb.TxnResponse) error {
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: success, Failure: taskProtoPoints(b.workerFailureKeys(s))}
	failure := taskProtoResponse(b.workerFailureKeys(s), b.workerFailureValues(s))
	if request.Size() > dueCheckpointWireBytes || reply.Size() > dueCheckpointWireBytes || failure.Size() > dueCheckpointWireBytes {
		return ErrInvalidMutation
	}
	return taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{reply, failure})
}

func dueCheckpointHeader(r *clientv3.TxnResponse) error {
	if r == nil || r.Header == nil || r.Header.MemberId == 0 || r.Header.RaftTerm == 0 || len(r.Header.XXX_unrecognized) != 0 || len(r.XXX_unrecognized) != 0 || (*pb.TxnResponse)(r).Size() > dueCheckpointWireBytes {
		return ErrCorruptRecord
	}
	return nil
}

func dueCheckpointNestedHeader(nested, outer *pb.ResponseHeader) bool {
	return operationNestedHeader(nested, outer) && (nested == nil || len(nested.XXX_unrecognized) == 0 && (nested.MemberId == 0 || nested.MemberId == outer.MemberId) && (nested.RaftTerm == 0 || nested.RaftTerm == outer.RaftTerm))
}

func (b *Backend) readDueCheckpoint(ctx context.Context, p *WorkerPartitionClaim) (saved *dueCheckpoint, resultErr error) {
	if ctx == nil || !b.validWorkerBackend() || p == nil {
		return nil, ErrInvalidRecord
	}
	call, err := b.beginWorkerCall(ctx, p.session)
	if err != nil {
		return nil, err
	}
	defer call.finish()
	defer func() {
		if resultErr != nil && call.started {
			call.lose()
		}
	}()

	// The row is permanent evidence. Current original partition/session
	// fences are read-bearing guards, never authority adopted from its writer.
	b.archiveLife.mu.Lock()
	cmps, err := b.workerComparisons(p.session, p, nil)
	b.archiveLife.mu.Unlock()
	if err != nil {
		return nil, err
	}

	key, err := b.namespace.dueCheckpointKey(p.partition)
	if err != nil {
		return nil, err
	}
	expected := taskProtoResponse([]string{key}, []string{strings.Repeat("x", dueCheckpointBytes)})
	if err = b.preflightDueCheckpoint(p.session, cmps, taskProtoPoints([]string{key}), expected); err != nil {
		return nil, err
	}
	r, err := b.workerTxn(call, cmps, []clientv3.Op{clientv3.OpGet(key)}, taskProtoPoints([]string{key}), expected)
	if err != nil {
		return nil, err
	}
	if dueCheckpointHeader(r) != nil || len(r.Responses) != 1 || r.Responses[0] == nil || len(r.Responses[0].XXX_unrecognized) != 0 {
		return nil, ErrCorruptRecord
	}
	point := r.Responses[0].GetResponseRange()
	if point == nil || point.More || point.Count != int64(len(point.Kvs)) || len(point.Kvs) > 1 || len(point.XXX_unrecognized) != 0 || !dueCheckpointNestedHeader(point.Header, r.Header) {
		return nil, ErrCorruptRecord
	}
	if len(point.Kvs) == 1 {
		record, err := b.decodeDueCheckpoint(point.Kvs[0], r.Header.Revision)
		if err != nil || record.Partition != p.partition {
			return nil, errors.Join(ErrCorruptRecord, err)
		}
		saved = b.ownDueCheckpoint(p, record, ownTaskFence(point.Kvs[0]))
	}
	if err = call.postcheck(); err != nil {
		return nil, err
	}
	return saved, nil
}

// The caller already owns the original workerCall. In particular the pending
// binding uses the actual admission's charged owner, never a concurrent call.
func (b *Backend) writeDueCheckpoint(call *workerCall, p *WorkerPartitionClaim, share *WorkerBudgetShare, previous *dueCheckpoint, next dueCheckpointRecord) (saved *dueCheckpoint, resultErr error) {
	if call == nil || p == nil || !b.validWorkerBackend() {
		return nil, ErrInvalidRecord
	}
	defer func() {
		if resultErr != nil && call.started {
			call.lose()
		}
	}()
	b.archiveLife.mu.Lock()
	if !b.validWorkerPartition(p) || call.session != p.session || p.session.operation != call || previous != nil && !b.validDueCheckpoint(previous, p) {
		b.archiveLife.mu.Unlock()
		return nil, ErrInvalidRecord
	}
	cmps, err := b.workerComparisons(p.session, p, share)
	next.WriterSessionID = p.session.reference.SessionID
	next.WriterPartitionRevision = p.fence.create
	b.archiveLife.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if next.Partition != p.partition || b.validateDueCheckpoint(next) != nil {
		return nil, ErrInvalidRecord
	}
	key, err := b.namespace.dueCheckpointKey(p.partition)
	if err != nil {
		return nil, err
	}
	// Retaining this full prior fence is essential when a new window replaces
	// an old view: a reset never pretends that an existing permanent row is absent.
	if previous == nil {
		cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	} else {
		if previous.fence.key != key {
			return nil, ErrInvalidRecord
		}
		cmps = append(cmps, previous.fence.comparisons()...)
	}
	wire, err := encodeDueCheckpoint(next)
	if err != nil {
		return nil, err
	}
	protoOps := []*pb.RequestOp{taskProtoPut(key, wire, 0)}
	expected := taskProtoPutResponse(1)
	if err = b.preflightDueCheckpoint(p.session, cmps, protoOps, expected); err != nil {
		return nil, err
	}
	r, err := b.workerTxn(call, cmps, []clientv3.Op{clientv3.OpPut(key, wire)}, protoOps, expected)
	if err != nil {
		return nil, err
	}
	if dueCheckpointHeader(r) != nil || len(r.Responses) != 1 || r.Responses[0] == nil || len(r.Responses[0].XXX_unrecognized) != 0 {
		return nil, ErrOutcomeUnknown
	}
	put := r.Responses[0].GetResponsePut()
	if put == nil || put.PrevKv != nil || len(put.XXX_unrecognized) != 0 || !dueCheckpointNestedHeader(put.Header, r.Header) || r.Header.Revision <= next.Revision || previous != nil && r.Header.Revision <= previous.fence.mod {
		return nil, ErrOutcomeUnknown
	}
	if err = call.postcheck(); err != nil {
		return nil, err
	}
	create := r.Header.Revision
	if previous != nil {
		create = previous.fence.create
	}
	return b.ownDueCheckpoint(p, next, taskFence{key: key, value: wire, lease: 0, create: create, mod: r.Header.Revision}), nil
}

func (b *Backend) resumeDuePage(ctx context.Context, p *WorkerPartitionClaim, ranges *WorkerBudgetShare, saved *dueCheckpoint) (DuePage, error) {
	if p == nil || !b.validDueCheckpoint(saved, p) || b.validateDueCheckpoint(saved.record) != nil || saved.record.Pending != nil {
		return DuePage{}, ErrInvalidRecord
	}
	r := saved.record
	prefix := fmt.Sprintf("%sp/%02x/due/", r.Namespace, r.Partition)
	start := prefix
	if r.Frontier != "" {
		start = r.Frontier + "\x00"
	}
	// Rebuild a new one-use cursor only after a guarded checkpoint read under
	// these original capabilities. Serialized cursor-like fields alone cannot
	// be passed to ReadDuePage to gain this authority.
	cursor := &DueCursor{origin: b, partition: p, share: ranges, start: start, end: prefix + r.WindowEnd.Format(dueTimeLayout) + "0", revision: r.Revision, windowEnd: r.WindowEnd}
	cursor.self = cursor
	return b.ReadDuePage(ctx, p, ranges, cursor)
}
