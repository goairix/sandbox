package etcd

import (
	"context"
	"errors"
	"math"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// These immutable private objects are published only by authenticated reads
// or acknowledged CAS. Self/origin/partition identity prevents diagnostic
// copies and replacement sessions from adopting another owner's observation.
type cleanupBackfillState struct {
	origin    *Backend
	self      *cleanupBackfillState
	partition *WorkerPartitionClaim
	record    cleanupBackfillRecord
	fence     taskFence
}

type cleanupBackfillObservation struct {
	origin    *Backend
	self      *cleanupBackfillObservation
	partition *WorkerPartitionClaim
	revision  int64
	state     *cleanupBackfillState
}

// Read a current guarded point, not a caller-selected historical revision.
// Original Range credit and the native call remain owned through validation;
// cancellation or malformed replies cannot publish a usable observation.
func (b *Backend) readCleanupBackfill(ctx context.Context, p *WorkerPartitionClaim, ranges *WorkerBudgetShare) (_ *cleanupBackfillObservation, resultErr error) {
	o, err := b.beginDuePage(ctx, p, ranges, nil)
	if err != nil {
		return nil, err
	}
	defer o.finish()
	defer func() {
		if resultErr != nil && o.call.started {
			o.call.lose()
		}
	}()

	key, err := b.namespace.cleanupBackfillKey(p.partition)
	if err != nil {
		return nil, err
	}
	b.archiveLife.mu.Lock()
	cmps, err := b.workerComparisons(p.session, p, ranges)
	b.archiveLife.mu.Unlock()
	if err != nil {
		return nil, err
	}
	keys, failureKeys := []string{key}, b.workerFailureKeys(p.session)
	reads := taskProtoPoints(keys)
	expected := b.cleanupBackfillPointReply(keys)
	projection := cleanupWireProjection{
		name:    "backfill-read",
		request: &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: reads, Failure: taskProtoPoints(failureKeys)},
		success: expected, failure: b.cleanupBackfillPointReply(failureKeys),
	}
	if err = b.preflightCleanupBackfill(projection, 25); err != nil {
		return nil, err
	}

	response, err := b.workerTxn(o.call, cmps, workspaceSyncPointOps(keys), reads, expected)
	if err != nil {
		return nil, err
	}
	points, err := b.workerPoints(response, keys)
	if err != nil {
		return nil, err
	}
	observed := &cleanupBackfillObservation{origin: b, partition: p, revision: response.Header.Revision}
	observed.self = observed
	if points[0] != nil {
		r, err := decodeCleanupBackfill(b.namespace, b.restoreEpoch, workerIdentityDigest(b.identityValue), p.partition, points[0], observed.revision)
		if err != nil {
			return nil, err
		}
		state := &cleanupBackfillState{origin: b, partition: p, record: r, fence: ownTaskFence(points[0])}
		state.self = state
		observed.state = state
	}
	if err = o.postcheck(); err != nil {
		return nil, err
	}
	return observed, nil
}

// Validate original ownership before spending Claim credit, including the
// dormant-complete path. All mutable lifecycle checks use archiveLife.mu;
// observation/state contents are immutable once published by this package.
func (b *Backend) validateCleanupBackfillObservation(ctx context.Context, p *WorkerPartitionClaim, claims *WorkerBudgetShare, observed *cleanupBackfillObservation) error {
	if ctx == nil || !b.validWorkerBackend() {
		return ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if !b.validWorkerPartition(p) || !b.validWorkerBudget(claims) || claims.kind != WorkerBudgetClaim || claims.session != p.session {
		return ErrInvalidRecord
	}
	if observed == nil || observed.self != observed || observed.origin != b || observed.partition != p || observed.revision <= 0 {
		return ErrInvalidRecord
	}
	if b.archiveLife.closed {
		return context.Canceled
	}
	if p.release != nil || p.released || p.lost || claims.release != nil || claims.released || claims.lost || p.session.release != nil {
		return ErrGuardExpired
	}
	if err := p.session.live(); err != nil {
		return err
	}
	if s := observed.state; s != nil {
		if s.self != s || s.origin != b || s.partition != p || s.record.Validate() != nil {
			return ErrInvalidRecord
		}
		if s.record.Namespace != b.namespace.Root() || s.record.RestoreEpoch != b.restoreEpoch || s.record.IdentityDigest != workerIdentityDigest(b.identityValue) || s.record.Partition != p.partition {
			return ErrInvalidRecord
		}
		key, err := b.namespace.cleanupBackfillKey(p.partition)
		wire, wireErr := encodeCleanupBackfill(s.record)
		if err != nil || wireErr != nil || s.fence.key != key || s.fence.value != wire || s.fence.lease != 0 {
			return ErrInvalidRecord
		}
		if s.fence.create <= s.record.LegacyCutoffRevision || s.fence.mod < s.fence.create || s.fence.mod <= s.record.ReadViewRevision || s.fence.mod > observed.revision {
			return ErrInvalidRecord
		}
	}
	return ctx.Err()
}

// Bootstrap or refresh exactly one observed checkpoint. No frontier or cutoff
// can be supplied by a caller. The concrete mutation is shared by projection
// and dispatch, and only a validated native Put ACK can publish the new fence.
func (b *Backend) refreshCleanupBackfill(ctx context.Context, p *WorkerPartitionClaim, claims *WorkerBudgetShare, observed *cleanupBackfillObservation) (_ *cleanupBackfillState, resultErr error) {
	if err := b.validateCleanupBackfillObservation(ctx, p, claims, observed); err != nil {
		return nil, err
	}
	if observed.state != nil && observed.state.record.Phase == "complete" {
		return observed.state, nil
	}
	o, stop, err := b.beginExpiredTaskOwner(ctx, p, claims)
	if err != nil {
		return nil, err
	}
	defer stop()
	defer o.finish()
	defer func() {
		if resultErr != nil && o.call.started && !errors.Is(resultErr, ErrConflict) {
			o.call.lose()
		}
	}()

	next, mutation, err := b.cleanupBackfillMutation(o, observed)
	if err != nil {
		return nil, err
	}
	write := mutation.Writes[0]
	projection := b.cleanupProjection("backfill-refresh", mutation.Comparisons, taskProtoWrites(mutation.Writes), taskProtoPutResponse(1))
	projection.failure = b.cleanupBackfillPointReply([]string{b.identityKey, b.restoreKey})
	nodes := 27
	if observed.state == nil {
		nodes = 24
	}
	if err = b.preflightCleanupBackfill(projection, nodes); err != nil {
		return nil, err
	}

	rpc, cancel := o.call.requestContext()
	if err = rpc.Err(); err != nil {
		cancel()
		return nil, err
	}
	o.call.started = true
	response, err := b.client.Txn(rpc).If(mutation.Comparisons...).Then(clientv3.OpPut(write.Key, string(write.Value))).Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey)).Commit()
	rpcErr := rpc.Err()
	cancel()
	if err != nil || rpcErr != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err, rpcErr)
	}

	if err = b.operationResponseHeader(response); err != nil || !enrolledResponseBound(response) {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	if !response.Succeeded {
		if _, err = b.stageEvidencePoints(response, []string{b.identityKey, b.restoreKey}); err != nil {
			return nil, errors.Join(ErrOutcomeUnknown, err)
		}
		return nil, ErrConflict
	}
	if !operationPutResponses(response, 1) || response.Header.Revision <= observed.revision {
		return nil, ErrOutcomeUnknown
	}
	if err = o.postcheck(); err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}

	create := response.Header.Revision
	if observed.state != nil {
		create = observed.state.fence.create
	}
	state := &cleanupBackfillState{origin: b, partition: p, record: next, fence: taskFence{key: write.Key, value: string(write.Value), create: create, mod: response.Header.Revision}}
	state.self = state
	return state, nil
}

// This builder accepts only the current original Claim owner and its observed
// predecessor. The resulting mutation never travels through generic Stage.
func (b *Backend) cleanupBackfillMutation(o *workerDueOwner, observed *cleanupBackfillObservation) (cleanupBackfillRecord, Mutation, error) {
	cmps, err := b.cleanupOwnerComparisons(o)
	if err != nil {
		return cleanupBackfillRecord{}, Mutation{}, err
	}
	key, err := b.namespace.cleanupBackfillKey(o.partition.partition)
	if err != nil {
		return cleanupBackfillRecord{}, Mutation{}, err
	}
	next := cleanupBackfillRecord{Version: 1, Namespace: b.namespace.Root(), RestoreEpoch: b.restoreEpoch, IdentityDigest: workerIdentityDigest(b.identityValue), Partition: o.partition.partition, LegacyCutoffRevision: observed.revision, ReadViewRevision: observed.revision, Phase: "list"}
	var prior *cleanupBackfillRecord
	if observed.state == nil {
		cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	} else {
		prior = &observed.state.record
		next = *prior
		next.ReadViewRevision = observed.revision
		cmps = append(cmps, observed.state.fence.comparisons()...)
	}
	if err = validateCleanupBackfillRefresh(prior, next); err != nil {
		return cleanupBackfillRecord{}, Mutation{}, err
	}
	value, err := encodeCleanupBackfill(next)
	if err != nil {
		return cleanupBackfillRecord{}, Mutation{}, err
	}
	return next, Mutation{Comparisons: cmps, Writes: []Write{{Key: key, Value: []byte(value)}}}, nil
}

func (b *Backend) cleanupBackfillPointReply(keys []string) *pb.TxnResponse {
	values := make([]string, len(keys))
	for i, key := range keys {
		values[i] = cleanupWirePadding(b.cleanupBackfillValueBound(key))
	}
	return taskProtoResponse(keys, values)
}

func (b *Backend) cleanupBackfillValueBound(key string) int {
	if key == b.identityKey {
		return maxIdentityJSONBytes
	}
	if key == b.restoreKey {
		return maxNamespaceComponentBytes
	}
	return cleanupBackfillBytes // B and original worker records each cap at 2048 bytes.
}

// Validate the actual dispatch shape and its conservative scalar/value copy.
// Only this copy is padded: its bytes never enter native comparisons or writes.
// Both response branches already carry maximal headers/scalars and full caps.
func (b *Backend) preflightCleanupBackfill(projection cleanupWireProjection, nodes int) error {
	request := projection.request
	if request == nil || len(request.Compare)+len(request.Success)+len(request.Failure) != nodes {
		return ErrInvalidMutation
	}
	if err := validateCleanupWire(projection); err != nil {
		return err
	}
	maximum := *request
	cmps := make([]clientv3.Cmp, len(request.Compare))
	for i, cmp := range request.Compare {
		cmps[i] = copyComparison(clientv3.Cmp(*cmp))
		switch value := cmps[i].TargetUnion.(type) {
		case *pb.Compare_Value:
			value.Value = []byte(cleanupWirePadding(b.cleanupBackfillValueBound(string(cmp.Key))))
		case *pb.Compare_CreateRevision:
			if value.CreateRevision > 0 {
				value.CreateRevision = math.MaxInt64
			}
		case *pb.Compare_ModRevision:
			value.ModRevision = math.MaxInt64
		case *pb.Compare_Lease:
			if value.Lease > 0 {
				value.Lease = math.MaxInt64
			}
		}
	}
	maximum.Compare = taskProtoComparisons(cmps)
	maximum.Success = append([]*pb.RequestOp(nil), request.Success...)
	for i, op := range maximum.Success {
		if put := op.GetRequestPut(); put != nil {
			maximum.Success[i] = taskProtoPut(string(put.Key), cleanupWirePadding(cleanupBackfillBytes), 0)
		}
	}
	projection.request = &maximum
	return validateCleanupWire(projection)
}
