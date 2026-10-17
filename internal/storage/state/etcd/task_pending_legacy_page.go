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

// Legacy KeysOnly ranges may fetch Limit+1 full values before truncation.
// The trusted generic Stage ceiling gives 4096+3*(1024+65536+128)=204160
// bytes; the two returned keys need only 4096+2*(1024+128)=6400 bytes.
// Arbitrary out-of-band writes above that ceiling are not supported here.
const cleanupLegacyPageEntries = 2

// A row is discovery metadata, not an authenticated cleanup intent or Task.
// Birth lets a later proof loader classify the immutable legacy cutoff locally.
type cleanupLegacyRow struct {
	taskID string
	birth  int64
}

// All routing fields are immutable after publication. The original embedded
// DueCursor owns its self identity and used bit under archiveLife.mu; copying
// this wrapper or adopting it with another B observation cannot transfer it.
type cleanupLegacyCursor struct {
	cursor       DueCursor
	state        *cleanupBackfillState
	fence        taskFence
	cutoff, view int64
	frontier     string
}

// EOF describes only exhaustion of this fixed-view suffix. Even nonempty EOF
// leaves every returned row unprocessed. A later coverage CAS must authenticate
// the rows and the exact current B predecessor; this reader never advances B.
type cleanupLegacyPage struct {
	origin     *Backend
	self       *cleanupLegacyPage
	state      *cleanupBackfillState
	fence      taskFence
	ranges     *WorkerBudgetShare
	revision   int64
	start, end string
	rows       []cleanupLegacyRow
	next       *cleanupLegacyCursor
	eof        bool
}

// Check immutable B/cursor routing before original admission spends credit.
// Partition membership uses the original lock; mutable lifecycle, share and
// cursor-used checks remain in beginDuePage. The original postcheck validates
// lifecycle again after native return, before publishing a page.
func (b *Backend) cleanupLegacyBounds(p *WorkerPartitionClaim, state *cleanupBackfillState, cursor *cleanupLegacyCursor) (string, string, error) {
	if !b.validWorkerBackend() {
		return "", "", ErrInvalidRecord
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if !b.validWorkerPartition(p) || state == nil || state.self != state || state.origin != b || state.partition != p {
		return "", "", ErrInvalidRecord
	}
	r := state.record
	if r.Validate() != nil || r.Phase != "list" || r.Namespace != b.namespace.Root() || r.RestoreEpoch != b.restoreEpoch || r.IdentityDigest != workerIdentityDigest(b.identityValue) || r.Partition != p.partition {
		return "", "", ErrInvalidRecord
	}
	key, keyErr := b.namespace.cleanupBackfillKey(p.partition)
	wire, wireErr := encodeCleanupBackfill(r)
	f := state.fence
	if keyErr != nil || wireErr != nil || f.key != key || f.value != wire || f.lease != 0 || f.create <= r.LegacyCutoffRevision || f.mod < f.create || f.mod <= r.ReadViewRevision {
		return "", "", ErrInvalidRecord
	}

	prefix := fmt.Sprintf("%sp/%02x/cleanup-intents/", b.namespace.Root(), p.partition)
	start, end := prefix, clientv3.GetPrefixRangeEnd(prefix)
	if r.Frontier != "" {
		start += r.Frontier + "\x00"
	}
	if cursor == nil {
		return start, end, nil
	}
	if cursor.state != state || cursor.fence != f || cursor.cutoff != r.LegacyCutoffRevision || cursor.view != r.ReadViewRevision || cursor.frontier != r.Frontier {
		return "", "", ErrInvalidRecord
	}
	c := &cursor.cursor
	if c.revision != r.ReadViewRevision || c.end != end || c.start <= start || c.start >= end || len(c.start) > maxKeyBytes+1 || !strings.HasPrefix(c.start, prefix) || !strings.HasSuffix(c.start, "\x00") {
		return "", "", ErrInvalidRecord
	}
	id := strings.TrimSuffix(strings.TrimPrefix(c.start, prefix), "\x00")
	if !validPreparationUUID(id) {
		return "", "", ErrInvalidRecord
	}
	return c.start, end, nil
}

// Dispatch and projection share this concrete flat-I Range shape. Natural key
// order uses no server sort or revision filter, so value fetching retains the
// backend Limit+1 bound. This does not bound Count computation or total CPU.
func cleanupLegacyRange(start, end string, revision int64) (clientv3.Op, *pb.RequestOp) {
	q := &pb.RangeRequest{
		Key: []byte(start), RangeEnd: []byte(end), Revision: revision,
		Limit: cleanupLegacyPageEntries, KeysOnly: true,
		SortTarget: pb.RangeRequest_KEY, SortOrder: pb.RangeRequest_NONE,
	}
	op := clientv3.OpGet(start, clientv3.WithRange(end), clientv3.WithRev(q.Revision), clientv3.WithLimit(q.Limit), clientv3.WithKeysOnly(), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
	return op, &pb.RequestOp{Request: &pb.RequestOp_RequestRange{RequestRange: q}}
}

// Project both native reply branches and maximal header/revision/lease widths.
// The existing B preflight also pads these same W20 metadata comparisons and
// validates their actual values. Padding is accounting, never read authority.
func (b *Backend) preflightCleanupLegacyPage(cmps []clientv3.Cmp, start, end string, revision int64, session *WorkerSession) error {
	if !b.validWorkerBackend() || session == nil || revision <= 0 || len(cmps) != 20 || cleanupLegacyPageEntries != 2 {
		return ErrInvalidMutation
	}
	if 4096+(cleanupLegacyPageEntries+1)*(maxKeyBytes+maxRecordBytes+128) > maxMutationBytes {
		return ErrInvalidMutation
	}
	_, read := cleanupLegacyRange(start, end, revision)
	read.GetRequestRange().Revision = math.MaxInt64
	failureKeys := b.workerFailureKeys(session)
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: []*pb.RequestOp{read}, Failure: taskProtoPoints(failureKeys)}
	header := taskProtoPutResponse(0).Header
	rangeReply := &pb.RangeResponse{Header: header, More: true, Count: math.MaxInt64}
	for i := 0; i < cleanupLegacyPageEntries; i++ {
		rangeReply.Kvs = append(rangeReply.Kvs, &mvccpb.KeyValue{
			Key:            bytes.Repeat([]byte("k"), maxKeyBytes),
			CreateRevision: math.MaxInt64, ModRevision: math.MaxInt64, Version: math.MaxInt64, Lease: math.MaxInt64,
		})
	}
	reply := &pb.TxnResponse{Header: header, Succeeded: true, Responses: []*pb.ResponseOp{{Response: &pb.ResponseOp_ResponseRange{ResponseRange: rangeReply}}}}
	if reply.Size() > 4096+cleanupLegacyPageEntries*(maxKeyBytes+128) {
		return ErrInvalidMutation
	}
	projection := cleanupWireProjection{name: "legacy-page", request: request, success: reply, failure: b.cleanupBackfillPointReply(failureKeys)}
	return b.preflightCleanupBackfill(projection, 25)
}

// Read one fixed B view using the original Range charge, one-use cursor and
// actual-return owner. Callers that refill worker state also use RunWorkerRelist.
// B is not compared by this Range: a concurrent refresh may stale discovery,
// and only a later exact-B CAS can establish coverage. No outcome refunds credit.
func (b *Backend) readCleanupLegacyPage(ctx context.Context, p *WorkerPartitionClaim, ranges *WorkerBudgetShare, state *cleanupBackfillState, cursor *cleanupLegacyCursor) (_ *cleanupLegacyPage, resultErr error) {
	if ctx == nil {
		return nil, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	start, end, err := b.cleanupLegacyBounds(p, state, cursor)
	if err != nil {
		return nil, err
	}
	var carrier *DueCursor
	if cursor != nil {
		carrier = &cursor.cursor
	}
	o, err := b.beginDuePage(ctx, p, ranges, carrier)
	if err != nil {
		return nil, err
	}
	defer o.finish()
	call := o.call
	knownCompaction := false
	defer func() {
		if resultErr != nil && call.started && !knownCompaction {
			call.lose()
		}
	}()

	revision := state.record.ReadViewRevision
	b.archiveLife.mu.Lock()
	cmps, err := b.workerComparisons(p.session, p, ranges)
	b.archiveLife.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err = b.preflightCleanupLegacyPage(cmps, start, end, revision, p.session); err != nil {
		return nil, err
	}
	rpc, cancel := call.requestContext()
	if err = rpc.Err(); err != nil {
		cancel()
		return nil, err
	}
	op, _ := cleanupLegacyRange(start, end, revision)
	call.started = true
	response, err := b.client.Txn(rpc).If(cmps...).Then(op).Else(workspaceSyncPointOps(b.workerFailureKeys(p.session))...).Commit()
	rpcErr := rpc.Err()
	cancel()
	if err != nil || rpcErr != nil {
		// Only a live, known-compacted call may audit guards before preserving
		// its session. The same paid owner stays held through this second RPC.
		if errors.Is(err, rpctypes.ErrCompacted) && rpcErr == nil {
			if guardErr := b.workerGuardCheck(call, p, ranges); guardErr != nil {
				return nil, errors.Join(err, guardErr)
			}
			knownCompaction = true
			return nil, rpctypes.ErrCompacted
		}
		return nil, errors.Join(ErrOutcomeUnknown, err, rpcErr)
	}

	if err = b.operationResponseHeader(response); err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	if !enrolledResponseBound(response) {
		return nil, ErrCorruptRecord
	}
	if !response.Succeeded {
		return nil, b.workerFailedFences(p.session, response)
	}
	prefix := fmt.Sprintf("%sp/%02x/cleanup-intents/", b.namespace.Root(), p.partition)
	rows, last, more, err := decodeCleanupLegacyRange(response, prefix, start, end, revision)
	if err != nil {
		return nil, err
	}
	if err = o.postcheck(); err != nil {
		return nil, err
	}

	page := &cleanupLegacyPage{origin: b, state: state, fence: state.fence, ranges: ranges, revision: revision, start: start, end: end, rows: rows, eof: !more}
	page.self = page
	if more {
		next := &cleanupLegacyCursor{
			cursor: DueCursor{origin: b, partition: p, share: ranges, start: last + "\x00", end: end, revision: revision},
			state:  state, fence: state.fence, cutoff: state.record.LegacyCutoffRevision, view: revision, frontier: state.record.Frontier,
		}
		next.cursor.self = &next.cursor
		page.next = next
	}
	return page, nil
}

// Legacy immutable envelopes are all-or-error, never skipped. Returned UUIDs
// own their bytes; raw-key continuation cannot borrow the native response.
// Both sides of cutoff remain visible for later local classification and proof.
func decodeCleanupLegacyRange(response *clientv3.TxnResponse, prefix, start, end string, revision int64) ([]cleanupLegacyRow, string, bool, error) {
	if response == nil || response.Header == nil || revision <= 0 || response.Header.Revision < revision || len(response.Responses) != 1 || response.Responses[0] == nil {
		return nil, "", false, ErrCorruptRecord
	}
	r := response.Responses[0].GetResponseRange()
	if r == nil || !dueRangeHeader(r.Header, response.Header, revision) || len(r.Kvs) > cleanupLegacyPageEntries {
		return nil, "", false, ErrCorruptRecord
	}
	count := int64(len(r.Kvs))
	if r.Count < count || !r.More && r.Count != count || r.More && (len(r.Kvs) != cleanupLegacyPageEntries || r.Count <= count) {
		return nil, "", false, ErrCorruptRecord
	}
	var rows []cleanupLegacyRow
	var previous []byte
	for _, kv := range r.Kvs {
		if kv == nil || len(kv.Key) == 0 || len(kv.Key) > maxKeyBytes || len(kv.Value) != 0 || kv.Version != 1 || kv.Lease != 0 {
			return nil, "", false, ErrCorruptRecord
		}
		if kv.CreateRevision <= 0 || kv.ModRevision != kv.CreateRevision || kv.ModRevision > revision {
			return nil, "", false, ErrCorruptRecord
		}
		if bytes.Compare(kv.Key, []byte(start)) < 0 || bytes.Compare(kv.Key, []byte(end)) >= 0 || previous != nil && bytes.Compare(kv.Key, previous) <= 0 {
			return nil, "", false, ErrCorruptRecord
		}
		key := string(kv.Key)
		id := strings.TrimPrefix(key, prefix)
		if !strings.HasPrefix(key, prefix) || !validPreparationUUID(id) {
			return nil, "", false, ErrCorruptRecord
		}
		rows = append(rows, cleanupLegacyRow{taskID: id, birth: kv.CreateRevision})
		previous = kv.Key
	}
	return rows, string(previous), r.More, nil
}
