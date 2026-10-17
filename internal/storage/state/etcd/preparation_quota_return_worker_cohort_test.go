package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
)

// The source is the existing decoded cold-history model. Only the journal's
// real trusted ACK creates the continuation permit; the model supplies no
// physical release authority and no public committed quota-return evidence.
type coldQuotaWorkerCohortFixture struct {
	b        *Backend
	server   *workerTestServer
	owner    *runtimeEnrolledPublicationOwner
	prepared *coldQuotaReturnPrepared
	permit   *coldQuotaWorkerJournalPermit
	session  *WorkerSession
	part     *WorkerPartitionClaim
	ranges   *WorkerBudgetShare
	claims   *WorkerBudgetShare
	grants   int
	revokes  int
}

func newColdQuotaWorkerCohortFixture(t *testing.T) *coldQuotaWorkerCohortFixture {
	t.Helper()
	b, server, owner, prepared := coldQuotaPreparedModel(t)
	session, err := b.OpenWorkerSession(context.Background(), "quota-worker-cohort")
	require.NoError(t, err)
	part, err := b.ClaimWorkerPartition(context.Background(), session, prepared.reference.Partition)
	require.NoError(t, err)
	ranges, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetRange, 0)
	require.NoError(t, err)
	claims, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetClaim, 0)
	require.NoError(t, err)
	f := &coldQuotaWorkerCohortFixture{b: b, server: server, owner: owner, prepared: prepared, session: session, part: part, ranges: ranges, claims: claims}
	f.fund()
	f.permit, err = b.journalColdRuntimeQuotaReturnWorker(owner, prepared, session, part, ranges, claims, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, f.permit)
	require.Zero(t, atomic.LoadUint32(&prepared.consumed))

	// Journal and Stage are separate earned turns. Freeze earning and restore
	// only local test credit, never replace buckets or bypass real admission.
	f.fund()
	f.grants, _, f.revokes = server.counts()
	server.mu.Lock()
	server.requests = nil
	server.mu.Unlock()
	return f
}

func (f *coldQuotaWorkerCohortFixture) fund() {
	f.b.archiveLife.mu.Lock()
	defer f.b.archiveLife.mu.Unlock()
	frozen := time.Now().Add(time.Hour)
	f.ranges.bucket.last, f.ranges.bucket.tokens = frozen, 262144
	f.claims.bucket.last, f.claims.bucket.tokens = frozen, 1
}

func (f *coldQuotaWorkerCohortFixture) run() (*coldQuotaWorkerCommitPermit, error) {
	return f.b.continueColdRuntimeQuotaReturnWorker(f.owner, f.permit, f.session, f.part, f.ranges, f.claims)
}

type coldQuotaWorkerCohortRPC struct {
	phase      string
	request    any
	reply      any
	connection *grpc.ClientConn
	call       *workerCall
}

func coldQuotaWorkerCohortPhase(request any) string {
	switch r := request.(type) {
	case *pb.LeaseGrantRequest:
		return "Grant"
	case *pb.LeaseRevokeRequest:
		return "Revoke"
	case *pb.TxnRequest:
		if len(r.Success) == 1 && r.Success[0].GetRequestPut() != nil && r.Success[0].GetRequestPut().Lease != 0 {
			return "guard"
		}
		return "Commit"
	default:
		return "unexpected"
	}
}

// A real raw-client interceptor observes the same ActiveConnection used by the
// fixed transport. Its optional post-invoker gate holds the actual client return
// after server effects, which a server-side cancellation gate cannot establish.
func coldQuotaWorkerCohortIntercept(t *testing.T, f *coldQuotaWorkerCohortFixture, after func(context.Context, coldQuotaWorkerCohortRPC, error) error) func() []coldQuotaWorkerCohortRPC {
	t.Helper()
	var mu sync.Mutex
	var trace []coldQuotaWorkerCohortRPC
	original := f.b.client
	transport, err := clientv3.New(clientv3.Config{Endpoints: original.Endpoints(), DialTimeout: time.Second, DialOptions: []grpc.DialOption{grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, request, reply any, connection *grpc.ClientConn, invoke grpc.UnaryInvoker, options ...grpc.CallOption) error {
		f.b.archiveLife.mu.Lock()
		call := f.session.operation
		f.b.archiveLife.mu.Unlock()
		rpc := coldQuotaWorkerCohortRPC{phase: coldQuotaWorkerCohortPhase(request), request: request, reply: reply, connection: connection, call: call}
		if revoke, ok := request.(*pb.LeaseRevokeRequest); ok && revoke.ID == int64(f.session.reference.LeaseID) {
			rpc.phase = "session-Revoke"
		}
		err := invoke(ctx, method, request, reply, connection, options...)
		mu.Lock()
		trace = append(trace, rpc)
		mu.Unlock()
		if after != nil {
			return after(ctx, rpc, err)
		}
		return err
	})}})
	require.NoError(t, err)
	f.b.client = transport
	t.Cleanup(func() {
		// Cleanup callbacks run LIFO. Finish this fixture's owner and let the
		// backend join/revoke its original session while the original client is
		// live; the older backend fixture cleanup then sees cached completion.
		// An explicit Close may already own the temporary transport's closure.
		select {
		case <-f.owner.done:
		default:
			f.owner.finish()
		}
		f.b.client = original
		require.NoError(t, f.b.Close())
		for _, client := range []*clientv3.Client{transport, original} {
			if client.Ctx().Err() == nil {
				require.NoError(t, client.Close())
			}
		}
	})
	return func() []coldQuotaWorkerCohortRPC {
		mu.Lock()
		defer mu.Unlock()
		return append([]coldQuotaWorkerCohortRPC(nil), trace...)
	}
}

func coldQuotaWorkerCohortRequireRetained(t *testing.T, f *coldQuotaWorkerCohortFixture, ref StageReference) {
	t.Helper()
	require.Equal(t, ref, f.prepared.reference)
	require.Equal(t, ref, f.permit.reference)
	require.NotNil(t, f.server.point(coldQuotaJournalKey(t, f.b, f.prepared)), "Stage cannot delete pending arbitration history")
	f.b.archiveLife.mu.Lock()
	_, registered := f.b.archiveLife.enrolled[f.owner]
	operation := f.session.operation
	f.b.archiveLife.mu.Unlock()
	require.True(t, registered, "the original owner belongs to later paid postreads")
	require.Nil(t, operation, "the Stage call must join before it returns")
	select {
	case <-f.owner.done:
		t.Fatal("Stage completion cannot finish the enrolled owner")
	default:
	}
}

// Missing any original RPC, adding a proof read, recreating an attempt, releasing
// a different lease, or completing the owner before postread breaks this trace.
func TestColdQuotaWorkerCohortOriginalFourRPCs(t *testing.T) {
	f := newColdQuotaWorkerCohortFixture(t)
	ref, ownerContext := f.prepared.reference, f.owner.context
	trace := coldQuotaWorkerCohortIntercept(t, f, nil)
	permit, err := f.run()
	require.NoError(t, err)
	require.NotNil(t, permit)
	require.Same(t, permit, permit.self)
	require.Same(t, f.b, permit.backend)
	require.Same(t, f.owner, permit.owner)
	require.Same(t, f.prepared, permit.prepared)
	require.Same(t, f.permit, permit.journal)
	require.Equal(t, ref, permit.reference)
	require.Equal(t, uint64(1), permit.commitACK.ClusterId)
	require.Positive(t, permit.commitACK.Revision)
	require.Same(t, ownerContext, f.owner.context)
	require.Equal(t, uint32(1), atomic.LoadUint32(&f.prepared.consumed))

	observed := trace()
	require.Len(t, observed, 4)
	names := make([]string, len(observed))
	for i, rpc := range observed {
		names[i] = rpc.phase
		require.NotNil(t, rpc.call)
		require.Same(t, observed[0].call, rpc.call)
		require.Same(t, f.b.client.ActiveConnection(), rpc.connection)
	}
	require.Equal(t, []string{"Grant", "guard", "Commit", "Revoke"}, names)
	require.Equal(t, int64(10), observed[0].request.(*pb.LeaseGrantRequest).TTL)
	lease := observed[0].reply.(*pb.LeaseGrantResponse).ID
	require.Positive(t, lease)
	require.Equal(t, lease, observed[3].request.(*pb.LeaseRevokeRequest).ID)
	guard, receipt, err := f.b.stageKeys(ref)
	require.NoError(t, err)
	guardRequest := observed[1].request.(*pb.TxnRequest)
	commitRequest := observed[2].request.(*pb.TxnRequest)
	require.Equal(t, 29, len(guardRequest.Compare)+len(guardRequest.Success)+len(guardRequest.Failure))
	require.Equal(t, 62, len(commitRequest.Compare)+len(commitRequest.Success)+len(commitRequest.Failure))
	require.Len(t, guardRequest.Compare, 26)
	require.Len(t, commitRequest.Compare, 54)
	require.Equal(t, taskProtoPoints([]string{f.b.identityKey, f.b.restoreKey}), guardRequest.Failure)
	require.Equal(t, taskProtoPoints([]string{f.b.identityKey, f.b.restoreKey, receipt, guard}), commitRequest.Failure)
	require.Equal(t, guard, string(guardRequest.Success[0].GetRequestPut().Key))
	require.Equal(t, lease, guardRequest.Success[0].GetRequestPut().Lease)
	var guardRecord struct {
		StageReference
		LeaseID int64 `json:"lease_id"`
	}
	require.NoError(t, json.Unmarshal(guardRequest.Success[0].GetRequestPut().Value, &guardRecord))
	require.Equal(t, ref, guardRecord.StageReference)
	require.Equal(t, lease, guardRecord.LeaseID)
	guardRevision := observed[1].reply.(*pb.TxnResponse).Header.Revision
	for _, comparison := range taskProtoComparisons([]clientv3.Cmp{
		clientv3.Compare(clientv3.Value(guard), "=", string(guardRequest.Success[0].GetRequestPut().Value)),
		clientv3.Compare(clientv3.LeaseValue(guard), "=", lease),
		clientv3.Compare(clientv3.CreateRevision(guard), "=", guardRevision),
		clientv3.Compare(clientv3.CreateRevision(receipt), "=", 0),
	}) {
		require.Contains(t, commitRequest.Compare, comparison)
	}
	for _, request := range []*pb.TxnRequest{guardRequest, commitRequest} {
		for _, fence := range []taskFence{f.b.workerControl.profile, f.session.guard, f.part.fence, f.ranges.fence, f.claims.fence} {
			coldQuotaWorkerJournalRequireFence(t, request, fence)
		}
		for _, comparison := range taskProtoComparisons(f.b.baseComparisons()) {
			found := 0
			for _, actual := range request.Compare {
				if reflect.DeepEqual(actual, comparison) {
					found++
				}
			}
			require.Equal(t, 1, found, "only the completely equal fresh base4 may be deduplicated")
		}
	}
	for _, comparison := range taskProtoComparisons(f.prepared.mutation.Comparisons) {
		require.Contains(t, commitRequest.Compare, comparison, "all original history/slot Value, Lease, Create and Mod fences survive composition")
	}
	require.Equal(t, taskProtoWrites(f.prepared.mutation.Writes), commitRequest.Success[:len(commitRequest.Success)-1])
	require.Equal(t, receipt, string(commitRequest.Success[len(commitRequest.Success)-1].GetRequestPut().Key))
	require.Nil(t, f.server.point(guard))
	require.NotNil(t, f.server.point(receipt))
	require.Equal(t, permit.commitACK.Revision, f.server.point(receipt).ModRevision)
	grants, keeps, revokes := f.server.counts()
	require.Equal(t, f.grants+1, grants)
	require.Zero(t, keeps, "the ten-second Stage must never renew")
	require.Equal(t, f.revokes+1, revokes)
	f.b.archiveLife.mu.Lock()
	ranges, claims := f.ranges.bucket.tokens, f.claims.bucket.tokens
	f.b.archiveLife.mu.Unlock()
	require.Zero(t, ranges)
	require.Zero(t, claims)
	coldQuotaWorkerCohortRequireRetained(t, f, ref)
}

// A pre-admission consume or a partial bucket charge would destroy the only
// original journal permit while it waits for its next earned worker turn.
func TestColdQuotaWorkerCohortInsufficientCreditCanReenter(t *testing.T) {
	for _, deficient := range []string{"range", "claim"} {
		t.Run(deficient, func(t *testing.T) {
			f := newColdQuotaWorkerCohortFixture(t)
			f.b.archiveLife.mu.Lock()
			if deficient == "range" {
				f.ranges.bucket.tokens = 262143
			} else {
				f.claims.bucket.tokens = 0
			}
			ranges, claims := f.ranges.bucket, f.claims.bucket
			f.b.archiveLife.mu.Unlock()
			permit, err := f.run()
			require.ErrorIs(t, err, ErrConflict)
			require.Nil(t, permit)
			require.Zero(t, atomic.LoadUint32(&f.prepared.consumed))
			f.b.archiveLife.mu.Lock()
			afterRanges, afterClaims, operation := f.ranges.bucket, f.claims.bucket, f.session.operation
			f.b.archiveLife.mu.Unlock()
			require.Equal(t, ranges, afterRanges)
			require.Equal(t, claims, afterClaims)
			require.Nil(t, operation)
			grants, _, _ := f.server.counts()
			require.Equal(t, f.grants, grants)

			f.fund()
			permit, err = f.run()
			require.NoError(t, err)
			require.NotNil(t, permit)
			grants, _, _ = f.server.counts()
			require.Equal(t, f.grants+1, grants)
		})
	}
}

// Each server-side fence dimension is changed independently while the original
// local capability remains valid. Omitting any Value/Create/Mod/Lease comparison
// would let that particular guard or refund execute against a changed owner.
func TestColdQuotaWorkerCohortEveryWorkerFenceRejects(t *testing.T) {
	for _, phase := range []string{"guard", "Commit"} {
		for _, role := range []string{"profile", "session", "partition", "range", "claim"} {
			for _, dimension := range []string{"value", "create", "mod", "lease"} {
				t.Run(phase+"-"+role+"-"+dimension, func(t *testing.T) {
					f := newColdQuotaWorkerCohortFixture(t)
					fences := map[string]taskFence{"profile": f.b.workerControl.profile, "session": f.session.guard, "partition": f.part.fence, "range": f.ranges.fence, "claim": f.claims.fence}
					fence := fences[role]
					mutate := func() {
						f.server.mu.Lock()
						defer f.server.mu.Unlock()
						kv := f.server.values[fence.key]
						switch dimension {
						case "value":
							kv.Value = append(kv.Value, ' ')
						case "create":
							kv.CreateRevision++
						case "mod":
							kv.ModRevision++
						case "lease":
							kv.Lease++
						}
					}
					if phase == "guard" {
						mutate()
					}
					coldQuotaWorkerCohortIntercept(t, f, func(_ context.Context, rpc coldQuotaWorkerCohortRPC, err error) error {
						if rpc.phase == "guard" && phase == "Commit" && err == nil {
							mutate()
						}
						return err
					})
					permit, err := f.run()
					require.Error(t, err)
					require.Nil(t, permit)
					require.Nil(t, f.server.point(f.prepared.returnKey))
					slot, free, err := f.b.namespace.coldSlotKeys(f.prepared.history.reservation.Quota)
					require.NoError(t, err)
					require.NotNil(t, f.server.point(slot))
					require.Nil(t, f.server.point(free))
					grants, _, revokes := f.server.counts()
					require.Equal(t, f.grants+1, grants)
					require.Equal(t, f.revokes+1, revokes)
					coldQuotaWorkerCohortRequireRetained(t, f, f.prepared.reference)
				})
			}
		}
	}
}

// A loser must never consume the winner's attempt, finish its paid call, or
// create another Grant while the original native invocation is still returning.
func TestColdQuotaWorkerCohortDuplicateCannotBurnWinner(t *testing.T) {
	f := newColdQuotaWorkerCohortFixture(t)
	entered, allow := make(chan struct{}), make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(allow) })
	trace := coldQuotaWorkerCohortIntercept(t, f, func(_ context.Context, rpc coldQuotaWorkerCohortRPC, err error) error {
		if rpc.phase == "Grant" {
			close(entered)
			<-allow
		}
		return err
	})
	type result struct {
		permit *coldQuotaWorkerCommitPermit
		err    error
	}
	finished := make(chan result, 1)
	go func() {
		permit, err := f.run()
		finished <- result{permit: permit, err: err}
	}()
	coldQuotaJournalAwait(t, entered)
	f.b.archiveLife.mu.Lock()
	call := f.session.operation
	f.b.archiveLife.mu.Unlock()
	require.NotNil(t, call)
	loser, err := f.run()
	require.ErrorIs(t, err, ErrConflict)
	require.Nil(t, loser)
	select {
	case <-call.done:
		t.Fatal("duplicate finished the winner's original call")
	default:
	}
	release.Do(func() { close(allow) })
	winner := workerTestReceive(t, finished)
	require.NoError(t, winner.err)
	require.NotNil(t, winner.permit)
	require.Len(t, trace(), 4)
	f.fund()
	loser, err = f.run()
	require.ErrorIs(t, err, ErrConflict)
	require.Nil(t, loser)
	grants, _, _ := f.server.counts()
	require.Equal(t, f.grants+1, grants)
}

// Lost acknowledgements and malformed successful ACKs cannot reopen admission,
// erase the original reference, refund credit, or omit identified-lease cleanup.
func TestColdQuotaWorkerCohortUnknownKeepsOriginalAttempt(t *testing.T) {
	for _, phase := range []string{"Grant", "guard", "Commit", "Revoke"} {
		faults := []string{"transport", "header", "cluster", "revision", "shape"}
		if phase == "guard" || phase == "Commit" {
			faults = append(faults, "nested-header", "put-prev-kv", "wrong-op")
		}
		for _, fault := range faults {
			t.Run(phase+"-"+fault, func(t *testing.T) {
				f := newColdQuotaWorkerCohortFixture(t)
				ref := f.prepared.reference
				trace := coldQuotaWorkerCohortIntercept(t, f, func(_ context.Context, rpc coldQuotaWorkerCohortRPC, err error) error {
					if rpc.phase != phase || err != nil {
						return err
					}
					if fault == "transport" {
						return errors.New("model lost original " + phase + " acknowledgement")
					}
					var header **pb.ResponseHeader
					switch reply := rpc.reply.(type) {
					case *pb.LeaseGrantResponse:
						header = &reply.Header
						if fault == "shape" {
							reply.TTL = 0
						}
					case *pb.TxnResponse:
						header = &reply.Header
						switch fault {
						case "shape":
							reply.Responses = nil
						case "nested-header":
							reply.Responses[0].GetResponsePut().Header.ClusterId++
						case "put-prev-kv":
							reply.Responses[0].GetResponsePut().PrevKv = &mvccpb.KeyValue{Key: []byte("not-requested")}
						case "wrong-op":
							reply.Responses[0] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: &pb.DeleteRangeResponse{Header: reply.Header, Deleted: 1}}}
						}
					case *pb.LeaseRevokeResponse:
						header = &reply.Header
						if fault == "shape" {
							reply.Header = nil
						}
					}
					switch fault {
					case "header":
						*header = nil
					case "cluster":
						(*header).ClusterId++
					case "revision":
						(*header).Revision = 0
					}
					return nil
				})
				permit, err := f.run()
				require.Error(t, err)
				require.Nil(t, permit)
				require.Equal(t, uint32(1), atomic.LoadUint32(&f.prepared.consumed))
				f.b.archiveLife.mu.Lock()
				rangeTokens, claimTokens := f.ranges.bucket.tokens, f.claims.bucket.tokens
				f.b.archiveLife.mu.Unlock()
				require.Zero(t, rangeTokens, "an unknown cohort must not refund its paid Range")
				require.Zero(t, claimTokens, "an unknown cohort must not refund its paid Claim")
				observed := trace()
				grants, _, revokes := f.server.counts()
				require.Equal(t, f.grants+1, grants)
				identified := phase != "Grant" || fault == "shape"
				if identified {
					require.Equal(t, f.revokes+1, revokes)
					require.Equal(t, "Revoke", observed[len(observed)-1].phase)
					require.Equal(t, observed[0].reply.(*pb.LeaseGrantResponse).ID, observed[len(observed)-1].request.(*pb.LeaseRevokeRequest).ID)
				} else {
					require.Equal(t, f.revokes, revokes)
				}
				f.fund()
				permit, err = f.run()
				require.ErrorIs(t, err, ErrConflict)
				require.Nil(t, permit)
				require.Len(t, trace(), len(observed), "unknown cannot create a fresh attempt")
				coldQuotaWorkerCohortRequireRetained(t, f, ref)
			})
		}
	}
}

// Once Grant identity is trusted, bad TTL/Error and caller/session cancellation
// still require cleanup of that exact lease with an independent bounded context.
func TestColdQuotaWorkerCohortIdentifiedGrantAlwaysCleansUp(t *testing.T) {
	for _, mode := range []string{"ttl", "error", "owner-cancelled", "session-cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f := newColdQuotaWorkerCohortFixture(t)
			trace := coldQuotaWorkerCohortIntercept(t, f, func(ctx context.Context, rpc coldQuotaWorkerCohortRPC, err error) error {
				if rpc.phase == "Grant" && err == nil {
					reply := rpc.reply.(*pb.LeaseGrantResponse)
					switch mode {
					case "ttl":
						reply.TTL = 0
					case "error":
						reply.Error = "identified but unusable lease"
					case "owner-cancelled":
						f.owner.cancel()
						<-ctx.Done()
					case "session-cancelled":
						f.session.cancel()
						<-ctx.Done()
					}
				}
				return err
			})
			permit, err := f.run()
			require.Error(t, err)
			require.Nil(t, permit)
			observed := trace()
			require.Len(t, observed, 2)
			require.Equal(t, "Grant", observed[0].phase)
			require.Equal(t, "Revoke", observed[1].phase)
			require.Equal(t, observed[0].reply.(*pb.LeaseGrantResponse).ID, observed[1].request.(*pb.LeaseRevokeRequest).ID)
			require.Nil(t, f.server.point(f.prepared.returnKey))
			coldQuotaWorkerCohortRequireRetained(t, f, f.prepared.reference)
		})
	}
}

// Preserve the public Stage protocol's failure arbitration: guard failure
// never constructs a Stage; a Commit failure gives its exact original receipt
// precedence, while an absent receipt still checks the full original guard.
func TestColdQuotaWorkerCohortOriginalFailureArbitration(t *testing.T) {
	for _, mode := range []string{"guard-present", "guard-receipt", "guard-identity", "commit-committed", "commit-aborted", "commit-wrong-receipt", "commit-missing-guard", "commit-changed-guard", "commit-conflict"} {
		t.Run(mode, func(t *testing.T) {
			f := newColdQuotaWorkerCohortFixture(t)
			ref := f.prepared.reference
			guard, receipt, err := f.b.stageKeys(ref)
			require.NoError(t, err)
			switch mode {
			case "guard-present":
				f.server.put(guard, "preexisting original guard", 0)
			case "guard-receipt":
				wire, err := encodeReceipt(ref, OutcomeCommitted)
				require.NoError(t, err)
				f.server.put(receipt, wire, 0)
			case "guard-identity":
				f.server.put(f.b.identityKey, f.b.identityValue+" ", 0)
			}
			trace := coldQuotaWorkerCohortIntercept(t, f, func(_ context.Context, rpc coldQuotaWorkerCohortRPC, err error) error {
				if rpc.phase != "guard" || err != nil || !strings.HasPrefix(mode, "commit-") {
					return err
				}
				switch mode {
				case "commit-committed", "commit-aborted", "commit-wrong-receipt":
					outcome, receiptRef := OutcomeCommitted, ref
					if mode == "commit-aborted" {
						outcome = OutcomeAborted
					}
					if mode == "commit-wrong-receipt" {
						receiptRef.AttemptID = "ffffffff-ffff-4fff-bfff-ffffffffffff"
					}
					wire, encodeErr := encodeReceipt(receiptRef, outcome)
					if encodeErr != nil {
						return encodeErr
					}
					f.server.put(receipt, wire, 0)
				case "commit-missing-guard":
					f.server.mu.Lock()
					delete(f.server.values, guard)
					f.server.mu.Unlock()
				case "commit-changed-guard":
					f.server.put(guard, "different original guard", 0)
				case "commit-conflict":
					f.server.put(string(f.prepared.history.workKV.Key), "changed work", 0)
				}
				return nil
			})
			permit, err := f.run()
			if mode == "commit-committed" {
				require.NoError(t, err)
				require.NotNil(t, permit, "the original canonical committed receipt wins arbitration")
			} else {
				require.Error(t, err)
				require.Nil(t, permit)
				if mode == "commit-missing-guard" || mode == "commit-changed-guard" {
					require.ErrorIs(t, err, ErrGuardExpired)
				}
				if mode == "commit-conflict" || mode == "guard-present" || mode == "guard-receipt" {
					require.ErrorIs(t, err, ErrConflict)
				}
			}
			require.Nil(t, f.server.point(f.prepared.returnKey), "receipt arbitration cannot fabricate a verified quota-return entry")
			want := []string{"Grant", "guard", "Commit", "Revoke"}
			if strings.HasPrefix(mode, "guard-") {
				want = []string{"Grant", "guard", "Revoke"}
			}
			var phases []string
			for _, rpc := range trace() {
				phases = append(phases, rpc.phase)
			}
			require.Equal(t, want, phases)
			coldQuotaWorkerCohortRequireRetained(t, f, ref)
		})
	}
}

// Holding every phase after the invoker has returned catches early call.finish
// on cancellation, including the final cleanup native return. Close is allowed
// to join the enrolled owner only after the caller receives this cohort result.
func TestColdQuotaWorkerCohortReleaseAndCloseJoinActualReturn(t *testing.T) {
	for _, mode := range []string{"release-session", "close"} {
		for _, phase := range []string{"Grant", "guard", "Commit", "Revoke"} {
			t.Run(mode+"-"+phase, func(t *testing.T) {
				f := newColdQuotaWorkerCohortFixture(t)
				entered, allow := make(chan struct{}), make(chan struct{})
				var release sync.Once
				defer release.Do(func() { close(allow) })
				coldQuotaWorkerCohortIntercept(t, f, func(_ context.Context, rpc coldQuotaWorkerCohortRPC, err error) error {
					if rpc.phase == phase {
						close(entered)
						<-allow
					}
					return err
				})
				finished := make(chan error, 1)
				go func() {
					_, err := f.run()
					if mode == "close" {
						f.owner.finish()
					}
					finished <- err
				}()
				coldQuotaJournalAwait(t, entered)
				f.b.archiveLife.mu.Lock()
				call := f.session.operation
				f.b.archiveLife.mu.Unlock()
				require.NotNil(t, call)
				stopped := make(chan error, 1)
				go func() {
					if mode == "close" {
						stopped <- f.b.Close()
					} else {
						stopped <- f.b.ReleaseWorkerSession(context.Background(), f.session)
					}
				}()
				coldQuotaJournalAwait(t, call.ctx.Done())
				select {
				case <-call.done:
					t.Fatal("cancellation completed the original call before actual native return")
				default:
				}
				select {
				case err := <-stopped:
					t.Fatalf("%s returned before %s client return: %v", mode, phase, err)
				default:
				}
				f.b.archiveLife.mu.Lock()
				operation := f.session.operation
				_, registered := f.b.archiveLife.enrolled[f.owner]
				f.b.archiveLife.mu.Unlock()
				require.Same(t, call, operation)
				require.True(t, registered)
				release.Do(func() { close(allow) })
				// A cancellation after successful cleanup may still leave trusted
				// commit evidence; it cannot change the physical join requirement.
				_ = workerTestReceive(t, finished)
				require.NoError(t, workerTestReceive(t, stopped))
				coldQuotaJournalAwait(t, call.done)
				grants, _, revokes := f.server.counts()
				require.Equal(t, f.grants+1, grants)
				require.Equal(t, f.revokes+2, revokes, "Stage cleanup and session shutdown own distinct original leases")
			})
		}
	}
}

// Derive maximum bytes from the real worker requests, then widen only bounded
// fields. No widened frame is sent or allowed to serve as authenticated history.
// Dropping W24 from sizing or charging each RPC independently breaks this bound.
func TestColdQuotaWorkerCohortMaximumWireEnvelope(t *testing.T) {
	f := newColdQuotaWorkerCohortFixture(t)
	trace := coldQuotaWorkerCohortIntercept(t, f, nil)
	_, err := f.run()
	require.NoError(t, err)
	observed := trace()
	require.Len(t, observed, 4)
	bounds := coldQuotaWorkerCanonicalBounds(t)
	ref := bounds.prepared.reference
	guardBytes, err := json.Marshal(struct {
		StageReference
		LeaseID int64 `json:"lease_id"`
	}{ref, math.MaxInt64})
	require.NoError(t, err)
	receiptValue, err := encodeReceipt(ref, OutcomeCommitted)
	require.NoError(t, err)
	guardKey, receiptKey, err := f.b.stageKeys(f.prepared.reference)
	require.NoError(t, err)
	slotKey, _, err := f.b.namespace.coldSlotKeys(f.prepared.history.reservation.Quota)
	require.NoError(t, err)
	h := f.prepared.history
	caps := map[string]int{
		f.b.identityKey: maxIdentityJSONBytes, f.b.restoreKey: 128,
		string(f.prepared.mutation.Comparisons[0].Key): bounds.values["ready"],
		string(h.publication.journal.Key):              8192, string(h.publication.receipt.Key): bounds.values["publication-receipt"],
		string(h.reservedKV.Key): bounds.values["reservation"], string(h.workKV.Key): bounds.values["work"],
		string(h.settledKV.Key): bounds.values["settlement-receipt"], string(h.release.kv.Key): 8192,
		slotKey: bounds.values["occupied-slot"], guardKey: len(guardBytes),
	}
	for _, fence := range []taskFence{f.b.workerControl.profile, f.session.guard, f.part.fence, f.ranges.fence, f.claims.fence} {
		caps[fence.key] = workerRecordBytes
	}
	requestBytes := (&pb.LeaseGrantRequest{TTL: 10}).Size() + (&pb.LeaseRevokeRequest{ID: math.MaxInt64}).Size()
	for _, index := range []int{1, 2} {
		original := observed[index].request.(*pb.TxnRequest)
		wire, err := original.Marshal()
		require.NoError(t, err)
		request := new(pb.TxnRequest)
		require.NoError(t, request.Unmarshal(wire))
		for _, comparison := range request.Compare {
			switch comparison.Target {
			case pb.Compare_VALUE:
				limit, ok := caps[string(comparison.Key)]
				require.True(t, ok, "every real value fence needs an explicit schema or codec bound")
				comparison.TargetUnion = &pb.Compare_Value{Value: []byte(strings.Repeat("x", limit))}
			case pb.Compare_CREATE:
				if comparison.GetCreateRevision() != 0 {
					comparison.TargetUnion = &pb.Compare_CreateRevision{CreateRevision: math.MaxInt64}
				}
			case pb.Compare_MOD:
				comparison.TargetUnion = &pb.Compare_ModRevision{ModRevision: math.MaxInt64}
			case pb.Compare_LEASE:
				if comparison.GetLease() != 0 {
					comparison.TargetUnion = &pb.Compare_Lease{Lease: math.MaxInt64}
				}
			}
		}
		for _, operation := range request.Success {
			put := operation.GetRequestPut()
			require.NotNil(t, put)
			switch string(put.Key) {
			case guardKey:
				put.Value, put.Lease = guardBytes, math.MaxInt64
			case receiptKey:
				put.Value = []byte(receiptValue)
			case f.prepared.returnKey:
				put.Value = []byte(strings.Repeat("x", 16384))
			default:
				put.Value = []byte(strings.Repeat("x", bounds.values["free-slot"]))
			}
		}
		keys := []string{f.b.identityKey, f.b.restoreKey}
		values := []string{strings.Repeat("i", maxIdentityJSONBytes), strings.Repeat("e", 128)}
		nodes := 29
		if index == 2 {
			keys, values = append(keys, receiptKey, guardKey), append(values, receiptValue, string(guardBytes))
			nodes = 62
		}
		projection := coldQuotaWorkerWidenKeys(cleanupWireProjection{fmt.Sprintf("worker-cohort-%d", index), request, taskProtoPutResponse(len(request.Success)), taskProtoResponse(keys, values)})
		coldQuotaWorkerRequireProjection(t, projection, nodes)
		require.LessOrEqual(t, max(projection.success.Size(), projection.failure.Size()), 16384)
		requestBytes += projection.request.Size()
		t.Logf("actual worker request widened phase=%s nodes=%d request=%d max_canonical_reply=%d", observed[index].phase, nodes, projection.request.Size(), max(projection.success.Size(), projection.failure.Size()))
	}
	require.LessOrEqual(t, requestBytes, 224256, "the original Revoke request is included before spending the cohort")
	require.LessOrEqual(t, requestBytes+4096+16384+16384+1024, 262144, "all native response caps, including malformed/error ACKs, share one paid Range")
}

// The native original-lease missing result is already safe cleanup. Wrapping
// it as a new unknown would wrongly withhold the trusted Commit continuation.
func TestColdQuotaWorkerCohortOriginalLeaseAlreadyMissingIsSafe(t *testing.T) {
	f := newColdQuotaWorkerCohortFixture(t)
	trace := coldQuotaWorkerCohortIntercept(t, f, func(_ context.Context, rpc coldQuotaWorkerCohortRPC, err error) error {
		if rpc.phase == "Revoke" && err == nil {
			return rpctypes.ErrLeaseNotFound
		}
		return err
	})
	permit, err := f.run()
	require.NoError(t, err)
	require.NotNil(t, permit)
	require.Equal(t, f.prepared.reference, permit.reference)
	require.Len(t, trace(), 4)
	coldQuotaWorkerCohortRequireRetained(t, f, f.prepared.reference)
}

// Journal identity, copied capabilities, changed frozen mutation and invalid
// ACKs must be rejected locally, before charging or creating any new lease.
func TestColdQuotaWorkerCohortSealedBindingRefusal(t *testing.T) {
	for _, mode := range []string{"nil", "copy", "backend", "owner", "prepared-copy", "reference", "digest", "mutation", "ack-cluster", "ack-zero", "ack-before-history", "consumed", "partition", "owner-cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f := newColdQuotaWorkerCohortFixture(t)
			original := f.prepared.reference
			switch mode {
			case "nil":
				f.permit = nil
			case "copy":
				copy := *f.permit
				f.permit = &copy
			case "backend":
				f.permit.backend = &Backend{}
			case "owner":
				f.permit.owner = &runtimeEnrolledPublicationOwner{backend: f.b, context: f.owner.context}
			case "prepared-copy":
				copy := *f.prepared
				f.permit.prepared = &copy
			case "reference":
				f.permit.reference.AttemptID = "ffffffff-ffff-4fff-bfff-ffffffffffff"
			case "digest":
				f.prepared.reference.Digest = strings.Repeat("0", 64)
			case "mutation":
				f.prepared.mutation.Writes[0].Value = []byte("changed after trusted journal")
			case "ack-cluster":
				f.permit.journalACK.ClusterId++
			case "ack-zero":
				f.permit.journalACK.Revision = 0
			case "ack-before-history":
				f.permit.journalACK.Revision = f.prepared.history.release.kv.ModRevision - 1
			case "consumed":
				atomic.StoreUint32(&f.prepared.consumed, 1)
			case "partition":
				var err error
				f.part, err = f.b.ClaimWorkerPartition(context.Background(), f.session, f.part.partition+1)
				require.NoError(t, err)
			case "owner-cancelled":
				f.owner.cancel()
			}
			f.b.archiveLife.mu.Lock()
			ranges, claims := f.ranges.bucket, f.claims.bucket
			f.b.archiveLife.mu.Unlock()
			permit, err := f.run()
			require.Error(t, err)
			require.Nil(t, permit)
			f.b.archiveLife.mu.Lock()
			afterRanges, afterClaims, operation := f.ranges.bucket, f.claims.bucket, f.session.operation
			f.b.archiveLife.mu.Unlock()
			require.Equal(t, ranges, afterRanges)
			require.Equal(t, claims, afterClaims)
			require.Nil(t, operation)
			grants, _, _ := f.server.counts()
			require.Equal(t, f.grants, grants)
			_, receipt, err := f.b.stageKeys(original)
			require.NoError(t, err)
			require.Nil(t, f.server.point(receipt))
		})
	}
}
