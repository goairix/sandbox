package etcd

import (
	"context"
	"encoding/hex"
	"errors"
	"math"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This fixed canonical completion is only hashed into the existing charged slot.
type taskWorkspaceSyncLocalCompletion struct {
	Version      uint32                           `json:"version"`
	Reference    TaskWorkspaceSyncIntentReference `json:"reference"`
	IntentBirth  int64                            `json:"intent_birth"`
	IntentDigest string                           `json:"intent_digest"`
	Bytes        int64                            `json:"bytes"`
	BodySHA256   string                           `json:"body_sha256"`
	Dev          uint64                           `json:"dev"`
	Ino          uint64                           `json:"ino"`
}

func (c taskWorkspaceSyncLocalCompletion) Validate() error {
	if c.Version != 1 || c.Reference.Validate() != nil || c.Reference.Revision != 0 || c.IntentBirth <= 0 || !validHexDigest(c.IntentDigest) || c.Bytes < 0 || !validHexDigest(c.BodySHA256) || c.Dev == 0 || c.Ino == 0 {
		return ErrInvalidRecord
	}
	return nil
}

func recyclingTerminalValue(d *taskWorkspaceSyncIntentDraft, w *taskWorkspaceSyncLocalWitness) (string, error) {
	completion := taskWorkspaceSyncLocalCompletion{1, d.record.Reference, d.revision, workspaceSyncIntentDigest(d.value), w.bytes, w.sha256, w.dev, w.ino}
	value, err := encodeOperationMetadata(completion, 4096)
	if err != nil {
		return "", err
	}
	r := d.record.Reference
	return encodeWorkspaceSyncRecyclingSlot(taskWorkspaceSyncRecyclingSlotRecord{Version: 2, BudgetID: r.BudgetID, Partition: r.Task.Partition, Slot: r.Slot, State: "terminal", IntentID: r.IntentID, IntentDigest: completion.IntentDigest, IntentBirth: d.revision, EvidenceDigest: workspaceSyncIntentDigest(value)})
}

// An operation owns all three lifetime registrations through native RPC and
// physical postguard. No source/body/target mutex crosses either boundary.
type taskWorkspaceSyncSettlementOperation struct {
	source *PreparedTaskWorkspaceSyncSource
	body   *taskWorkspaceSyncBody
	target *taskWorkspaceSyncLocalTarget
	ctx    context.Context
	cancel context.CancelFunc
	stops  []func() bool
}

func (s *PreparedTaskWorkspaceSyncSource) beginLocalSettlement(ctx context.Context, w *taskWorkspaceSyncLocalWitness) (*taskWorkspaceSyncSettlementOperation, error) {
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	if !w.original() || w.origin.source != s || !s.originalLocalBody(w.origin.body) {
		s.end()
		return nil, ErrInvalidRecord
	}
	p, body, target, d := w.origin, w.origin.body, w.origin.target, w.origin.draft
	p.mu.Lock()
	joined := false
	if p.ioDone != nil {
		select {
		case <-p.ioDone:
			joined = p.sealed && !p.active
		default:
		}
	}
	p.mu.Unlock()
	b := s.archive.owner.backend
	body.mu.Lock()
	versionValid := d.recycling != nil && recyclingCanAdvance(d.recycling.version) && recyclingCanAdvance(d.recycling.version+1)
	valid := joined && body.intent == d && body.localTarget == target &&
		!body.closed && !body.active && body.localConsumed && body.complete && body.ctx.Err() == nil &&
		body.bytes == d.record.Size && hex.EncodeToString(body.hash.Sum(nil)) == d.record.BodySHA256 &&
		w.bytes == d.record.Size && w.sha256 == d.record.BodySHA256 &&
		d.record.Local != nil && w.dev == d.record.Local.Dev && w.ino != 0 &&
		d.acknowledged && d.revision > 0 && d.revision < math.MaxInt64 && versionValid &&
		b.workspaceSyncBudget != nil && b.workspaceSyncBudget.profile == 2 &&
		target != nil && target.self == target && *d.record.Local == target.pin
	if !valid {
		body.mu.Unlock()
		s.end()
		return nil, ErrInvalidRecord
	}
	body.active, body.done = true, make(chan struct{})
	body.mu.Unlock()
	if err := target.begin(ctx); err != nil {
		body.mu.Lock()
		body.active = false
		close(body.done)
		body.mu.Unlock()
		s.end()
		return nil, err
	}
	owned, cancel := context.WithCancel(ctx)
	op := &taskWorkspaceSyncSettlementOperation{source: s, body: body, target: target, ctx: owned, cancel: cancel}
	op.stops = []func() bool{context.AfterFunc(s.ctx, cancel), context.AfterFunc(body.ctx, cancel), context.AfterFunc(target.ctx, cancel)}
	if err := op.guard(); err != nil {
		op.end()
		return nil, err
	}
	return op, nil
}

func (op *taskWorkspaceSyncSettlementOperation) local() error {
	return errors.Join(op.source.local(op.ctx), op.ctx.Err(), op.body.ctx.Err(), op.target.ctx.Err())
}

func (op *taskWorkspaceSyncSettlementOperation) guard() error {
	if err := op.local(); err != nil {
		return err
	}
	return errors.Join(op.target.check(op.ctx), op.local())
}

func (op *taskWorkspaceSyncSettlementOperation) end() {
	op.cancel()
	for _, stop := range op.stops {
		stop()
	}
	op.body.mu.Lock()
	op.body.active = false
	close(op.body.done)
	op.body.mu.Unlock()
	op.target.ops.Done()
	op.source.end()
}

func (s *PreparedTaskWorkspaceSyncSource) recyclingCompletionComparisons(d *taskWorkspaceSyncIntentDraft, intent bool) []clientv3.Cmp {
	o := s.archive.owner
	cmps := append([]clientv3.Cmp(nil), o.comparisons...)
	cmps = append(cmps, clientv3.Compare(clientv3.Version(o.prefix), "=", 0).WithRange(clientv3.GetPrefixRangeEnd(o.prefix)))
	cmps = append(cmps, o.backend.workspaceSyncBudget.comparisons()...)
	if intent {
		cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(d.intentKey), "=", d.revision), clientv3.Compare(clientv3.ModRevision(d.intentKey), "=", d.revision), clientv3.Compare(clientv3.Value(d.intentKey), "=", d.value))
	}
	return cmps
}

func (s *PreparedTaskWorkspaceSyncSource) recyclingTerminalComparisons(d *taskWorkspaceSyncIntentDraft) []clientv3.Cmp {
	return append(s.recyclingCompletionComparisons(d, true), clientv3.Compare(clientv3.CreateRevision(d.slotKey), "=", d.recycling.create), clientv3.Compare(clientv3.ModRevision(d.slotKey), "=", d.revision), clientv3.Compare(clientv3.Value(d.slotKey), "=", d.slotValue))
}

func (op *taskWorkspaceSyncSettlementOperation) native(cmps []clientv3.Cmp, success []clientv3.Op, protoSuccess []*pb.RequestOp, reply *pb.TxnResponse) (*clientv3.TxnResponse, error) {
	o, b := op.source.archive.owner, op.source.archive.owner.backend
	keys := []string{b.identityKey, b.restoreKey, b.workspaceSyncBudget.key}
	values := []string{b.identityValue, b.restoreEpoch, b.workspaceSyncBudget.value}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: protoSuccess, Failure: taskProtoPoints(keys)}
	if err := taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{reply, taskProtoResponse(keys, values)}); err != nil {
		return nil, err
	}
	o.nativeMu.Lock()
	defer o.nativeMu.Unlock()
	if err := op.local(); err != nil {
		return nil, err
	}
	rpcCtx, cancel := b.requestContext(op.ctx)
	r, err := b.client.Txn(rpcCtx).If(cmps...).Then(success...).Else(workspaceSyncPointOps(keys)...).Commit()
	rpcErr := rpcCtx.Err()
	cancel()
	if err != nil || rpcErr != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err, rpcErr)
	}
	if err := b.operationResponseHeader(r); err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	if err := taskCheckWire(b.namespace, nil, []*pb.TxnResponse{(*pb.TxnResponse)(r)}); err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	if !r.Succeeded {
		points, err := workspaceSyncPointResponse(b, r, keys)
		if err != nil {
			return nil, errors.Join(ErrOutcomeUnknown, err)
		}
		if !b.workspaceSyncBudgetMatches(points[2]) {
			return r, ErrIdentityMismatch
		}
		return r, ErrConflict
	}
	return r, nil
}

func recyclingPutACK(r *clientv3.TxnResponse, count int) bool {
	if r == nil || len(r.Responses) != count {
		return false
	}
	for _, response := range r.Responses {
		if response == nil || response.GetResponsePut() == nil || response.GetResponsePut().PrevKv != nil || !operationNestedHeader(response.GetResponsePut().Header, r.Header) {
			return false
		}
	}
	return true
}

func (op *taskWorkspaceSyncSettlementOperation) readCompletion(d *taskWorkspaceSyncIntentDraft) ([]*mvccpb.KeyValue, error) {
	b := op.source.archive.owner.backend
	keys := []string{d.slotKey, d.intentKey}
	values := []string{string(make([]byte, 512)), string(make([]byte, b.workspaceSyncBudget.options.MaxIntentBytes))}
	r, err := op.native(op.source.recyclingCompletionComparisons(d, false), workspaceSyncPointOps(keys), taskProtoPoints(keys), taskProtoResponse(keys, values))
	if err != nil {
		return nil, err
	}
	return workspaceSyncPointResponse(b, r, keys)
}

func (s *PreparedTaskWorkspaceSyncSource) settleLocal(ctx context.Context, w *taskWorkspaceSyncLocalWitness) (*taskWorkspaceSyncLocalSettlement, error) {
	op, err := s.beginLocalSettlement(ctx, w)
	if err != nil {
		return nil, err
	}
	defer op.end()
	d := w.origin.draft
	value, err := recyclingTerminalValue(d, w)
	if err != nil {
		return nil, err
	}
	q := d.localSettlement
	if q == nil {
		q = &taskWorkspaceSyncLocalSettlement{witness: w, draft: d, value: value}
		q.self, d.localSettlement = q, q
	}
	if !q.original() || q.value != value || q.completed {
		return nil, ErrInvalidRecord
	}
	if q.denied {
		return q, ErrConflict
	}
	if q.attempted {
		points, err := op.readCompletion(d)
		if err != nil {
			return q, err
		}
		kv := points[0]
		if !immutableWorkspaceSyncKV(points[1]) || points[1].CreateRevision != d.revision || string(points[1].Value) != d.value || !recyclingGenerationMatches(kv, d.recycling.create, d.recycling.version+2, d.revision, q.value) {
			return q, ErrOutcomeUnknown
		}
		q.terminal = taskWorkspaceSyncRecyclingGeneration{kv.CreateRevision, kv.ModRevision, kv.Version, q.value}
	} else {
		q.attempted = true // never reset, including known rejection or lost ACK
		r, err := op.native(s.recyclingTerminalComparisons(d), []clientv3.Op{clientv3.OpPut(d.slotKey, value)}, []*pb.RequestOp{taskProtoPut(d.slotKey, value, 0)}, taskProtoPutResponse(1))
		if err != nil {
			q.denied = r != nil && !r.Succeeded
			return q, err
		}
		if !recyclingPutACK(r, 1) || r.Header.Revision <= d.revision || r.Header.Revision == math.MaxInt64 {
			return q, ErrOutcomeUnknown
		}
		q.terminal = taskWorkspaceSyncRecyclingGeneration{d.recycling.create, r.Header.Revision, d.recycling.version + 2, value}
	}
	if err := op.guard(); err != nil {
		return q, err
	}
	if q.probeComplete != nil {
		q.probeComplete()
	}
	if err := op.local(); err != nil {
		return q, err
	}
	q.known = true
	return q, nil
}

func recyclingGenerationMatches(kv *mvccpb.KeyValue, create, version, previous int64, value string) bool {
	if _, err := decodeWorkspaceSyncRecyclingSlot(kv); err != nil {
		return false
	}
	return kv.CreateRevision == create && kv.Version == version && kv.ModRevision > previous && string(kv.Value) == value
}

func recyclingCanAdvance(version int64) bool {
	// Max is deliberately unusable, matching the strict native slot decoder.
	return version > 0 && version < math.MaxInt64-1
}
