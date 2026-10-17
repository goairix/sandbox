package etcd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Reserve 4096 bytes for framing and 128 per row for maximal MVCC scalars.
// Eighty bounded hints fit one original 256 KiB earned Range charge.
const cleanupPendingPageEntries = (maxMutationBytes - 4096) / (maxKeyBytes + cleanupPendingBytes + 128)

// The embedded carrier shares original Due ownership and one-use admission.
// Its routing fields are immutable after publication; only used changes, under
// archiveLife.mu. Copying this wrapper cannot copy the carrier's self identity.
type cleanupPendingCursor struct {
	cursor DueCursor
}

// A page contains owned discovery hints, never Task execution authority.
// Rejected counts bounded invalid hints without retaining their native bytes.
type cleanupPendingPage struct {
	Candidates []cleanupPendingRecord
	Revision   int64
	Next       *cleanupPendingCursor
	Rejected   uint32
}

// Validate this concrete family before original admission spends any credit.
// Original beginDuePage subsequently checks session/share/cursor membership
// and consumes the cursor atomically. This routing snapshot never reads used;
// membership is inspected only under the original lifecycle lock.
func (b *Backend) cleanupPendingBounds(p *WorkerPartitionClaim, cursor *cleanupPendingCursor) (string, string, int64, error) {
	if !b.validWorkerBackend() {
		return "", "", 0, ErrInvalidRecord
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if !b.validWorkerPartition(p) {
		return "", "", 0, ErrInvalidRecord
	}
	prefix := fmt.Sprintf("%sp/%02x/cleanup-pending/", b.namespace.Root(), p.partition)
	end := clientv3.GetPrefixRangeEnd(prefix)
	if cursor == nil {
		return prefix, end, 0, nil
	}
	c := &cursor.cursor
	if c.end != end || c.start <= prefix || c.start >= end || !strings.HasPrefix(c.start, prefix) {
		return "", "", 0, ErrInvalidRecord
	}
	if !strings.HasSuffix(c.start, "\x00") || len(c.start) > maxKeyBytes+1 || c.revision <= 0 {
		return "", "", 0, ErrInvalidRecord
	}
	return c.start, end, c.revision, nil
}

// Project the concrete native shape, including both branches and maximal
// headers/scalars. Requests have the stricter 128 KiB bound; replies share the
// original 256 KiB charge. The projection never becomes native authority.
func (b *Backend) preflightCleanupPendingPage(cmps []clientv3.Cmp, start, end string, revision int64, session *WorkerSession) error {
	if cleanupPendingPageEntries != 80 || len(cmps) != 20 {
		return ErrInvalidMutation
	}
	rangeRequest := &pb.RangeRequest{Key: []byte(start), RangeEnd: []byte(end), Revision: revision, Limit: cleanupPendingPageEntries, SortOrder: pb.RangeRequest_NONE, SortTarget: pb.RangeRequest_KEY}
	request := &pb.TxnRequest{
		Compare: taskProtoComparisons(cmps),
		Success: []*pb.RequestOp{{Request: &pb.RequestOp_RequestRange{RequestRange: rangeRequest}}},
		Failure: taskProtoPoints(b.workerFailureKeys(session)),
	}
	if request.Size() > 128*1024 {
		return ErrInvalidMutation
	}
	header := taskProtoPutResponse(0).Header
	rangeReply := &pb.RangeResponse{Header: header, More: true, Count: math.MaxInt64}
	for i := 0; i < cleanupPendingPageEntries; i++ {
		rangeReply.Kvs = append(rangeReply.Kvs, &mvccpb.KeyValue{
			Key: bytes.Repeat([]byte("k"), maxKeyBytes), Value: bytes.Repeat([]byte("v"), cleanupPendingBytes),
			CreateRevision: math.MaxInt64, ModRevision: math.MaxInt64, Version: math.MaxInt64, Lease: math.MaxInt64,
		})
	}
	reply := &pb.TxnResponse{Header: header, Succeeded: true, Responses: []*pb.ResponseOp{{Response: &pb.ResponseOp_ResponseRange{ResponseRange: rangeReply}}}}
	return taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{reply, taskProtoResponse(b.workerFailureKeys(session), b.workerFailureValues(session))})
}

// Read one fixed-R pending page under the original Range owner. Callers that
// refill worker state must additionally use RunWorkerRelist. No result refunds
// credit, and cancellation retains the owner until the actual RPC and all
// validation return. A known compaction permits a fresh view only after the
// same paid call authenticates original guards; this function never retries.
func (b *Backend) readCleanupPendingPage(ctx context.Context, p *WorkerPartitionClaim, ranges *WorkerBudgetShare, cursor *cleanupPendingCursor) (page cleanupPendingPage, resultErr error) {
	if ctx == nil {
		return page, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return page, err
	}
	start, end, revision, err := b.cleanupPendingBounds(p, cursor)
	if err != nil {
		return page, err
	}
	var carrier *DueCursor
	if cursor != nil {
		carrier = &cursor.cursor
	}
	o, err := b.beginDuePage(ctx, p, ranges, carrier)
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

	b.archiveLife.mu.Lock()
	cmps, err := b.workerComparisons(p.session, p, ranges)
	b.archiveLife.mu.Unlock()
	if err != nil {
		return cleanupPendingPage{}, err
	}
	if err = b.preflightCleanupPendingPage(cmps, start, end, revision, p.session); err != nil {
		return cleanupPendingPage{}, err
	}
	rpc, cancel := call.requestContext()
	if err = rpc.Err(); err != nil {
		cancel()
		return cleanupPendingPage{}, err
	}
	op := clientv3.OpGet(start, clientv3.WithRange(end), clientv3.WithRev(revision), clientv3.WithLimit(cleanupPendingPageEntries), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
	call.started = true
	response, err := b.client.Txn(rpc).If(cmps...).Then(op).Else(workspaceSyncPointOps(b.workerFailureKeys(p.session))...).Commit()
	rpcErr := rpc.Err()
	cancel()
	if err != nil || rpcErr != nil {
		// Preserve the raw compaction classification before workerTxn's
		// generic unknown-outcome wrapping, exactly as the original reader.
		if errors.Is(err, rpctypes.ErrCompacted) && rpcErr == nil {
			if guardErr := b.workerGuardCheck(call, p, ranges); guardErr != nil {
				return cleanupPendingPage{}, errors.Join(err, guardErr)
			}
			knownCompaction = true
			return cleanupPendingPage{}, rpctypes.ErrCompacted
		}
		return cleanupPendingPage{}, errors.Join(ErrOutcomeUnknown, err, rpcErr)
	}

	if err = b.operationResponseHeader(response); err != nil {
		return cleanupPendingPage{}, errors.Join(ErrOutcomeUnknown, err)
	}
	if !enrolledResponseBound(response) {
		return cleanupPendingPage{}, ErrCorruptRecord
	}
	if !response.Succeeded {
		return cleanupPendingPage{}, b.workerFailedFences(p.session, response)
	}
	if revision == 0 {
		revision = response.Header.Revision
	}
	page, last, more, err := b.decodeCleanupPendingRange(response, start, end, revision, p.partition)
	if err != nil {
		return cleanupPendingPage{}, err
	}
	if err = o.postcheck(); err != nil {
		return cleanupPendingPage{}, err
	}
	if more {
		next := &cleanupPendingCursor{cursor: DueCursor{origin: b, partition: p, share: ranges, start: last + "\x00", end: end, revision: revision}}
		next.cursor.self = &next.cursor
		page.Next = next
	}
	return page, nil
}

// Structural corruption invalidates the entire page. Bounded but invalid
// hints only increment Rejected, so a permanently bad first row cannot starve
// later work. Continuation always uses the last raw key, even if all hints
// were rejected; neither returned records nor that key retain response bytes.
// The caller must first authenticate the outer response header and byte bound.
func (b *Backend) decodeCleanupPendingRange(response *clientv3.TxnResponse, start, end string, revision int64, partition uint8) (cleanupPendingPage, string, bool, error) {
	if revision <= 0 || response.Header.Revision < revision || len(response.Responses) != 1 || response.Responses[0] == nil {
		return cleanupPendingPage{}, "", false, ErrCorruptRecord
	}
	r := response.Responses[0].GetResponseRange()
	if r == nil || !dueRangeHeader(r.Header, response.Header, revision) || len(r.Kvs) > cleanupPendingPageEntries {
		return cleanupPendingPage{}, "", false, ErrCorruptRecord
	}
	count := int64(len(r.Kvs))
	if r.Count < count || !r.More && r.Count != count || r.More && (len(r.Kvs) != cleanupPendingPageEntries || r.Count <= count) {
		return cleanupPendingPage{}, "", false, ErrCorruptRecord
	}
	page := cleanupPendingPage{Revision: revision}
	var previous []byte
	for _, kv := range r.Kvs {
		if kv == nil || len(kv.Key) == 0 || len(kv.Key) > maxKeyBytes || len(kv.Value) > cleanupPendingBytes {
			return cleanupPendingPage{}, "", false, ErrCorruptRecord
		}
		if kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || kv.ModRevision > revision {
			return cleanupPendingPage{}, "", false, ErrCorruptRecord
		}
		if bytes.Compare(kv.Key, []byte(start)) < 0 || bytes.Compare(kv.Key, []byte(end)) >= 0 || previous != nil && bytes.Compare(kv.Key, previous) <= 0 {
			return cleanupPendingPage{}, "", false, ErrCorruptRecord
		}
		previous = kv.Key
		candidate, err := decodeCleanupPending(b.namespace, b.restoreEpoch, kv, revision)
		if err != nil || candidate.Reference.Partition != partition {
			page.Rejected++
			continue
		}
		page.Candidates = append(page.Candidates, candidate)
	}
	return page, string(previous), r.More, nil
}
