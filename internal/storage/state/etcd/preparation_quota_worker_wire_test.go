package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const (
	coldQuotaWorkerRequestBytes = 128 << 10
	coldQuotaWorkerRangeBytes   = 262144
)

// coldQuotaWorkerWireBounds distinguishes schema-derived canonical bounds from
// codec hard caps. It contains sizing inputs only, never authenticated history
// or a capability that may be passed to the continuation.
type coldQuotaWorkerWireBounds struct {
	backend  *Backend
	prepared *coldQuotaReturnPrepared
	ready    *mvccpb.KeyValue
	slot     *mvccpb.KeyValue
	values   map[string]int
}

// Small fixed records cannot legally contain arbitrary 16KiB payloads. Every
// variable string below is an ASCII domain segment (128 bytes), lowercase digest
// (64), canonical UUID (36), or canonical namespace (512). JSON therefore has no
// escaping expansion for these types. Positive signed and unsigned fields use
// their full decimal widths; fixed phases and stage names come from the actual
// validators. Complex publication/release/refund records retain their production
// hard caps instead: their opaque fields must not be bounded by this ASCII rule.
func coldQuotaWorkerCanonicalBounds(t *testing.T) coldQuotaWorkerWireBounds {
	t.Helper()
	n, err := NewNamespace("/"+strings.Repeat("p", 252), strings.Repeat("s", 128), strings.Repeat("c", 128))
	require.NoError(t, err)
	require.Len(t, n.Root(), maxNamespaceRootBytes)

	b := &Backend{namespace: n, restoreEpoch: strings.Repeat("e", 128), preparationDigest: strings.Repeat("d", 64), preparationInventory: &PreparationInventoryOptions{}, preparationTotal: 256*1000000 + 32}
	for i := range b.preparationInventory.QuotaSlots {
		b.preparationInventory.QuotaSlots[i] = 1000000
	}
	b.identityKey, err = n.Key("identity")
	require.NoError(t, err)
	b.restoreKey, err = n.Key("restore-epoch")
	require.NoError(t, err)
	identity, err := json.Marshal(Identity{SchemaVersion: 1, Prefix: n.prefix, AuthorityID: n.scope, Cell: n.cell, ClusterID: math.MaxUint64, StorageID: strings.Repeat("s", 128), RuntimeID: strings.Repeat("r", 128)})
	require.NoError(t, err)
	// Identity accepts original JSON whitespace, so its real 4096-byte cap is
	// attainable even though all structured fields themselves are bounded.
	b.identityValue = string(identity) + strings.Repeat(" ", maxIdentityJSONBytes-len(identity))
	_, err = decodeIdentity(b.identityValue, n)
	require.NoError(t, err)

	id := "ffffffff-ffff-4fff-bfff-ffffffffffff"
	owner := ColdSlotOwner{IntentID: strings.Repeat("i", 128), SandboxID: strings.Repeat("s", 128), WorkspaceHash: strings.Repeat("f", 64), RequestID: strings.Repeat("r", 128), Generation: math.MaxInt64, Partition: 255}
	quota := ColdSlotReference{Kind: "quota", Partition: 255, ID: 999999, Epoch: math.MaxUint64}
	preparation := ColdSlotReference{Kind: "preparation", ID: 31, Epoch: math.MaxUint64}
	locator := StageAttemptLocator{Namespace: n.Root(), RestoreEpoch: b.restoreEpoch, Partition: 255, RequestID: owner.RequestID, StageID: "cold_runtime_quota_return", AttemptID: id}
	require.NoError(t, locator.Validate())
	settlement, publication := locator, locator
	settlement.StageID, publication.StageID = "cold_preparation_settle", enrolledPublicationStage
	reservation := ColdPreparationRecord{Version: 1, RestoreEpoch: b.restoreEpoch, ProfileDigest: b.preparationDigest, Owner: owner, Quota: quota, Preparation: preparation, Phase: "settled", Settlement: settlement}
	require.True(t, b.validColdReservation(reservation, WorkspaceIdentity{hash: owner.WorkspaceHash, partition: 255}, owner.IntentID))
	ready := preparationInventoryRecord{Version: 1, RestoreEpoch: b.restoreEpoch, ProfileDigest: b.preparationDigest, Next: b.preparationTotal, Total: b.preparationTotal, Ready: true}
	require.True(t, b.validColdInventory(ready))
	slot := coldSlotRecord{Version: 1, RestoreEpoch: b.restoreEpoch, ProfileDigest: b.preparationDigest, Slot: quota, Occupied: true, Owner: owner}
	require.True(t, b.validColdSlot(slot))
	work := coldPreparationWork{Version: 1, RestoreEpoch: b.restoreEpoch, ProfileDigest: b.preparationDigest, IntentID: owner.IntentID, Phase: "settled"}

	encode := func(value any) string {
		wire, err := coldEncode(value)
		require.NoError(t, err)
		return wire
	}
	point := func(name, value string, create, mod int64) *mvccpb.KeyValue {
		key, err := n.Key("wire-sizing", name)
		require.NoError(t, err)
		return &mvccpb.KeyValue{Key: []byte(key), Value: []byte(value), Version: 1, CreateRevision: create, ModRevision: mod}
	}
	publicationRef := publication.reference(strings.Repeat("a", 64))
	settlementRef := settlement.reference(strings.Repeat("b", 64))
	publicationReceipt, err := encodeReceipt(publicationRef, OutcomeCommitted)
	require.NoError(t, err)
	settlementReceipt, err := encodeReceipt(settlementRef, OutcomeCommitted)
	require.NoError(t, err)

	// A valid codec fixture supplies relationships needed by the real refund
	// encoder. It is not used to establish the complex record size bounds.
	r := quotaReturnRecordFixture(t)
	r.Release.Task.Namespace, r.Release.Task.RestoreEpoch = n.Root(), b.restoreEpoch
	r.Release.Task.SandboxID, r.Release.Task.Partition = owner.SandboxID, owner.Partition
	r.Release.Claim.Task = r.Release.Task
	r.Release.WorkspaceHash, r.Release.Generation = owner.WorkspaceHash, owner.Generation
	_, err = encodeLocalOwnerRelease(r.Release)
	require.NoError(t, err)
	h := &coldQuotaReturnHistory{
		reservation: reservation,
		publication: &coldPublicationHistory{
			entry:   &RuntimeEnrolledPublicationEntry{Record: RuntimeEnrolledPublicationRecord{RuntimePublicationRecord: RuntimePublicationRecord{Runtime: r.Runtime, DataGateEpoch: math.MaxInt64}, NodeUID: r.NodeUID, NodeBootID: r.NodeBootID}, Reference: publicationRef},
			journal: point("publication", strings.Repeat("x", 8192), math.MaxInt64-20, math.MaxInt64-20),
			receipt: point("publication-receipt", publicationReceipt, math.MaxInt64-20, math.MaxInt64-20),
		},
		release:    &localOwnerReleaseProof{record: r.Release, kv: point("release", strings.Repeat("x", 8192), math.MaxInt64-6, math.MaxInt64-6)},
		settlement: settlementRef,
		reservedKV: point("reservation", encode(reservation), math.MaxInt64-22, math.MaxInt64-18),
		workKV:     point("work", encode(work), math.MaxInt64-22, math.MaxInt64-18),
		settledKV:  point("settlement-receipt", settlementReceipt, math.MaxInt64-18, math.MaxInt64-18),
	}
	readyKV := point("ready", encode(ready), math.MaxInt64-21, math.MaxInt64-21)
	slotKey, freeKey, err := n.coldSlotKeys(quota)
	require.NoError(t, err)
	slotKV := point("slot", encode(slot), math.MaxInt64-22, math.MaxInt64-18)
	slotKV.Key = []byte(slotKey)
	returnKey, err := n.coldRuntimeQuotaReturnKey(owner.Partition, owner.IntentID)
	require.NoError(t, err)
	mutation, err := b.coldQuotaReturnMutation(h, readyKV, slotKV, returnKey, freeKey, locator)
	require.NoError(t, err)
	mutation, digest, err := prepareMutation(n, mutation)
	require.NoError(t, err)
	prepared := &coldQuotaReturnPrepared{backend: b, history: h, mutation: mutation, reference: locator.reference(digest), returnKey: returnKey}
	journal, err := encodeColdRuntimeQuotaAttempt(b.coldQuotaAttemptRecord(prepared))
	require.NoError(t, err, "the full legal namespace/owner/locator dimensions must still fit the journal schema")

	values := map[string]int{"ready": len(readyKV.Value), "reservation": len(h.reservedKV.Value), "work": len(h.workKV.Value), "occupied-slot": len(slotKV.Value), "free-slot": len(mutation.Writes[0].Value), "publication-receipt": len(publicationReceipt), "settlement-receipt": len(settlementReceipt), "journal-canonical-example": len(journal), "publication-hard-cap": 8192, "release-hard-cap": 8192, "return-hard-cap": 16384, "journal-hard-cap": coldQuotaAttemptMaxBytes}
	return coldQuotaWorkerWireBounds{backend: b, prepared: prepared, ready: readyKV, slot: slotKV, values: values}
}

// Keys are independently widened to 1024 bytes after the production builders
// have validated the protocol shape. This is a conservative protobuf envelope,
// not a claim that every fixed key family can attain 1024 simultaneously. No
// widened request is submitted or used to recalculate an authority digest.
func coldQuotaWorkerWidenKeys(p cleanupWireProjection) cleanupWireProjection {
	key := []byte(strings.Repeat("k", maxKeyBytes))
	for _, comparison := range p.request.Compare {
		comparison.Key = key
	}
	for _, branch := range [][]*pb.RequestOp{p.request.Success, p.request.Failure} {
		for _, operation := range branch {
			if put := operation.GetRequestPut(); put != nil {
				put.Key = key
			}
			if point := operation.GetRequestRange(); point != nil {
				point.Key = key
			}
		}
	}
	for _, response := range []*pb.TxnResponse{p.success, p.failure} {
		for _, operation := range response.Responses {
			if point := operation.GetResponseRange(); point != nil {
				for _, kv := range point.Kvs {
					kv.Key = key
				}
			}
		}
	}
	return p
}

func coldQuotaWorkerRequireProjection(t *testing.T, p cleanupWireProjection, nodes int) {
	t.Helper()
	require.Equal(t, nodes, len(p.request.Compare)+len(p.request.Success)+len(p.request.Failure), p.name)
	require.LessOrEqual(t, nodes, 64, p.name)
	require.LessOrEqual(t, p.request.Size(), coldQuotaWorkerRequestBytes, "%s: 1024-byte keys plus canonical fixed records and complex codec caps", p.name)
	require.LessOrEqual(t, p.success.Size(), coldQuotaWorkerRangeBytes, p.name)
	require.LessOrEqual(t, p.failure.Size(), coldQuotaWorkerRangeBytes, p.name)
	t.Logf("schema envelope %s nodes=%d request=%d success_response=%d failure_response=%d", p.name, nodes, p.request.Size(), p.success.Size(), p.failure.Size())
}

// This is the original public body, with no W fence or extra comparisons:
// fresh journal 30/1/3, replacement 37/1/3, refund 34/4/4. Hard-cap substitutions
// are sizing-only bounds on already-authenticated values, not decodable history.
// In particular, these byte bounds do not establish a future worker adapter's
// W24 admission/guard shape; adding that shape requires its own complete bound.
func coldQuotaWorkerProjections(t *testing.T) (coldQuotaWorkerWireBounds, []cleanupWireProjection, []cleanupWireProjection) {
	t.Helper()
	bounds := coldQuotaWorkerCanonicalBounds(t)
	b, prepared := bounds.backend, bounds.prepared
	journalKey, err := b.namespace.coldRuntimeQuotaAttemptKey(255, prepared.history.reservation.Owner.IntentID)
	require.NoError(t, err)
	journalWire, err := encodeColdRuntimeQuotaAttempt(b.coldQuotaAttemptRecord(prepared))
	require.NoError(t, err)
	oldRef := prepared.reference
	oldRef.AttemptID = "eeeeeeee-eeee-4eee-beee-eeeeeeeeeeee"
	old := b.coldQuotaAttemptRecord(prepared)
	old.Reference = oldRef
	oldWire, err := encodeColdRuntimeQuotaAttempt(old)
	require.NoError(t, err)
	previous := &mvccpb.KeyValue{Key: []byte(journalKey), Value: []byte(oldWire), Version: 2, CreateRevision: math.MaxInt64 - 5, ModRevision: math.MaxInt64 - 4}
	_, abortedKey, err := b.stageKeys(oldRef)
	require.NoError(t, err)
	abortedWire, err := encodeReceipt(oldRef, OutcomeAborted)
	require.NoError(t, err)
	aborted := &mvccpb.KeyValue{Key: []byte(abortedKey), Value: []byte(abortedWire), Version: 1, CreateRevision: math.MaxInt64 - 3, ModRevision: math.MaxInt64 - 3}
	var journals []cleanupWireProjection
	for _, replacement := range []bool{false, true} {
		var comparisons []clientv3.Cmp
		if replacement {
			comparisons, err = b.coldQuotaAttemptComparisons(prepared, journalKey, previous, aborted)
		} else {
			comparisons, err = b.coldQuotaAttemptComparisons(prepared, journalKey, nil, nil)
		}
		require.NoError(t, err)
		keys := []string{b.identityKey, b.restoreKey, journalKey}
		request := &pb.TxnRequest{Compare: taskProtoComparisons(comparisons), Success: taskProtoWrites([]Write{{Key: journalKey, Value: []byte(journalWire)}}), Failure: taskProtoPoints(keys)}
		// The exact production encoding above is admitted before replacing its
		// value by the larger, unconditional codec ceiling for sizing.
		request.Success[0].GetRequestPut().Value = []byte(strings.Repeat("x", coldQuotaAttemptMaxBytes))
		journals = append(journals, coldQuotaWorkerWidenKeys(cleanupWireProjection{fmt.Sprintf("journal-replacement-%t", replacement), request, taskProtoPutResponse(1), taskProtoResponse(keys, []string{b.identityValue, b.restoreEpoch, strings.Repeat("x", coldQuotaAttemptMaxBytes)})}))
	}

	mutation := prepared.mutation
	mutation.Writes = append([]Write(nil), mutation.Writes...)
	mutation.Writes[2].Value = []byte(strings.Repeat("x", 16384))
	guardKey, receiptKey, err := b.stageKeys(prepared.reference)
	require.NoError(t, err)
	committed, err := encodeReceipt(prepared.reference, OutcomeCommitted)
	require.NoError(t, err)
	stages, err := b.taskStageWireProjections(prepared.reference, mutation, guardKey, receiptKey, committed, nil)
	require.NoError(t, err)
	// Cross-check the consumer's real refund projection before the sizing
	// keys are widened. This catches accidental changes to either producer.
	stage := &Stage{mutation: mutation, guardKey: guardKey, guardValue: string(stages[0].request.Success[0].GetRequestPut().Value), guardRevision: math.MaxInt64, leaseID: clientv3.LeaseID(math.MaxInt64), receiptKey: receiptKey, committedValue: committed}
	refund, _, err := b.coldQuotaReturnProjection(stage)
	require.NoError(t, err)
	require.Equal(t, refund, stages[1].request)
	for i := range stages {
		stages[i] = coldQuotaWorkerWidenKeys(stages[i])
	}
	return bounds, journals, stages
}

func TestColdQuotaWorkerWireCanonicalEnvelope(t *testing.T) {
	bounds, journals, stages := coldQuotaWorkerProjections(t)
	for name, size := range bounds.values {
		t.Logf("canonical/cap source %s=%d", name, size)
	}
	coldQuotaWorkerRequireProjection(t, journals[0], 34)
	coldQuotaWorkerRequireProjection(t, journals[1], 41)
	for i, nodes := range []int{9, 42, 9} {
		coldQuotaWorkerRequireProjection(t, stages[i], nodes)
	}
	// Every failure branch includes identity and restore, while the refund
	// also carries its original receipt and leased guard. Maximal protobuf
	// scalar widths include KV Lease even for a reply that will be refused.
	require.Len(t, stages[1].failure.Responses, 4)
	for _, response := range stages[1].failure.Responses {
		require.Equal(t, int64(math.MaxInt64), response.GetResponseRange().Kvs[0].Lease)
	}
}

// The production worker builder includes base identity/restore plus profile,
// session, partition and one budget share: 4+4*4=20 comparisons. Adding the other
// original share gives W24. This is a node-count counterexample only, not a
// chosen private guard protocol or a maximum-byte proof for a future adapter.
// In particular, blindly appending W24 to the unchanged refund body is illegal.
func TestColdQuotaWorkerWireW24CannotBeBlindlyAppended(t *testing.T) {
	b, _ := workerTestBackend(t)
	ctx := context.Background()
	session, err := b.OpenWorkerSession(ctx, "quota-wire-dimensions")
	require.NoError(t, err)
	partition, err := b.ClaimWorkerPartition(ctx, session, 255)
	require.NoError(t, err)
	ranges, err := b.AcquireWorkerBudget(ctx, session, WorkerBudgetRange, 0)
	require.NoError(t, err)
	claims, err := b.AcquireWorkerBudget(ctx, session, WorkerBudgetClaim, 0)
	require.NoError(t, err)

	b.archiveLife.mu.Lock()
	worker, err := b.workerComparisons(session, partition, ranges)
	otherShare := claims.fence.comparisons()
	b.archiveLife.mu.Unlock()
	require.NoError(t, err)
	require.Len(t, worker, 20)
	require.Len(t, otherShare, 4)
	worker = append(worker, otherShare...)
	require.Len(t, worker, 24)

	_, journals, stages := coldQuotaWorkerProjections(t)
	for _, body := range append(journals, stages[1]) {
		publicNodes := len(body.request.Compare) + len(body.request.Success) + len(body.request.Failure)
		blindAppendNodes := publicNodes + len(worker)
		t.Logf("public body %s nodes=%d; blind W24 append nodes=%d; no worker adapter byte proof asserted", body.name, publicNodes, blindAppendNodes)
		if body.name == "commit" {
			require.Equal(t, 66, blindAppendNodes)
			require.Greater(t, blindAppendNodes, maxStageOperations, "the future worker guard cannot be hidden inside the public body budget")
		}
	}
}

// The four mutation RPCs remain one cohort: pausing for credit between Grant
// and guard/Commit would change the original ten-second lease protocol. Each
// cut includes all preceding successful ACKs, the terminal failed ACK, and the
// cleanup Revoke when the Grant identity is known. Transport loss consumes the
// same reservation; it does not refund paid credit or cause another Grant.
func TestColdQuotaWorkerWireMutationCohortPrefixes(t *testing.T) {
	_, journals, stages := coldQuotaWorkerProjections(t)
	header := taskProtoResponse(nil, nil).Header
	grantRequest := &pb.LeaseGrantRequest{TTL: 10}
	grantSuccess := &pb.LeaseGrantResponse{Header: header, ID: math.MaxInt64, TTL: math.MaxInt64}
	revokeRequest := &pb.LeaseRevokeRequest{ID: math.MaxInt64}
	revokeResponse := &pb.LeaseRevokeResponse{Header: header}
	for _, native := range []struct {
		name              string
		request, response int
	}{{"Grant", grantRequest.Size(), grantSuccess.Size()}, {"Revoke", revokeRequest.Size(), revokeResponse.Size()}} {
		require.LessOrEqual(t, native.request, coldQuotaWorkerRequestBytes)
		require.LessOrEqual(t, native.response, coldQuotaWorkerRangeBytes)
		t.Logf("native %s request=%d successful_response=%d", native.name, native.request, native.response)
	}

	// These are validated protocol ACK envelopes. The separate negative test
	// below records why arbitrary Grant.Error is not covered by this bound.
	paths := []struct {
		name                string
		requests, responses []int
	}{
		{"grant-no-identity-or-lost-ack", []int{grantRequest.Size()}, []int{grantSuccess.Size()}},
		{"grant-trusted-invalid-ttl-cleanup", []int{grantRequest.Size(), revokeRequest.Size()}, []int{grantSuccess.Size(), revokeResponse.Size()}},
		{"guard-failure-cleanup", []int{grantRequest.Size(), stages[0].request.Size(), revokeRequest.Size()}, []int{grantSuccess.Size(), stages[0].failure.Size(), revokeResponse.Size()}},
		{"commit-failure-cleanup", []int{grantRequest.Size(), stages[0].request.Size(), stages[1].request.Size(), revokeRequest.Size()}, []int{grantSuccess.Size(), stages[0].success.Size(), stages[1].failure.Size(), revokeResponse.Size()}},
		{"commit-success-release-or-lost-revoke-ack", []int{grantRequest.Size(), stages[0].request.Size(), stages[1].request.Size(), revokeRequest.Size()}, []int{grantSuccess.Size(), stages[0].success.Size(), stages[1].success.Size(), revokeResponse.Size()}},
	}
	for _, path := range paths {
		requestBytes, responseBytes := 0, 0
		for _, size := range path.requests {
			requestBytes += size
		}
		for _, size := range path.responses {
			responseBytes += size
		}
		require.LessOrEqual(t, len(path.requests), 4, path.name)
		require.LessOrEqual(t, responseBytes, coldQuotaWorkerRangeBytes, path.name)
		require.LessOrEqual(t, requestBytes+responseBytes, coldQuotaWorkerRangeBytes, path.name)
		t.Logf("four-RPC cohort %s rpcs=%d response_prefix_plus_failure_cleanup=%d bidirectional=%d reserved_range=%d reserved_claim=1", path.name, len(path.requests), responseBytes, requestBytes+responseBytes, coldQuotaWorkerRangeBytes)
	}
	for _, p := range append(journals, stages[2]) {
		worst := max(p.success.Size(), p.failure.Size())
		require.LessOrEqual(t, p.request.Size()+worst, coldQuotaWorkerRangeBytes)
		t.Logf("single-RPC mutation cohort %s responses=%d bidirectional=%d reserved_range=%d reserved_claim=1", p.name, worst, p.request.Size()+worst, coldQuotaWorkerRangeBytes)
	}
}

// A protobuf Grant error string has no canonical schema limit, unlike the
// success case which must have Error == "". This executable counterexample is
// deliberately outside the valid-ACK envelope: a blanket claim that every
// possible failure response fits one paid Range would be false. No transport
// cap, adapter refusal, or automatic worker implementation is invented here.
func TestColdQuotaWorkerWireGrantErrorOutsideCanonicalFrame(t *testing.T) {
	reply := &pb.LeaseGrantResponse{Header: taskProtoResponse(nil, nil).Header, ID: math.MaxInt64, TTL: math.MaxInt64, Error: strings.Repeat("x", coldQuotaWorkerRangeBytes)}
	require.Greater(t, reply.Size(), coldQuotaWorkerRangeBytes)
	t.Logf("RED outside canonical ACK frame: native LeaseGrantResponse.Error has no field limit; error_bytes=%d response=%d before cleanup Revoke", len(reply.Error), reply.Size())
}

// Each actual metadata call is a separate cohort with exactly one Txn. The
// arrays mirror loadColdPublicationHistory, readColdQuotaReturnHistory,
// readLocalOwnerReleaseProof/readLocalRuntimeHistory, fresh preparation and
// post-commit/arbitration readback. Their codec caps include corrupt-but-bounded
// records that must be refused, rather than only the expected successful value.
func TestColdQuotaWorkerWireMetadataCohorts(t *testing.T) {
	cohorts := []struct {
		name string
		caps []int
	}{
		{"publication-locate", []int{8192, 16384, 8192, 8192}},
		{"publication-history", []int{8192, 16384, 8192, 8192, 4096, 4096, 65536, 4096, 4096, 8192, 4096, 4096, 4096}},
		{"settled-reservation", []int{16384, 16384}},
		{"settlement-receipt", []int{4096}},
		{"owner-release", []int{8192}},
		{"termination-locate", []int{16384, 16384}},
		{"termination-history", []int{4096, 8192, 32768, 4096, 8192, 8192, 8192, maxRecordBytes, 8192}},
		{"return-replay-or-postcommit", []int{16384}},
		{"pending-attempt", []int{coldQuotaAttemptMaxBytes}},
		{"quota-slot-and-free", []int{16384, 16384}},
		{"aborted-receipt-reread", []int{coldQuotaAttemptMaxBytes}},
		{"return-receipt", []int{4096}},
	}
	for _, cohort := range cohorts {
		t.Run(cohort.name, func(t *testing.T) {
			b, server := workerTestBackend(t)
			keys, values := []string{b.identityKey, b.restoreKey}, []string{strings.Repeat("i", maxIdentityJSONBytes), strings.Repeat("e", 128)}
			var points []string
			for i, cap := range cohort.caps {
				suffix := fmt.Sprintf("%02d", i)
				key := b.namespace.Root() + strings.Repeat("k", maxKeyBytes-len(b.namespace.Root())-len(suffix)) + suffix
				points = append(points, key)
				value := strings.Repeat("x", cap)
				server.put(key, value, 0)
				keys, values = append(keys, key), append(values, value)
			}
			// New performs identity and worker-profile Txns before this call.
			// Snapshot the boundary rather than clearing history or discarding
			// any target requests; every native call after it remains counted.
			server.mu.Lock()
			start := len(server.requests)
			server.mu.Unlock()
			_, err := b.readEnrolledPoints(context.Background(), points, cohort.caps)
			require.NoError(t, err)
			server.mu.Lock()
			requests := append([]*pb.TxnRequest(nil), server.requests[start:]...)
			server.mu.Unlock()
			require.Len(t, requests, 1, "one metadata cohort must not hide multiple native calls")
			// The actual reader above confirms the one-RPC shape; replace only
			// dimensions when measuring the independent maximum envelope.
			request := requests[0]
			for _, c := range request.Compare {
				if c.Target == pb.Compare_VALUE {
					if string(c.Key) == b.identityKey {
						c.TargetUnion = &pb.Compare_Value{Value: []byte(values[0])}
					} else {
						c.TargetUnion = &pb.Compare_Value{Value: []byte(values[1])}
					}
				}
			}
			p := coldQuotaWorkerWidenKeys(cleanupWireProjection{cohort.name, request, taskProtoResponse(keys, values), taskProtoResponse(keys, values)})
			coldQuotaWorkerRequireProjection(t, p, 4+2*len(keys))
			worst := max(p.success.Size(), p.failure.Size())
			require.LessOrEqual(t, p.request.Size()+worst, coldQuotaWorkerRangeBytes)
			t.Logf("single-RPC metadata cohort response=%d bidirectional=%d reserved_range=%d reserved_claim=1", worst, p.request.Size()+worst, coldQuotaWorkerRangeBytes)
		})
	}
	t.Run("inventory-ready-domain-read", func(t *testing.T) {
		bounds := coldQuotaWorkerCanonicalBounds(t)
		b, server := workerTestBackend(t)
		key, err := b.namespace.Key("preparation", "inventory", "ready")
		require.NoError(t, err)
		server.put(key, string(bounds.ready.Value), 0)
		server.mu.Lock()
		start := len(server.requests)
		server.mu.Unlock()
		_, err = b.readDomain(context.Background(), key)
		require.NoError(t, err)
		server.mu.Lock()
		requests := append([]*pb.TxnRequest(nil), server.requests[start:]...)
		server.mu.Unlock()
		require.Len(t, requests, 1)
		require.Len(t, requests[0].Compare, 4)
		require.Len(t, requests[0].Success, 1)
		require.Len(t, requests[0].Failure, 2)
		// coldReady alone uses readDomain: its success omits the identity
		// points that appear in both branches of readEnrolledPoints.
		maximum := bounds.backend
		request := &pb.TxnRequest{Compare: taskProtoComparisons(maximum.baseComparisons()), Success: taskProtoPoints([]string{key}), Failure: taskProtoPoints([]string{maximum.identityKey, maximum.restoreKey})}
		p := coldQuotaWorkerWidenKeys(cleanupWireProjection{"inventory-ready-domain-read", request, taskProtoResponse([]string{key}, []string{strings.Repeat("x", 16384)}), taskProtoResponse([]string{maximum.identityKey, maximum.restoreKey}, []string{maximum.identityValue, maximum.restoreEpoch})})
		coldQuotaWorkerRequireProjection(t, p, 7)
		require.LessOrEqual(t, p.request.Size()+max(p.success.Size(), p.failure.Size()), coldQuotaWorkerRangeBytes)
		t.Logf("single-RPC metadata cohort reserved_range=%d reserved_claim=1", coldQuotaWorkerRangeBytes)
	})
}

type coldQuotaWorkerObservedRPC struct {
	name              string
	request, response int
}

// The existing host protocol model starts with decoded history. These traces
// observe real client protobuf calls through the original continuation only;
// they establish neither public history authentication nor physical release.
func TestColdQuotaWorkerWireModelPrefixes(t *testing.T) {
	for _, failure := range []string{"none", "journal", "Grant", "guard", "refund", "return-read", "receipt-read", "Revoke"} {
		t.Run(failure, func(t *testing.T) {
			b, server, owner, prepared := coldQuotaPreparedModel(t)
			journalKey := coldQuotaJournalKey(t, b, prepared)
			_, receiptKey, err := b.stageKeys(prepared.reference)
			require.NoError(t, err)
			var mu sync.Mutex
			var trace []coldQuotaWorkerObservedRPC
			record := func(name string, request, response int) {
				mu.Lock()
				defer mu.Unlock()
				trace = append(trace, coldQuotaWorkerObservedRPC{name, request, response})
			}
			server.txnReply = func(request *pb.TxnRequest, response *pb.TxnResponse) {
				name := ""
				for _, op := range request.Success {
					if put := op.GetRequestPut(); put != nil {
						switch {
						case string(put.Key) == journalKey:
							name = "journal"
						case put.Lease != 0:
							name = "guard"
						case len(request.Success) == 4:
							name = "refund"
						}
					}
					if point := op.GetRequestRange(); point != nil {
						if string(point.Key) == prepared.returnKey {
							name = "return-read"
						}
						if string(point.Key) == receiptKey {
							name = "receipt-read"
						}
					}
				}
				if name == failure {
					response.Header = nil
				}
				record(name, request.Size(), response.Size())
			}
			lease := b.client.Lease
			b.client.Lease = &workerTestLease{Lease: lease,
				grant: func(ctx context.Context, ttl int64) (*clientv3.LeaseGrantResponse, error) {
					response, err := lease.Grant(ctx, ttl)
					if response != nil {
						wire := &pb.LeaseGrantResponse{Header: response.ResponseHeader, ID: int64(response.ID), TTL: response.TTL, Error: response.Error}
						record("Grant", (&pb.LeaseGrantRequest{TTL: ttl}).Size(), wire.Size())
						if failure == "Grant" {
							response.TTL = 0 // trusted identity still requires cleanup
						}
					}
					return response, err
				},
				revoke: func(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
					response, err := lease.Revoke(ctx, id)
					if response != nil {
						record("Revoke", (&pb.LeaseRevokeRequest{ID: int64(id)}).Size(), (*pb.LeaseRevokeResponse)(response).Size())
					}
					if failure == "Revoke" {
						return nil, errors.New("model lost original Revoke acknowledgement")
					}
					return response, err
				},
			}
			result, err := b.journalAndContinueColdRuntimeQuotaReturn(owner, prepared, nil, nil)
			if failure == "none" {
				require.NoError(t, err)
				require.Equal(t, OutcomeCommitted, result.Outcome)
			} else {
				require.Error(t, err)
			}
			mu.Lock()
			observed := append([]coldQuotaWorkerObservedRPC(nil), trace...)
			mu.Unlock()
			var names []string
			mutationRPCs, mutationResponses, mutationBoth := 0, 0, 0
			for _, rpc := range observed {
				names = append(names, rpc.name)
				require.NotEmpty(t, rpc.name, "new calls require an explicit cohort assignment")
				require.LessOrEqual(t, rpc.request, coldQuotaWorkerRequestBytes)
				require.LessOrEqual(t, rpc.response, coldQuotaWorkerRangeBytes)
				if rpc.name == "Grant" || rpc.name == "guard" || rpc.name == "refund" || rpc.name == "Revoke" {
					mutationRPCs++
					mutationResponses += rpc.response
					mutationBoth += rpc.request + rpc.response
				}
				t.Logf("model actual %s request=%d response=%d", rpc.name, rpc.request, rpc.response)
			}
			want := []string{"journal", "Grant", "guard", "refund", "return-read", "receipt-read", "Revoke"}
			if failure == "journal" {
				want = want[:1]
			} else if failure != "none" && failure != "Revoke" {
				for i, name := range want {
					if name == failure {
						want = append(append([]string(nil), want[:i+1]...), "Revoke")
						break
					}
				}
			}
			require.Equal(t, want, names)
			require.LessOrEqual(t, mutationRPCs, 4)
			require.LessOrEqual(t, mutationResponses, coldQuotaWorkerRangeBytes)
			require.LessOrEqual(t, mutationBoth, coldQuotaWorkerRangeBytes)
			t.Logf("model actual mutation cohort rpcs=%d responses=%d bidirectional=%d; journal and each proof read remain separate paid cohorts", mutationRPCs, mutationResponses, mutationBoth)
		})
	}
}

// Resolve is a separate one-RPC mutation cohort. Its authenticated receipt or
// return readback belongs to later single-RPC metadata cohorts and never grants
// permission to reconstruct the original Stage or charge a hidden native Grant.
func TestColdQuotaWorkerWireModelResolveCohorts(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed-%t", committed), func(t *testing.T) {
			b, server, owner, prepared := coldQuotaPreparedModel(t)
			if committed {
				_, err := b.journalAndContinueColdRuntimeQuotaReturn(owner, prepared, nil, nil)
				require.NoError(t, err)
			}
			journal := coldQuotaJournalInstall(t, b, server, prepared)
			grants, _, revokes := server.counts()
			server.mu.Lock()
			server.requests = nil
			server.mu.Unlock()
			result, receipt, err := b.resolveColdRuntimeQuotaAttempt(owner, prepared.history, journal)
			require.NoError(t, err)
			if committed {
				require.Equal(t, OutcomeCommitted, result.Outcome)
				require.Nil(t, receipt)
			} else {
				require.Equal(t, OutcomeAborted, result.Outcome)
				require.NotNil(t, receipt)
			}
			server.mu.Lock()
			requests := append([]*pb.TxnRequest(nil), server.requests...)
			server.mu.Unlock()
			want := 2 // Resolve followed by canonical aborted receipt reread.
			if committed {
				want = 3 // Resolve, immutable refund, immutable committed receipt.
			}
			require.Len(t, requests, want)
			require.Len(t, requests[0].Compare, 5)
			require.Len(t, requests[0].Success, 1)
			require.Len(t, requests[0].Failure, 3)
			for i, request := range requests {
				require.LessOrEqual(t, request.Size(), coldQuotaWorkerRequestBytes)
				t.Logf("model actual Resolve/read cohort %d rpcs=1 request=%d reserved_range=%d reserved_claim=1", i, request.Size(), coldQuotaWorkerRangeBytes)
			}
			afterGrants, _, afterRevokes := server.counts()
			require.Equal(t, grants, afterGrants)
			require.Equal(t, revokes, afterRevokes)
		})
	}
}
