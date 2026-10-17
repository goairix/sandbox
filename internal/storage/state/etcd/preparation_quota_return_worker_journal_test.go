package etcd

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
)

// The decoded quota history remains a bounded protocol model. Worker session,
// partition and shares are the real originals acquired through the backend;
// none of these tests establishes physical death or automatic worker wiring.
type coldQuotaWorkerJournalFixture struct {
	b        *Backend
	server   *workerTestServer
	owner    *runtimeEnrolledPublicationOwner
	prepared *coldQuotaReturnPrepared
	session  *WorkerSession
	part     *WorkerPartitionClaim
	ranges   *WorkerBudgetShare
	claims   *WorkerBudgetShare
	grants   int
}

func newColdQuotaWorkerJournalFixture(t *testing.T) *coldQuotaWorkerJournalFixture {
	t.Helper()
	b, server, owner, prepared := coldQuotaPreparedModel(t)
	session, err := b.OpenWorkerSession(context.Background(), "quota-worker-journal")
	require.NoError(t, err)
	part, err := b.ClaimWorkerPartition(context.Background(), session, prepared.reference.Partition)
	require.NoError(t, err)
	ranges, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetRange, 0)
	require.NoError(t, err)
	claims, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetClaim, 0)
	require.NoError(t, err)

	// Freeze earning, so a missing charge cannot hide behind elapsed time and
	// duplicate callers cannot earn replacement credit during the assertions.
	b.archiveLife.mu.Lock()
	frozen := time.Now().Add(time.Hour)
	ranges.bucket.last, ranges.bucket.tokens = frozen, 262144
	claims.bucket.last, claims.bucket.tokens = frozen, 1
	b.archiveLife.mu.Unlock()
	grants, _, _ := server.counts()
	server.mu.Lock()
	server.requests = nil
	server.mu.Unlock()
	return &coldQuotaWorkerJournalFixture{b: b, server: server, owner: owner, prepared: prepared, session: session, part: part, ranges: ranges, claims: claims, grants: grants}
}

func (f *coldQuotaWorkerJournalFixture) journal(previous, aborted *mvccpb.KeyValue) (*coldQuotaWorkerJournalPermit, error) {
	return f.b.journalColdRuntimeQuotaReturnWorker(f.owner, f.prepared, f.session, f.part, f.ranges, f.claims, previous, aborted)
}

func (f *coldQuotaWorkerJournalFixture) requireNoContinuation(t *testing.T) {
	t.Helper()
	grants, _, _ := f.server.counts()
	require.Equal(t, f.grants, grants, "worker journal cannot Grant a Stage lease")
	guard, receipt, err := f.b.stageKeys(f.prepared.reference)
	require.NoError(t, err)
	require.Nil(t, f.server.point(guard))
	require.Nil(t, f.server.point(receipt))
	require.Nil(t, f.server.point(f.prepared.returnKey))
	slot, free, err := f.b.namespace.coldSlotKeys(f.prepared.history.reservation.Quota)
	require.NoError(t, err)
	require.NotNil(t, f.server.point(slot), "journal cannot delete the occupied quota slot")
	require.Nil(t, f.server.point(free), "journal cannot refund quota")
	select {
	case <-f.owner.done:
		t.Fatal("journal must retain the original enrolled owner for continuation")
	default:
	}
}

func coldQuotaWorkerJournalRequireFence(t *testing.T, request *pb.TxnRequest, fence taskFence) {
	t.Helper()
	targets := make(map[pb.Compare_CompareTarget]int)
	for _, comparison := range request.Compare {
		if string(comparison.Key) != fence.key {
			continue
		}
		require.Equal(t, pb.Compare_EQUAL, comparison.Result)
		require.Empty(t, comparison.RangeEnd)
		targets[comparison.Target]++
		switch comparison.Target {
		case pb.Compare_VALUE:
			require.Equal(t, fence.value, string(comparison.GetValue()))
		case pb.Compare_CREATE:
			require.Equal(t, fence.create, comparison.GetCreateRevision())
		case pb.Compare_MOD:
			require.Equal(t, fence.mod, comparison.GetModRevision())
		case pb.Compare_LEASE:
			require.Equal(t, fence.lease, comparison.GetLease())
		default:
			t.Fatal("worker fence lost its exact target")
		}
	}
	require.Equal(t, map[pb.Compare_CompareTarget]int{pb.Compare_VALUE: 1, pb.Compare_CREATE: 1, pb.Compare_MOD: 1, pb.Compare_LEASE: 1}, targets)
}

// Losing any original body or worker fence, rewriting the failure branch, or
// calling the public journal-and-continue path must fail these assertions.
func TestColdQuotaWorkerJournalPaidOriginalFences(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprintf("replacement-%t", replacement), func(t *testing.T) {
			f := newColdQuotaWorkerJournalFixture(t)
			var previous, aborted *mvccpb.KeyValue
			if replacement {
				previous = coldQuotaJournalInstall(t, f.b, f.server, f.prepared)
				outcome, err := f.b.ResolveStage(context.Background(), f.prepared.reference)
				require.NoError(t, err)
				require.Equal(t, OutcomeAborted, outcome)
				_, receipt, err := f.b.stageKeys(f.prepared.reference)
				require.NoError(t, err)
				aborted = f.server.point(receipt)
				f.prepared = coldQuotaJournalNext(t, f.b, f.owner, f.prepared)
			}
			originalContext := f.owner.context
			permit, err := f.journal(previous, aborted)
			require.NoError(t, err)
			require.NotNil(t, permit)
			require.Same(t, permit, permit.self)
			require.Same(t, f.prepared, permit.prepared)
			require.Same(t, f.owner, permit.owner)
			require.Equal(t, f.prepared.reference, permit.reference)
			require.Equal(t, uint64(1), permit.journalACK.ClusterId)
			require.Equal(t, f.server.point(coldQuotaJournalKey(t, f.b, f.prepared)).ModRevision, permit.journalACK.Revision)
			require.Zero(t, atomic.LoadUint32(&f.prepared.consumed))
			require.Same(t, originalContext, f.owner.context)
			f.requireNoContinuation(t)

			requests := coldQuotaJournalWrites(f.server, coldQuotaJournalKey(t, f.b, f.prepared))
			require.Len(t, requests, 1)
			request := requests[0]
			compares, nodes := 50, 54
			if replacement {
				compares, nodes = 57, 61
			}
			require.Len(t, request.Compare, compares)
			require.Equal(t, nodes, len(request.Compare)+len(request.Success)+len(request.Failure))
			require.LessOrEqual(t, request.Size(), 128<<10)
			require.Len(t, request.Success, 1)
			require.Zero(t, request.Success[0].GetRequestPut().Lease)
			require.Equal(t, taskProtoPoints([]string{f.b.identityKey, f.b.restoreKey, coldQuotaJournalKey(t, f.b, f.prepared)}), request.Failure)
			for _, kv := range []*mvccpb.KeyValue{f.prepared.history.publication.journal, f.prepared.history.publication.receipt, f.prepared.history.reservedKV, f.prepared.history.workKV, f.prepared.history.settledKV, f.prepared.history.release.kv} {
				coldQuotaJournalRequireFullFence(t, request, kv)
			}
			for _, fence := range []taskFence{f.b.workerControl.profile, f.session.guard, f.part.fence, f.ranges.fence, f.claims.fence} {
				coldQuotaWorkerJournalRequireFence(t, request, fence)
			}
			if replacement {
				coldQuotaJournalRequireFullFence(t, request, previous)
				coldQuotaJournalRequireFullFence(t, request, aborted)
			}
			f.b.archiveLife.mu.Lock()
			rangeTokens, claimTokens, operation := f.ranges.bucket.tokens, f.claims.bucket.tokens, f.session.operation
			_, registered := f.b.archiveLife.enrolled[f.owner]
			f.b.archiveLife.mu.Unlock()
			require.Zero(t, rangeTokens)
			require.Zero(t, claimTokens)
			require.Nil(t, operation, "actual journal client return ends only its own worker call")
			require.True(t, registered)
		})
	}
}

// Start with the transaction emitted by the real private worker method, then
// widen only bounded wire fields. Canonical fixed domain lengths come from the
// existing codec fixture; opaque history and worker records retain hard caps.
// The widened request is sizing evidence only and is never submitted as history.
func TestColdQuotaWorkerJournalMaximumWireEnvelope(t *testing.T) {
	bounds := coldQuotaWorkerCanonicalBounds(t)
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprintf("replacement-%t", replacement), func(t *testing.T) {
			f := newColdQuotaWorkerJournalFixture(t)
			var previous, aborted *mvccpb.KeyValue
			if replacement {
				previous = coldQuotaJournalInstall(t, f.b, f.server, f.prepared)
				_, err := f.b.ResolveStage(context.Background(), f.prepared.reference)
				require.NoError(t, err)
				_, key, err := f.b.stageKeys(f.prepared.reference)
				require.NoError(t, err)
				aborted = f.server.point(key)
				f.prepared = coldQuotaJournalNext(t, f.b, f.owner, f.prepared)
			}
			_, err := f.journal(previous, aborted)
			require.NoError(t, err)
			actual := coldQuotaJournalWrites(f.server, coldQuotaJournalKey(t, f.b, f.prepared))
			require.Len(t, actual, 1)
			wire, err := actual[0].Marshal()
			require.NoError(t, err)
			request := new(pb.TxnRequest)
			require.NoError(t, request.Unmarshal(wire))

			h := f.prepared.history
			caps := map[string]int{
				f.b.identityKey:                   maxIdentityJSONBytes,
				f.b.restoreKey:                    128,
				string(h.publication.journal.Key): 8192,
				string(h.publication.receipt.Key): bounds.values["publication-receipt"],
				string(h.reservedKV.Key):          bounds.values["reservation"],
				string(h.workKV.Key):              bounds.values["work"],
				string(h.settledKV.Key):           bounds.values["settlement-receipt"],
				string(h.release.kv.Key):          8192,
			}
			for _, fence := range []taskFence{f.b.workerControl.profile, f.session.guard, f.part.fence, f.ranges.fence, f.claims.fence} {
				caps[fence.key] = workerRecordBytes
			}
			if replacement {
				caps[string(previous.Key)] = coldQuotaAttemptMaxBytes
				maxReceipt, err := encodeReceipt(bounds.prepared.reference, OutcomeAborted)
				require.NoError(t, err)
				caps[string(aborted.Key)] = len(maxReceipt)
			}
			for _, comparison := range request.Compare {
				switch comparison.Target {
				case pb.Compare_VALUE:
					limit, ok := caps[string(comparison.Key)]
					require.True(t, ok, "every actual value fence needs an explicit bound")
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
				default:
					t.Fatal("new comparison target needs an explicit wire bound")
				}
			}
			request.Success[0].GetRequestPut().Value = []byte(strings.Repeat("x", coldQuotaAttemptMaxBytes))
			failureKeys := []string{f.b.identityKey, f.b.restoreKey, coldQuotaJournalKey(t, f.b, f.prepared)}
			projection := coldQuotaWorkerWidenKeys(cleanupWireProjection{name: "actual-worker-journal", request: request, success: taskProtoPutResponse(1), failure: taskProtoResponse(failureKeys, []string{strings.Repeat("x", maxIdentityJSONBytes), strings.Repeat("x", 128), strings.Repeat("x", coldQuotaAttemptMaxBytes)})})
			nodes := 54
			if replacement {
				nodes = 61
			}
			coldQuotaWorkerRequireProjection(t, projection, nodes)
			require.LessOrEqual(t, projection.request.Size()+projection.failure.Size(), 262144, "one journal paid cohort covers both directions")
		})
	}
}

// Mutate one remote field at a time while keeping every local capability live.
// This catches omitting any full worker fence from the actual journal CAS.
func TestColdQuotaWorkerJournalRemoteFenceLossRefuses(t *testing.T) {
	for _, kind := range []string{"profile", "session", "partition", "range", "claim"} {
		for _, field := range []string{"value", "create", "mod", "lease"} {
			t.Run(kind+"-"+field, func(t *testing.T) {
				f := newColdQuotaWorkerJournalFixture(t)
				fences := map[string]taskFence{"profile": f.b.workerControl.profile, "session": f.session.guard, "partition": f.part.fence, "range": f.ranges.fence, "claim": f.claims.fence}
				f.server.mu.Lock()
				kv := f.server.values[fences[kind].key]
				switch field {
				case "value":
					kv.Value = append(kv.Value, 'x')
				case "create":
					kv.CreateRevision++
				case "mod":
					kv.ModRevision++
				case "lease":
					kv.Lease++
				}
				f.server.mu.Unlock()
				permit, err := f.journal(nil, nil)
				require.ErrorIs(t, err, ErrConflict)
				require.Nil(t, permit)
				require.Nil(t, f.server.point(coldQuotaJournalKey(t, f.b, f.prepared)))
				require.Equal(t, uint32(1), atomic.LoadUint32(&f.prepared.consumed))
				f.requireNoContinuation(t)
			})
		}
	}
}

// Intercept the real raw gRPC call, including its actual server effects. A
// post-invoke gate keeps the original client method from returning even when
// cancellation has fired; a server-only gate cannot exercise that boundary.
// Cleanup finishes the enrolled owner, then restores the still-open original
// client before Backend.Close joins worker calls and revokes their leases. An
// earlier explicit Close may have closed only the temporary transport; physical
// client closes below therefore retain this fixture's once-only ownership.
func coldQuotaWorkerJournalIntercept(t *testing.T, f *coldQuotaWorkerJournalFixture, after func(context.Context, *pb.TxnResponse, error) error) {
	t.Helper()
	key := coldQuotaJournalKey(t, f.b, f.prepared)
	original := f.b.client
	transport, err := clientv3.New(clientv3.Config{Endpoints: original.Endpoints(), DialTimeout: time.Second, DialOptions: []grpc.DialOption{grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, request, reply any, connection *grpc.ClientConn, invoke grpc.UnaryInvoker, options ...grpc.CallOption) error {
		err := invoke(ctx, method, request, reply, connection, options...)
		txn, ok := request.(*pb.TxnRequest)
		if !ok || len(txn.Success) != 1 || txn.Success[0].GetRequestPut() == nil || string(txn.Success[0].GetRequestPut().Key) != key {
			return err
		}
		return after(ctx, reply.(*pb.TxnResponse), err)
	})}})
	require.NoError(t, err)
	f.b.client = transport
	t.Cleanup(func() {
		select {
		case <-f.owner.done:
		default:
			f.owner.finish()
		}
		f.b.client = original
		require.NoError(t, f.b.Close())
		if transport.Ctx().Err() == nil {
			require.NoError(t, transport.Close())
		}
		if original.Ctx().Err() == nil {
			require.NoError(t, original.Close())
		}
	})
}

func TestColdQuotaWorkerJournalUnknownAcknowledgementBurns(t *testing.T) {
	for _, mode := range []string{"transport", "header", "cluster", "revision", "put-shape", "put-prev-kv", "nested-header", "owner-cancelled", "session-cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f := newColdQuotaWorkerJournalFixture(t)
			var hits atomic.Int32
			coldQuotaWorkerJournalIntercept(t, f, func(_ context.Context, response *pb.TxnResponse, err error) error {
				hits.Add(1)
				if err != nil {
					return err
				}
				switch mode {
				case "transport":
					return errors.New("model lost durable journal acknowledgement")
				case "header":
					response.Header = nil
				case "cluster":
					response.Header.ClusterId++
				case "revision":
					response.Header.Revision = 0
				case "put-shape":
					response.Responses = nil
				case "put-prev-kv":
					response.Responses[0].GetResponsePut().PrevKv = &mvccpb.KeyValue{}
				case "nested-header":
					response.Responses[0].GetResponsePut().Header = &pb.ResponseHeader{ClusterId: 99, Revision: response.Header.Revision}
				case "owner-cancelled":
					f.owner.cancel()
				case "session-cancelled":
					f.session.cancel()
				}
				return nil
			})
			permit, err := f.journal(nil, nil)
			require.Error(t, err)
			require.Nil(t, permit)
			require.Equal(t, int32(1), hits.Load())
			require.NotNil(t, f.server.point(coldQuotaJournalKey(t, f.b, f.prepared)), "unknown acknowledgement cannot prove absence")
			require.Equal(t, uint32(1), atomic.LoadUint32(&f.prepared.consumed))
			_, err = f.b.continueColdRuntimeQuotaReturn(f.owner, f.prepared)
			require.Error(t, err)
			permit, err = f.journal(nil, nil)
			require.ErrorIs(t, err, ErrConflict)
			require.Nil(t, permit)
			require.Equal(t, int32(1), hits.Load())
			f.requireNoContinuation(t)
		})
	}
}

func TestColdQuotaWorkerJournalDuplicateCannotBurnWinner(t *testing.T) {
	f := newColdQuotaWorkerJournalFixture(t)
	entered, allow := make(chan struct{}), make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(allow) })
	coldQuotaWorkerJournalIntercept(t, f, func(_ context.Context, _ *pb.TxnResponse, err error) error {
		close(entered)
		<-allow
		return err
	})
	type result struct {
		permit *coldQuotaWorkerJournalPermit
		err    error
	}
	finished := make(chan result, 1)
	go func() {
		permit, err := f.journal(nil, nil)
		finished <- result{permit: permit, err: err}
	}()
	coldQuotaJournalAwait(t, entered)
	permit, err := f.journal(nil, nil)
	require.ErrorIs(t, err, ErrConflict)
	require.Nil(t, permit)
	require.Zero(t, atomic.LoadUint32(&f.prepared.consumed), "duplicate loser cannot burn the actual submitter")
	release.Do(func() { close(allow) })
	winner := workerTestReceive(t, finished)
	require.NoError(t, winner.err)
	require.NotNil(t, winner.permit)
	require.Zero(t, atomic.LoadUint32(&f.prepared.consumed))
	require.Len(t, coldQuotaJournalWrites(f.server, coldQuotaJournalKey(t, f.b, f.prepared)), 1)
	f.requireNoContinuation(t)
}

func TestColdQuotaWorkerJournalReleaseAndCloseJoinActualClientReturn(t *testing.T) {
	for _, mode := range []string{"release-session", "close"} {
		t.Run(mode, func(t *testing.T) {
			f := newColdQuotaWorkerJournalFixture(t)
			entered, allow := make(chan struct{}), make(chan struct{})
			var release sync.Once
			defer release.Do(func() { close(allow) })
			coldQuotaWorkerJournalIntercept(t, f, func(_ context.Context, _ *pb.TxnResponse, err error) error {
				close(entered)
				<-allow
				return err
			})
			finished := make(chan error, 1)
			go func() {
				_, err := f.journal(nil, nil)
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
				t.Fatal("cancellation published completion before the actual client return")
			default:
			}
			select {
			case err := <-stopped:
				t.Fatalf("%s returned before the actual journal call: %v", mode, err)
			default:
			}
			f.b.archiveLife.mu.Lock()
			operation := f.session.operation
			_, registered := f.b.archiveLife.enrolled[f.owner]
			rangeTokens, claimTokens := f.ranges.bucket.tokens, f.claims.bucket.tokens
			f.b.archiveLife.mu.Unlock()
			require.Same(t, call, operation)
			require.True(t, registered)
			require.Zero(t, rangeTokens)
			require.Zero(t, claimTokens)
			release.Do(func() { close(allow) })
			require.Error(t, workerTestReceive(t, finished))
			require.NoError(t, workerTestReceive(t, stopped))
			coldQuotaJournalAwait(t, call.done)
			require.Equal(t, uint32(1), atomic.LoadUint32(&f.prepared.consumed))
			grants, _, _ := f.server.counts()
			require.Equal(t, f.grants, grants)
		})
	}
}

func TestColdQuotaWorkerJournalRefusesChangedBinding(t *testing.T) {
	for _, mode := range []string{"owner", "copy", "digest", "reference", "attempt-id", "partition", "same-reference-replacement", "insufficient-range", "insufficient-claim"} {
		t.Run(mode, func(t *testing.T) {
			f := newColdQuotaWorkerJournalFixture(t)
			var previous, aborted *mvccpb.KeyValue
			switch mode {
			case "owner":
				owner, err := f.b.registerEnrolledPublication(context.Background())
				require.NoError(t, err)
				defer owner.finish()
				f.owner = owner
			case "copy":
				copy := *f.prepared
				f.prepared = &copy
			case "digest":
				f.prepared.mutation.Writes[0].Value = []byte("changed after preparation")
			case "reference":
				f.prepared.reference.Digest = strings.Repeat("0", 64)
			case "attempt-id":
				f.prepared.reference.AttemptID = "ffffffff-ffff-4fff-bfff-ffffffffffff"
			case "partition":
				var err error
				f.part, err = f.b.ClaimWorkerPartition(context.Background(), f.session, f.part.partition+1)
				require.NoError(t, err)
			case "same-reference-replacement":
				previous = coldQuotaJournalInstall(t, f.b, f.server, f.prepared)
				_, err := f.b.ResolveStage(context.Background(), f.prepared.reference)
				require.NoError(t, err)
				_, key, err := f.b.stageKeys(f.prepared.reference)
				require.NoError(t, err)
				aborted = f.server.point(key)
			case "insufficient-range":
				f.ranges.bucket.tokens--
			case "insufficient-claim":
				f.claims.bucket.tokens--
			}
			f.b.archiveLife.mu.Lock()
			ranges, claims := f.ranges.bucket, f.claims.bucket
			f.b.archiveLife.mu.Unlock()
			before := len(coldQuotaJournalWrites(f.server, coldQuotaJournalKey(t, f.b, f.prepared)))
			permit, err := f.journal(previous, aborted)
			require.Error(t, err)
			require.Nil(t, permit)
			require.Equal(t, uint32(1), atomic.LoadUint32(&f.prepared.consumed))
			require.Len(t, coldQuotaJournalWrites(f.server, coldQuotaJournalKey(t, f.b, f.prepared)), before)
			f.b.archiveLife.mu.Lock()
			afterRanges, afterClaims, operation := f.ranges.bucket, f.claims.bucket, f.session.operation
			f.b.archiveLife.mu.Unlock()
			require.Equal(t, ranges, afterRanges)
			require.Equal(t, claims, afterClaims)
			require.Nil(t, operation)
			grants, _, _ := f.server.counts()
			require.Equal(t, f.grants, grants)
		})
	}
}

// Equality is over each complete protobuf comparison, not just a key or target.
// Every corrupt base is also checked against this backend's fresh original base,
// so identically corrupted body and W24 bases cannot silently authorize merging.
func TestColdQuotaWorkerJournalCompositionRejectsEveryChangedBaseField(t *testing.T) {
	f := newColdQuotaWorkerJournalFixture(t)
	for _, side := range []string{"body", "worker", "both"} {
		for _, field := range []string{"key", "target", "result", "range", "value", "lease"} {
			t.Run(side+"-"+field, func(t *testing.T) {
				body, err := f.b.coldQuotaAttemptComparisons(f.prepared, coldQuotaJournalKey(t, f.b, f.prepared), nil, nil)
				require.NoError(t, err)
				worker := append(f.b.baseComparisons(), f.b.workerControl.profile.comparisons()...)
				for _, fence := range []taskFence{f.session.guard, f.part.fence, f.ranges.fence, f.claims.fence} {
					worker = append(worker, fence.comparisons()...)
				}
				mutate := func(comparisons []clientv3.Cmp) {
					switch field {
					case "key":
						comparisons[0].Key = []byte(f.b.restoreKey)
					case "target":
						comparisons[0].Target = pb.Compare_VERSION
					case "result":
						comparisons[0].Result = pb.Compare_NOT_EQUAL
					case "range":
						comparisons[0].RangeEnd = []byte(f.b.restoreKey)
					case "value":
						comparisons[0] = clientv3.Compare(clientv3.Value(f.b.identityKey), "=", f.b.identityValue+" ")
					case "lease":
						comparisons[1] = clientv3.Compare(clientv3.LeaseValue(f.b.identityKey), "=", 1)
					}
				}
				if side != "worker" {
					mutate(body)
				}
				if side != "body" {
					mutate(worker)
				}
				composed, err := f.b.coldQuotaWorkerJournalComparisons(body, worker)
				require.ErrorIs(t, err, ErrInvalidMutation)
				require.Nil(t, composed)
			})
		}
	}
}
