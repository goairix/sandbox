package etcd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This fixture supplies already-decoded codec history to the private
// continuation. Its bounded gRPC server exercises metadata transactions only;
// it does not establish authenticated release history or physical runtime death.
func coldQuotaPreparedModel(t *testing.T) (*Backend, *workerTestServer, *runtimeEnrolledPublicationOwner, *coldQuotaReturnPrepared) {
	t.Helper()
	b, server := workerTestBackend(t)
	b.enrolledPublication = &EnrolledPublicationOptions{MaxActive: 4}
	b.preparationInventory = &PreparationInventoryOptions{}
	b.preparationInventory.QuotaSlots[7] = 1
	b.preparationDigest = strings.Repeat("b", 64)
	o, err := b.registerEnrolledPublication(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		select {
		case <-o.done:
		default:
			o.finish()
		}
	})

	r := quotaReturnRecordFixture(t)
	r.Namespace, r.RestoreEpoch = b.namespace.Root(), b.restoreEpoch
	r.Attempt.Namespace, r.Attempt.RestoreEpoch = r.Namespace, r.RestoreEpoch
	r.Publication.Namespace, r.Publication.RestoreEpoch = r.Namespace, r.RestoreEpoch
	r.PreparationSettlement.Namespace, r.PreparationSettlement.RestoreEpoch = r.Namespace, r.RestoreEpoch
	r.Release.Task.Namespace, r.Release.Task.RestoreEpoch = r.Namespace, r.RestoreEpoch
	r.Release.Claim.Task = r.Release.Task
	point := func(name, wire string, create, mod int64) *mvccpb.KeyValue {
		return &mvccpb.KeyValue{Key: []byte(b.namespace.Root() + "quota-model/" + name), Value: []byte(wire), Version: 1, CreateRevision: create, ModRevision: mod}
	}
	journal := point("publication", "model-publication", 200, 200)
	h := &coldQuotaReturnHistory{
		reservation: ColdPreparationRecord{Version: 1, RestoreEpoch: b.restoreEpoch, ProfileDigest: b.preparationDigest, Owner: r.Owner, Quota: r.Quota, Preparation: r.Preparation, Phase: "settled"},
		publication: &coldPublicationHistory{
			entry:   &RuntimeEnrolledPublicationEntry{Record: RuntimeEnrolledPublicationRecord{RuntimePublicationRecord: RuntimePublicationRecord{Runtime: r.Runtime, DataGateEpoch: r.DataGateEpoch}, NodeUID: r.NodeUID, NodeBootID: r.NodeBootID}, Reference: r.Publication},
			journal: journal,
			receipt: point("publication-receipt", "model-publication-receipt", 200, 200),
		},
		release:    &localOwnerReleaseProof{record: r.Release, kv: point("release", "model-release", 250, 250)},
		settlement: r.PreparationSettlement,
		reservedKV: point("reservation", "model-reservation", 180, 230),
		workKV:     point("work", "model-work", 180, 230),
		settledKV:  point("settlement-receipt", "model-settlement-receipt", 230, 230),
	}
	ready := point("ready", "model-ready", 190, 190)
	slotKey, free, err := b.namespace.coldSlotKeys(r.Quota)
	require.NoError(t, err)
	slotWire, err := coldEncode(coldSlotRecord{Version: 1, RestoreEpoch: b.restoreEpoch, ProfileDigest: b.preparationDigest, Slot: r.Quota, Occupied: true, Owner: r.Owner})
	require.NoError(t, err)
	slot := &mvccpb.KeyValue{Key: []byte(slotKey), Value: []byte(slotWire), Version: 1, CreateRevision: 180, ModRevision: 230}
	key, err := b.namespace.coldRuntimeQuotaReturnKey(r.Owner.Partition, r.Owner.IntentID)
	require.NoError(t, err)
	mutation, err := b.coldQuotaReturnMutation(h, ready, slot, key, free, r.Attempt)
	require.NoError(t, err)
	mutation, digest, err := prepareMutation(b.namespace, mutation)
	require.NoError(t, err)
	p := &coldQuotaReturnPrepared{backend: b, owner: o, reference: r.Attempt.reference(digest), mutation: mutation, history: h, returnKey: key}
	p.self = p

	server.mu.Lock()
	for _, kv := range []*mvccpb.KeyValue{ready, journal, h.publication.receipt, h.reservedKV, h.workKV, h.settledKV, h.release.kv, slot} {
		server.values[string(kv.Key)] = workerTestKV(kv)
	}
	server.revision = 300
	server.requests = nil
	server.mu.Unlock()
	return b, server, o, p
}

func coldQuotaPreparedNoWrites(t *testing.T, server *workerTestServer) {
	t.Helper()
	grants, _, _ := server.counts()
	require.Zero(t, grants)
	server.mu.Lock()
	defer server.mu.Unlock()
	for _, request := range server.requests {
		for _, operation := range append(append([]*pb.RequestOp(nil), request.Success...), request.Failure...) {
			require.Nil(t, operation.GetRequestPut(), "preparation/refusal must not write a guard or domain record")
			require.Nil(t, operation.GetRequestDeleteRange())
		}
	}
}

// Removing registration checks would let an untracked call outlive Close or
// consume another backend's capacity; both must refuse before every write.
func TestColdQuotaPreparedOwnerAdmission(t *testing.T) {
	for _, mode := range []string{"nil", "foreign", "unregistered", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			b, server, owner, _ := coldQuotaPreparedModel(t)
			switch mode {
			case "nil":
				owner = nil
			case "foreign":
				owner = &runtimeEnrolledPublicationOwner{backend: &Backend{}, context: context.Background()}
			case "unregistered":
				owner = &runtimeEnrolledPublicationOwner{backend: b, context: context.Background()}
			case "cancelled":
				owner.cancel()
			}
			prepared, result, err := b.prepareColdRuntimeQuotaReturn(owner, WorkspaceIdentity{}, "intent", TaskReference{})
			require.Error(t, err)
			require.Nil(t, prepared)
			require.Equal(t, OutcomeUnknown, result.Outcome)
			require.Empty(t, result.Reference)
			coldQuotaPreparedNoWrites(t, server)
		})
	}
}

// Losing the consume CAS, self check, backend binding, owner binding, or owner
// cancellation check must never allocate a second lease or recreate a guard.
func TestColdQuotaPreparedContinuationRefusal(t *testing.T) {
	for _, mode := range []string{"copy", "foreign-backend", "foreign-owner", "cancelled", "finished", "changed-mutation"} {
		t.Run(mode, func(t *testing.T) {
			b, server, owner, prepared := coldQuotaPreparedModel(t)
			original := prepared.reference
			caller, callOwner := b, owner
			switch mode {
			case "copy":
				copy := *prepared
				prepared = &copy
			case "foreign-backend":
				caller = &Backend{}
			case "foreign-owner":
				var err error
				callOwner, err = b.registerEnrolledPublication(context.Background())
				require.NoError(t, err)
				defer callOwner.finish()
			case "cancelled":
				owner.cancel()
			case "finished":
				owner.finish()
			case "changed-mutation":
				prepared.mutation.Writes[0].Value = []byte("changed after digest")
			}
			result, err := caller.continueColdRuntimeQuotaReturn(callOwner, prepared)
			require.Error(t, err)
			require.Equal(t, OutcomeUnknown, result.Outcome)
			require.Equal(t, original, result.Reference)
			require.Nil(t, result.Entry)
			result, err = b.continueColdRuntimeQuotaReturn(owner, prepared)
			require.Error(t, err, "a refused continuation is consumed even when retried with the original binding")
			require.Equal(t, OutcomeUnknown, result.Outcome)
			require.Equal(t, original, result.Reference)
			coldQuotaPreparedNoWrites(t, server)
		})
	}
}

func TestColdQuotaPreparedContinuationUsesOriginalAttempt(t *testing.T) {
	b, server, owner, prepared := coldQuotaPreparedModel(t)
	original := prepared.reference
	result, err := b.continueColdRuntimeQuotaReturn(owner, prepared)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.Equal(t, original, result.Reference)
	require.NotNil(t, result.Entry)
	guard, receipt, err := b.stageKeys(original)
	require.NoError(t, err)
	require.Nil(t, server.point(guard), "the original guard is released after the continuation")
	receiptKV := server.point(receipt)
	require.NotNil(t, receiptKV)
	outcome, err := decodeReceipt(receiptKV, original)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	returned, err := decodeColdRuntimeQuotaReturn(server.point(prepared.returnKey))
	require.NoError(t, err)
	require.Equal(t, original.AttemptID, returned.Attempt.AttemptID)
	grants, _, revokes := server.counts()
	require.Equal(t, 1, grants)
	require.Equal(t, 1, revokes)
	result, err = b.continueColdRuntimeQuotaReturn(owner, prepared)
	require.Error(t, err)
	require.Equal(t, OutcomeUnknown, result.Outcome)
	require.Equal(t, original, result.Reference)
	grants, _, _ = server.counts()
	require.Equal(t, 1, grants)
}

func TestColdQuotaPreparedConcurrentConsume(t *testing.T) {
	b, server, owner, prepared := coldQuotaPreparedModel(t)
	const contenders = 8
	start := make(chan struct{})
	var done sync.WaitGroup
	done.Add(contenders)
	results := make(chan ColdRuntimeQuotaReturnResult, contenders)
	for i := 0; i < contenders; i++ {
		go func() {
			defer done.Done()
			<-start
			result, _ := b.continueColdRuntimeQuotaReturn(owner, prepared)
			results <- result
		}()
	}
	close(start)
	done.Wait()
	close(results)
	committed := 0
	for result := range results {
		require.Equal(t, prepared.reference, result.Reference)
		if result.Outcome == OutcomeCommitted {
			committed++
		}
	}
	require.Equal(t, 1, committed)
	grants, _, _ := server.counts()
	require.Equal(t, 1, grants)
}

// Lost or malformed replies retain the pre-Grant reference. The only legal
// retry is arbitration of that reference, never another continuation/Grant.
func TestColdQuotaPreparedUnknownKeepsReference(t *testing.T) {
	for _, phase := range []string{"grant", "guard", "commit", "postread"} {
		t.Run(phase, func(t *testing.T) {
			b, server, owner, prepared := coldQuotaPreparedModel(t)
			original := prepared.reference
			postreadFaults := 0
			if phase == "grant" {
				lease := b.client.Lease
				b.client.Lease = &workerTestLease{Lease: lease, grant: func(ctx context.Context, ttl int64) (*clientv3.LeaseGrantResponse, error) {
					_, err := lease.Grant(ctx, ttl)
					if err != nil {
						return nil, err
					}
					// The model allocated the lease, but its identity was lost.
					return nil, errors.New("model lost grant acknowledgement")
				}}
			} else {
				server.txnReply = func(request *pb.TxnRequest, response *pb.TxnResponse) {
					guardWrite := len(request.Success) == 1 && request.Success[0].GetRequestPut() != nil && request.Success[0].GetRequestPut().Lease != 0
					commit := len(request.Success) == 4 && request.Success[0].GetRequestPut() != nil
					// Bounded point reads include both base identity fences in
					// each branch before the requested quota-return point.
					postread := len(request.Success) == 3 && len(request.Failure) == 3
					if postread {
						for _, branch := range [][]*pb.RequestOp{request.Success, request.Failure} {
							for i, key := range []string{b.identityKey, b.restoreKey, prepared.returnKey} {
								point := branch[i].GetRequestRange()
								if point == nil || string(point.Key) != key || len(point.RangeEnd) != 0 {
									postread = false
								}
							}
						}
					}
					if phase == "guard" && guardWrite || phase == "commit" && commit || phase == "postread" && postread {
						if phase == "postread" {
							postreadFaults++
						}
						response.Header = nil
					}
				}
			}
			result, err := b.continueColdRuntimeQuotaReturn(owner, prepared)
			if phase == "postread" {
				server.mu.Lock()
				hits := postreadFaults
				server.mu.Unlock()
				require.Equal(t, 1, hits, "the actual bounded quota-return postread must be corrupted once")
			}
			require.Error(t, err)
			require.Equal(t, OutcomeUnknown, result.Outcome)
			require.Equal(t, original, result.Reference)
			require.Nil(t, result.Entry)
			server.mu.Lock()
			server.txnReply = nil
			server.mu.Unlock()
			grants, _, _ := server.counts()
			require.Equal(t, 1, grants)
			result, err = b.continueColdRuntimeQuotaReturn(owner, prepared)
			require.Error(t, err)
			require.Equal(t, original, result.Reference)
			after, _, _ := server.counts()
			require.Equal(t, grants, after)
			outcome, err := b.ResolveStage(context.Background(), original)
			require.NoError(t, err)
			if phase == "commit" || phase == "postread" {
				require.Equal(t, OutcomeCommitted, outcome)
			} else {
				require.Equal(t, OutcomeAborted, outcome)
				require.Nil(t, server.point(prepared.returnKey))
			}
		})
	}
}

func TestColdQuotaPreparedAbortBeforeContinueCannotRefund(t *testing.T) {
	b, server, owner, prepared := coldQuotaPreparedModel(t)
	original := prepared.reference
	outcome, err := b.ResolveStage(context.Background(), original)
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, outcome)
	result, err := b.continueColdRuntimeQuotaReturn(owner, prepared)
	require.Error(t, err)
	require.Equal(t, original, result.Reference)
	require.NotEqual(t, OutcomeCommitted, result.Outcome)
	require.Nil(t, result.Entry)
	require.Nil(t, server.point(prepared.returnKey))
	slotKey, free, err := b.namespace.coldSlotKeys(prepared.history.reservation.Quota)
	require.NoError(t, err)
	var slot coldSlotRecord
	require.NoError(t, coldDecode(server.point(slotKey), &slot))
	require.True(t, slot.Occupied)
	require.Nil(t, server.point(free))
	outcome, err = b.ResolveStage(context.Background(), original)
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, outcome)
}
