package etcd

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/goairix/sandbox/internal/runtime/launcher"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const cleanupRequestBytes = 128 << 10 // consumer-local limit; generic Stage remains 256KiB

type cleanupWireProjection struct {
	name             string
	request          *pb.TxnRequest
	success, failure *pb.TxnResponse
}

func cleanupWirePadding(size int) string { return strings.Repeat("x", size) }

func cleanupWirePaddingDigest() string { return strings.Repeat("f", 64) }

func (b *Backend) cleanupProjection(name string, cmps []clientv3.Cmp, success []*pb.RequestOp, response *pb.TxnResponse) cleanupWireProjection {
	keys := []string{b.identityKey, b.restoreKey}
	return cleanupWireProjection{name, &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: success, Failure: taskProtoPoints(keys)}, response, taskProtoResponse(keys, []string{b.identityValue, b.restoreEpoch})}
}

// Count both branches literally, then measure the actual protobuf objects.
// Nested transactions are refused rather than hidden behind an outer node.
func validateCleanupWire(p cleanupWireProjection) error {
	if p.request == nil || p.success == nil || p.failure == nil || len(p.request.Compare)+len(p.request.Success)+len(p.request.Failure) > maxStageOperations {
		return ErrInvalidConfiguration
	}
	for _, cmp := range p.request.Compare {
		if cmp == nil || !validComparison(clientv3.Cmp(*cmp)) {
			return ErrInvalidConfiguration
		}
	}
	for _, branch := range [][]*pb.RequestOp{p.request.Success, p.request.Failure} {
		for _, op := range branch {
			if op == nil || op.GetRequestTxn() != nil || op.GetRequestPut() == nil && op.GetRequestRange() == nil && op.GetRequestDeleteRange() == nil {
				return ErrInvalidConfiguration
			}
		}
	}
	if p.request.Size() > cleanupRequestBytes || p.success.Size() > maxMutationBytes || p.failure.Size() > maxMutationBytes {
		return fmt.Errorf("%w: cleanup wire profile %s exceeds request/response limits", ErrInvalidConfiguration, p.name)
	}
	return nil
}

// Future records are pure size projections, never written or handed to an
// effect API. All 32 candidates and legal scalar widths are checked before a
// managed claim; actual dispatch is checked again with its real values.
func (b *Backend) cleanupSlotWireInventory(o *workerDueOwner, evidence *cleanupSlotEvidence, state *cleanupSlotState) ([]cleanupWireProjection, error) {
	w, err := b.cleanupEvidenceComparisons(o, evidence)
	if err != nil {
		return nil, err
	}
	if state == nil || state.record.Task != evidence.record.Task {
		return nil, ErrInvalidRecord
	}
	// Evidence comparisons include the original W followed by T/I/R.
	worker := append([]clientv3.Cmp(nil), w[:20]...)
	var inventory []cleanupWireProjection
	for candidate := uint64(0); candidate < cleanupExecutionSlots; candidate++ {
		r := evidence.record
		r.Selection = math.MaxUint64 - candidate
		r.Slot = cleanupCandidate(r.TaskDigest, r.Selection)
		projected, err := b.cleanupCandidateWire(worker, evidence, r)
		if err != nil {
			return nil, err
		}
		if inventory == nil {
			inventory = projected
			continue
		}
		for i, p := range projected {
			// The maxima may come from distinct legal branches/candidates.
			if p.request.Size() > inventory[i].request.Size() {
				inventory[i].request = p.request
			}
			if p.success.Size() > inventory[i].success.Size() {
				inventory[i].success = p.success
			}
			if p.failure.Size() > inventory[i].failure.Size() {
				inventory[i].failure = p.failure
			}
		}
	}
	return inventory, nil
}

func cleanupMaxFence(f taskFence) taskFence {
	f.create, f.mod = math.MaxInt64, math.MaxInt64
	if f.lease > 0 {
		f.lease = math.MaxInt64
	}
	return f
}

func (b *Backend) cleanupCandidateWire(worker []clientv3.Cmp, evidence *cleanupSlotEvidence, r cleanupExecutionRecord) ([]cleanupWireProjection, error) {
	eKey, err := b.namespace.cleanupExecutionKey(r.Task)
	if err != nil {
		return nil, err
	}
	sKey, err := b.namespace.cleanupExecutionSlotKey(r.Slot)
	if err != nil {
		return nil, err
	}
	initial, err := encodeCleanupExecution(evidence.record)
	if err != nil {
		return nil, err
	}
	selecting, err := encodeCleanupExecution(r)
	if err != nil {
		return nil, err
	}
	r.Phase, r.Confirmed = "occupied", "reserved"
	occupied, err := encodeCleanupExecution(r)
	if err != nil {
		return nil, err
	}
	slotValue, err := encodeCleanupExecutionSlot(cleanupSlotRecordFromExecution(r))
	if err != nil {
		return nil, err
	}
	active, joined, settled, err := cleanupFutureExecutionValues(r)
	if err != nil {
		return nil, err
	}
	recoveryOld, recoveryNext, err := cleanupFutureRecoveryValues(r)
	if err != nil {
		return nil, err
	}
	e := taskFence{key: eKey, value: active, create: math.MaxInt64, mod: math.MaxInt64}
	s := taskFence{key: sKey, value: slotValue, create: math.MaxInt64, mod: math.MaxInt64}
	tir := make([]clientv3.Cmp, 0, 12)
	for _, fence := range []taskFence{evidence.bundle.fences[0], evidence.bundle.fences[1], evidence.receipt} {
		tir = append(tir, cleanupMaxFence(fence).comparisons()...)
	}
	appendCopies := func(first []clientv3.Cmp, rest ...[]clientv3.Cmp) []clientv3.Cmp {
		out := append([]clientv3.Cmp(nil), first...)
		for _, part := range rest {
			out = append(out, part...)
		}
		return out
	}
	put := func(key, value string) []*pb.RequestOp { return []*pb.RequestOp{taskProtoPut(key, value, 0)} }
	base := b.baseComparisons()
	point := func(name string, keys, values []string) cleanupWireProjection {
		return b.cleanupProjection(name, base, taskProtoPoints(keys), taskProtoResponse(keys, values))
	}
	keys, values := make([]string, 0, 10), make([]string, 0, 10)
	for _, fence := range evidence.bundle.fences {
		keys = append(keys, fence.key)
		values = append(values, cleanupWirePadding(cleanupExecutionBytes))
	}
	claimKey, err := b.namespace.taskClaimKey(r.Task)
	if err != nil {
		return nil, err
	}
	keys = append(keys, string(evidence.bundle.checkpoint.Key), claimKey)
	values = append(values, cleanupWirePadding(cleanupExecutionBytes), cleanupWirePadding(cleanupExecutionBytes))
	terminalKeys, err := b.cleanupTerminalHistoryKeys(r)
	if err != nil {
		return nil, err
	}
	terminalValues := []string{cleanupWirePadding(cleanupExecutionBytes), cleanupWirePadding(cleanupExecutionBytes), cleanupWirePadding(cleanupExecutionBytes), cleanupWirePadding(cleanupExecutionBytes)}
	result := []cleanupWireProjection{
		point("point", []string{eKey}, []string{cleanupWirePadding(cleanupExecutionBytes)}),
		point("bundle", keys, values),
		point("observe", []string{eKey, sKey}, []string{active, slotValue}),
		point("claim-observe", []string{eKey, sKey, evidence.receipt.key}, []string{active, slotValue, evidence.receipt.value}),
		point("terminal-history", terminalKeys, terminalValues),
		b.cleanupProjection("initialize", appendCopies(worker, tir, []clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(eKey), "=", 0)}), put(eKey, initial), taskProtoPutResponse(1)),
		b.cleanupProjection("rotate", appendCopies(worker, (taskFence{key: eKey, value: selecting, create: math.MaxInt64, mod: math.MaxInt64}).comparisons()), put(eKey, selecting), taskProtoPutResponse(1)),
		b.cleanupProjection("reserve", appendCopies(worker, tir, (taskFence{key: eKey, value: selecting, create: math.MaxInt64, mod: math.MaxInt64}).comparisons(), []clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(sKey), "=", 0)}), []*pb.RequestOp{taskProtoPut(sKey, slotValue, 0), taskProtoPut(eKey, occupied, 0)}, taskProtoPutResponse(2)),
		b.cleanupProjection("progress", appendCopies(worker, e.comparisons(), s.comparisons()), put(eKey, active), taskProtoPutResponse(1)),
		b.cleanupProjection("recovery-run", appendCopies(worker, tir, (taskFence{key: eKey, value: recoveryOld, create: math.MaxInt64, mod: math.MaxInt64}).comparisons(), s.comparisons()), put(eKey, recoveryNext), taskProtoPutResponse(1)),
	}
	// The managed creation projection uses the same claim constructor and
	// creation mutation as public ClaimTask. Extra pins do not enter c.fences.
	c, checkpoint, err := b.prepareTaskClaim(context.Background(), evidence.bundle, strings.Repeat("w", 128), 24*time.Hour, "ffffffff-ffff-4fff-bfff-ffffffffffff")
	if err != nil {
		return nil, err
	}
	claimCmps := appendCopies(base, c.creationMutation(checkpoint).Comparisons, worker[4:], []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(eKey), "=", math.MaxInt64), clientv3.Compare(clientv3.ModRevision(sKey), "=", math.MaxInt64)})
	result = append(result, b.cleanupProjection("claim", claimCmps, []*pb.RequestOp{taskProtoPut(c.claimKey, c.value, math.MaxInt64), taskProtoPut(c.guardKey, c.value, math.MaxInt64)}, taskProtoPutResponse(2)))
	post := point("claim-postcheck", []string{b.identityKey}, []string{b.identityValue})
	post.request.Compare = taskProtoComparisons(appendCopies(base, c.comparisons()))
	post.request.Failure = taskProtoPoints([]string{b.identityKey})
	post.failure = taskProtoResponse([]string{b.identityKey}, []string{b.identityValue})
	result = append(result, post, b.cleanupProjection("joined", appendCopies(base, e.comparisons(), s.comparisons()), put(eKey, joined), taskProtoPutResponse(1)))
	_, _, releaseKey, err := b.namespace.localRuntimeReleaseKeys(r.Task)
	if err != nil {
		return nil, err
	}
	// The actual owner-release codec accepts 8192 bytes; the smaller terminal
	// proof limit does not bound this separate retained release record.
	release := taskFence{key: releaseKey, value: cleanupWirePadding(8192), create: math.MaxInt64, mod: math.MaxInt64}
	response := taskProtoPutResponse(1)
	response.Responses = append([]*pb.ResponseOp{{Response: &pb.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: &pb.DeleteRangeResponse{Header: response.Header, Deleted: 1}}}}, response.Responses...)
	result = append(result, b.cleanupProjection("settle", appendCopies(worker, tir, e.comparisons(), s.comparisons(), release.comparisons()), []*pb.RequestOp{{Request: &pb.RequestOp_RequestDeleteRange{RequestDeleteRange: &pb.DeleteRangeRequest{Key: []byte(sKey)}}}, taskProtoPut(eKey, settled, 0)}, response))
	for _, projection := range result {
		if err := validateCleanupWire(projection); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// Maximal canonical fields are only protobuf projections; they never escape
// inventory construction as a worker, manifest or effect-bearing permission.
func cleanupFutureExecutionRecord(r cleanupExecutionRecord) cleanupExecutionRecord {
	id := "ffffffff-ffff-4fff-bfff-ffffffffffff"
	r.Run = &cleanupExecutionRun{ID: id, Worker: launcher.WorkspaceSyncWorkerReference{Incarnation: id, BootID: id, PID: math.MaxInt32, StartTicks: math.MaxUint64, UID: math.MaxUint32, GID: math.MaxUint32, PIDNamespaceDev: math.MaxUint64, PIDNamespaceIno: math.MaxUint64, MountNamespaceDev: math.MaxUint64, MountNamespaceIno: math.MaxUint64, ExecutableDev: math.MaxUint64, ExecutableIno: math.MaxUint64}}
	r.Pending = &cleanupExecutionPending{Purpose: "owner-release", ClaimID: id, StageID: strings.Repeat("s", 128), RequestID: strings.Repeat("r", 128), AttemptID: id, Digest: cleanupWirePaddingDigest(), LeaseID: math.MaxInt64}
	// Recovery stores encode their random 32-byte IDs as 64 hex characters;
	// project that full coordinate width independently of UUID-shaped run IDs.
	r.Archive = &cleanupExecutionArchive{StoreID: cleanupWirePaddingDigest(), Slot: 63, BindingDigest: cleanupWirePaddingDigest()}
	r.Confirmed = "owner-released"
	return r
}

func cleanupFutureExecutionValues(r cleanupExecutionRecord) (active, joined, settled string, err error) {
	r = cleanupFutureExecutionRecord(r)
	active, err = encodeCleanupExecution(r)
	if err != nil {
		return
	}
	r.Run.Joined = true
	joined, err = encodeCleanupExecution(r)
	if err != nil {
		return
	}
	r.Pending = nil
	r.Phase, r.Confirmed = "settled", "settled"
	r.Release = &cleanupExecutionRelease{CreateRevision: math.MaxInt64, Digest: cleanupWirePaddingDigest()}
	settled, err = encodeCleanupExecution(r)
	return
}

func cleanupFutureRecoveryValues(r cleanupExecutionRecord) (prior, next string, err error) {
	r = cleanupFutureExecutionRecord(r)
	r.Confirmed, r.Pending, r.Run.Joined = "archive-bound", nil, true
	prior, err = encodeCleanupExecution(r)
	if err != nil {
		return
	}
	// The real transition preserves archive coordinates and occupation, but
	// installs a distinct current Run and its single pending Claim candidate.
	id := "eeeeeeee-eeee-4eee-beee-eeeeeeeeeeee"
	r.Run.ID, r.Run.Worker.Incarnation, r.Run.Joined = id, id, false
	r.Pending = &cleanupExecutionPending{Purpose: "claim", ClaimID: id}
	next, err = encodeCleanupExecution(r)
	return
}
