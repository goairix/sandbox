package etcd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// History is the existing decoded protocol model, not a physical-release
// proof. The original enrolled owner and worker capabilities come from the
// existing journal fixture; only the durable journal is installed directly.
func coldQuotaWorkerResolveFixture(t *testing.T) (*coldQuotaWorkerJournalFixture, *mvccpb.KeyValue, string) {
	t.Helper()
	f := newColdQuotaWorkerJournalFixture(t)
	journal := coldQuotaJournalInstall(t, f.b, f.server, f.prepared)
	_, key, err := f.b.stageKeys(f.prepared.reference)
	require.NoError(t, err)
	f.server.mu.Lock()
	f.server.requests = nil
	f.server.mu.Unlock()
	return f, journal, key
}

func coldQuotaWorkerResolve(f *coldQuotaWorkerJournalFixture, journal *mvccpb.KeyValue) (StageReference, Outcome, error) {
	return f.b.resolveColdRuntimeQuotaAttemptWorker(f.owner, f.prepared.history, journal, f.session, f.part, f.ranges, f.claims)
}

func coldQuotaWorkerResolveRequests(f *coldQuotaWorkerJournalFixture) []*pb.TxnRequest {
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	return append([]*pb.TxnRequest(nil), f.server.requests...)
}

func coldQuotaWorkerResolveNoContinuation(t *testing.T, f *coldQuotaWorkerJournalFixture, journal *mvccpb.KeyValue) {
	t.Helper()
	grants, _, _ := f.server.counts()
	require.Equal(t, f.grants, grants)
	guard, _, err := f.b.stageKeys(f.prepared.reference)
	require.NoError(t, err)
	require.Nil(t, f.server.point(guard))
	require.Nil(t, f.server.point(f.prepared.returnKey))
	require.Equal(t, journal, f.server.point(string(journal.Key)))
	slot, free, err := f.b.namespace.coldSlotKeys(f.prepared.history.reservation.Quota)
	require.NoError(t, err)
	require.NotNil(t, f.server.point(slot))
	require.Nil(t, f.server.point(free))
	f.b.archiveLife.mu.Lock()
	_, registered := f.b.archiveLife.enrolled[f.owner]
	operation := f.session.operation
	f.b.archiveLife.mu.Unlock()
	require.True(t, registered)
	require.Nil(t, operation)
}

// Omitting payment, weakening any W24 field, resolving another reference, or
// adding a read/continuation must change this actual native transaction.
func TestColdQuotaWorkerResolvePaidExactOriginalTransaction(t *testing.T) {
	f, journal, receiptKey := coldQuotaWorkerResolveFixture(t)
	connection := f.b.client.ActiveConnection()
	ref, outcome, err := coldQuotaWorkerResolve(f, journal)
	require.NoError(t, err)
	require.Equal(t, f.prepared.reference, ref)
	require.Equal(t, OutcomeAborted, outcome)
	require.Same(t, connection, f.b.client.ActiveConnection())
	requests := coldQuotaWorkerResolveRequests(f)
	require.Len(t, requests, 1, "Resolve alone cannot perform its later proof reread")
	request := requests[0]
	require.Len(t, request.Compare, 25)
	require.Equal(t, 29, len(request.Compare)+len(request.Success)+len(request.Failure))
	for _, fence := range []taskFence{f.b.workerControl.profile, f.session.guard, f.part.fence, f.ranges.fence, f.claims.fence} {
		coldQuotaWorkerJournalRequireFence(t, request, fence)
	}
	require.Equal(t, taskProtoComparisons(f.b.baseComparisons()), request.Compare[:4])
	require.Equal(t, taskProtoComparisons([]clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(receiptKey), "=", 0)})[0], request.Compare[24])
	aborted, err := encodeReceipt(ref, OutcomeAborted)
	require.NoError(t, err)
	require.Equal(t, []*pb.RequestOp{taskProtoPut(receiptKey, aborted, 0)}, request.Success)
	require.Equal(t, taskProtoPoints([]string{f.b.identityKey, f.b.restoreKey, receiptKey}), request.Failure)
	require.LessOrEqual(t, request.Size(), 128<<10)
	worst := taskProtoResponse([]string{f.b.identityKey, f.b.restoreKey, receiptKey}, []string{f.b.identityValue, f.b.restoreEpoch, strings.Repeat("x", 4096)})
	require.LessOrEqual(t, worst.Size(), 16<<10)
	require.LessOrEqual(t, request.Size()+worst.Size(), 262144)
	require.Equal(t, []byte(aborted), f.server.point(receiptKey).Value)
	f.b.archiveLife.mu.Lock()
	rangeTokens, claimTokens := f.ranges.bucket.tokens, f.claims.bucket.tokens
	f.b.archiveLife.mu.Unlock()
	require.Zero(t, rangeTokens)
	require.Zero(t, claimTokens)
	coldQuotaWorkerResolveNoContinuation(t, f, journal)
}

// A receipt supplies arbitration data only. Even Committed must not load the
// quota return or publish a replacement, and false W fences cannot write Then.
func TestColdQuotaWorkerResolveExistingReceiptRemainsOrdinaryOutcome(t *testing.T) {
	for _, outcome := range []Outcome{OutcomeAborted, OutcomeCommitted} {
		t.Run(string(outcome), func(t *testing.T) {
			f, journal, key := coldQuotaWorkerResolveFixture(t)
			wire, err := encodeReceipt(f.prepared.reference, outcome)
			require.NoError(t, err)
			f.server.put(key, wire, 0)
			original := f.server.point(key)
			f.server.mu.Lock()
			f.server.values[f.claims.fence.key].ModRevision++
			f.server.mu.Unlock()
			ref, got, err := coldQuotaWorkerResolve(f, journal)
			require.NoError(t, err)
			require.Equal(t, f.prepared.reference, ref)
			require.Equal(t, outcome, got)
			require.Equal(t, original, f.server.point(key))
			require.Len(t, coldQuotaWorkerResolveRequests(f), 1)
			coldQuotaWorkerResolveNoContinuation(t, f, journal)
		})
	}
}

// These local refusals precede admission. In particular malformed attribution
// and the full final W24 request must not consume either original bucket.
func TestColdQuotaWorkerResolveLocalRefusalPreservesCredit(t *testing.T) {
	for _, mode := range []string{"journal-nil", "noncanonical", "history", "stage", "partition", "owner-copy", "range-copy", "nil-claim", "range-credit", "claim-credit", "request-cap"} {
		t.Run(mode, func(t *testing.T) {
			f, journal, _ := coldQuotaWorkerResolveFixture(t)
			switch mode {
			case "journal-nil":
				journal = nil
			case "noncanonical":
				journal.Value = append(journal.Value, ' ')
			case "history":
				f.prepared.history.release.record.TaskBirth++
			case "stage":
				journal.Value = []byte(strings.Replace(string(journal.Value), "cold_runtime_quota_return", "other_stage", 1))
			case "partition":
				var err error
				f.part, err = f.b.ClaimWorkerPartition(context.Background(), f.session, f.part.partition+1)
				require.NoError(t, err)
			case "owner-copy":
				copy := *f.owner
				f.owner = &copy
			case "range-copy":
				copy := *f.ranges
				copy.self = &copy
				f.ranges = &copy
			case "nil-claim":
				f.claims = nil
			case "range-credit":
				f.ranges.bucket.tokens = 262143
			case "claim-credit":
				f.claims.bucket.tokens = 0
			case "request-cap":
				f.b.workerControl.profile.value = strings.Repeat("x", 131072)
			}
			f.server.mu.Lock()
			f.server.requests = nil
			f.server.mu.Unlock()
			beforeRange := f.ranges.bucket
			var beforeClaim workerBucket
			if f.claims != nil {
				beforeClaim = f.claims.bucket
			}
			_, outcome, err := coldQuotaWorkerResolve(f, journal)
			require.Error(t, err)
			require.Equal(t, OutcomeUnknown, outcome)
			require.Empty(t, coldQuotaWorkerResolveRequests(f))
			require.Equal(t, beforeRange, f.ranges.bucket)
			if f.claims != nil {
				require.Equal(t, beforeClaim, f.claims.bucket)
			}
			require.Nil(t, f.session.operation)
		})
	}
}

// One changed remote field must defeat Then even when every original local
// capability remains live. Existing receipt recovery is covered separately.
func TestColdQuotaWorkerResolveEveryWorkerFencePreventsFreshAbort(t *testing.T) {
	for _, kind := range []string{"profile", "session", "partition", "range", "claim"} {
		for _, field := range []string{"value", "create", "mod", "lease"} {
			t.Run(kind+"-"+field, func(t *testing.T) {
				f, journal, key := coldQuotaWorkerResolveFixture(t)
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
				ref, outcome, err := coldQuotaWorkerResolve(f, journal)
				require.Error(t, err)
				require.Equal(t, f.prepared.reference, ref)
				require.Equal(t, OutcomeUnknown, outcome)
				require.Nil(t, f.server.point(key))
				require.Len(t, coldQuotaWorkerResolveRequests(f), 1)
				coldQuotaWorkerResolveNoContinuation(t, f, journal)
			})
		}
	}
}

// Mutations are applied before real protobuf serialization, so malformed
// headers, point identities and oversized ACKs cross the actual native codec.
func TestColdQuotaWorkerResolveUnknownACKPreservesOriginalReference(t *testing.T) {
	for _, mode := range []string{"header", "put-shape", "put-prev", "nested", "identity", "restore", "receipt-ref", "receipt-lease", "receipt-cap", "response-cap"} {
		t.Run(mode, func(t *testing.T) {
			f, journal, key := coldQuotaWorkerResolveFixture(t)
			if mode != "header" && mode != "put-shape" && mode != "put-prev" && mode != "nested" {
				wire, err := encodeReceipt(f.prepared.reference, OutcomeCommitted)
				require.NoError(t, err)
				f.server.put(key, wire, 0)
			}
			f.server.mu.Lock()
			f.server.txnReply = func(_ *pb.TxnRequest, response *pb.TxnResponse) {
				switch mode {
				case "header":
					response.Header = nil
				case "put-shape":
					response.Responses = nil
				case "put-prev":
					response.Responses[0].GetResponsePut().PrevKv = &mvccpb.KeyValue{}
				case "nested":
					response.Responses[0] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseTxn{ResponseTxn: &pb.TxnResponse{}}}
				case "identity":
					response.Responses[0].GetResponseRange().Kvs[0].Value = []byte("foreign-identity")
				case "restore":
					response.Responses[1].GetResponseRange().Kvs[0].Value = []byte("foreign-restore")
				case "receipt-ref":
					r := f.prepared.reference
					r.Digest = strings.Repeat("0", 64)
					wire, _ := encodeReceipt(r, OutcomeCommitted)
					response.Responses[2].GetResponseRange().Kvs[0].Value = []byte(wire)
				case "receipt-lease":
					response.Responses[2].GetResponseRange().Kvs[0].Lease = 99
				case "receipt-cap":
					kv := response.Responses[2].GetResponseRange().Kvs[0]
					// Generic decodeReceipt accepts trailing JSON whitespace;
					// this isolates the worker's original 4096-byte read cap.
					kv.Value = append(kv.Value, []byte(strings.Repeat(" ", 4097-len(kv.Value)))...)
				case "response-cap":
					response.Responses[2].GetResponseRange().Kvs[0].Value = []byte(strings.Repeat("x", 16385))
				}
			}
			f.server.mu.Unlock()
			ref, outcome, err := coldQuotaWorkerResolve(f, journal)
			require.Error(t, err)
			require.Equal(t, f.prepared.reference, ref)
			require.Equal(t, OutcomeUnknown, outcome)
			if mode == "response-cap" {
				require.Equal(t, codes.ResourceExhausted, status.Code(err))
			}
			require.Len(t, coldQuotaWorkerResolveRequests(f), 1)
			require.True(t, f.session.lost)
			coldQuotaWorkerResolveNoContinuation(t, f, journal)
		})
	}
}

// A client interceptor withholds the actual return after the native server has
// applied the abort. Cancellation must not release the registered call early.
func coldQuotaWorkerResolveIntercept(t *testing.T, f *coldQuotaWorkerJournalFixture, key string, after func(*pb.TxnResponse, error) error) {
	t.Helper()
	original := f.b.client
	transport, err := clientv3.New(clientv3.Config{Endpoints: original.Endpoints(), DialTimeout: time.Second, DialOptions: []grpc.DialOption{grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, request, reply any, connection *grpc.ClientConn, invoke grpc.UnaryInvoker, options ...grpc.CallOption) error {
		err := invoke(ctx, method, request, reply, connection, options...)
		txn, ok := request.(*pb.TxnRequest)
		if ok && len(txn.Success) == 1 && txn.Success[0].GetRequestPut() != nil && string(txn.Success[0].GetRequestPut().Key) == key {
			return after(reply.(*pb.TxnResponse), err)
		}
		return err
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

func TestColdQuotaWorkerResolveLostDurableACKDoesNotRetryOrReadProof(t *testing.T) {
	f, journal, key := coldQuotaWorkerResolveFixture(t)
	coldQuotaWorkerResolveIntercept(t, f, key, func(_ *pb.TxnResponse, err error) error {
		return errors.Join(err, errors.New("lost original abort acknowledgement"))
	})
	ref, outcome, err := coldQuotaWorkerResolve(f, journal)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, f.prepared.reference, ref)
	require.Equal(t, OutcomeUnknown, outcome)
	require.NotNil(t, f.server.point(key), "unknown ACK cannot prove no abort was written")
	require.Len(t, coldQuotaWorkerResolveRequests(f), 1)
	require.True(t, f.session.lost)
	coldQuotaWorkerResolveNoContinuation(t, f, journal)
}

func TestColdQuotaWorkerResolveReleaseAndCloseJoinActualNativeReturn(t *testing.T) {
	for _, mode := range []string{"release", "close"} {
		t.Run(mode, func(t *testing.T) {
			f, journal, key := coldQuotaWorkerResolveFixture(t)
			entered, allow := make(chan struct{}), make(chan struct{})
			var release sync.Once
			defer release.Do(func() { close(allow) })
			coldQuotaWorkerResolveIntercept(t, f, key, func(_ *pb.TxnResponse, err error) error {
				close(entered)
				<-allow
				return err
			})
			finished := make(chan error, 1)
			go func() {
				_, _, err := coldQuotaWorkerResolve(f, journal)
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
				t.Fatal("call completed before actual native return")
			default:
			}
			select {
			case err := <-stopped:
				t.Fatalf("%s returned before native return: %v", mode, err)
			default:
			}
			require.Zero(t, f.ranges.bucket.tokens)
			require.Zero(t, f.claims.bucket.tokens)
			release.Do(func() { close(allow) })
			require.Error(t, workerTestReceive(t, finished))
			require.NoError(t, workerTestReceive(t, stopped))
			coldQuotaJournalAwait(t, call.done)
		})
	}
}

// Resolve owns phase seven on the original call. The stage, journal and
// metadata cohorts must reject it in either order before any second send.
func TestColdQuotaWorkerResolveRPCExcludesEveryOldCohort(t *testing.T) {
	for _, first := range []string{"resolve", "grant", "guard", "commit", "revoke", "journal", "metadata"} {
		t.Run(first, func(t *testing.T) {
			f, host := newColdQuotaRPCHost(t)
			call, _, err := f.begin()
			require.NoError(t, err)
			defer call.finish()
			request := &pb.TxnRequest{Success: taskProtoPoints([]string{f.b.identityKey}), Failure: taskProtoPoints([]string{f.b.identityKey})}
			if first == "resolve" {
				_, err = f.b.coldQuotaWorkerResolveRPC(call, request)
				require.NoError(t, err)
				before := host.count("/etcdserverpb.KV/Txn")
				_, err = f.b.coldQuotaWorkerGrantRPC(call)
				require.Error(t, err)
				_, err = f.b.coldQuotaWorkerGuardRPC(call, request)
				require.Error(t, err)
				_, err = f.b.coldQuotaWorkerCommitRPC(call, request)
				require.Error(t, err)
				_, err = f.b.coldQuotaWorkerRevokeRPC(call, &pb.LeaseRevokeRequest{ID: f.session.reference.LeaseID})
				require.Error(t, err)
				_, err = f.b.coldQuotaWorkerJournalRPC(call, request)
				require.Error(t, err)
				_, err = f.b.coldQuotaWorkerMetadataRPC(call, request)
				require.Error(t, err)
				_, err = f.b.coldQuotaWorkerResolveRPC(call, request)
				require.Error(t, err)
				require.Equal(t, before, host.count("/etcdserverpb.KV/Txn"))
				require.Zero(t, host.count("/etcdserverpb.Lease/LeaseGrant"))
				require.Zero(t, host.count("/etcdserverpb.Lease/LeaseRevoke"))
				return
			}
			if first == "journal" {
				_, err = f.b.coldQuotaWorkerJournalRPC(call, request)
			} else if first == "metadata" {
				_, err = f.b.coldQuotaWorkerMetadataRPC(call, request)
			} else {
				var grant *pb.LeaseGrantResponse
				grant, err = f.b.coldQuotaWorkerGrantRPC(call)
				require.NoError(t, err)
				if first != "grant" {
					_, err = f.b.coldQuotaWorkerGuardRPC(call, request)
					require.NoError(t, err)
				}
				if first == "commit" {
					_, err = f.b.coldQuotaWorkerCommitRPC(call, request)
				} else if first == "revoke" {
					_, err = f.b.coldQuotaWorkerRevokeRPC(call, &pb.LeaseRevokeRequest{ID: grant.ID})
				}
			}
			require.NoError(t, err)
			before := host.count("/etcdserverpb.KV/Txn")
			_, err = f.b.coldQuotaWorkerResolveRPC(call, request)
			require.Error(t, err)
			require.Equal(t, before, host.count("/etcdserverpb.KV/Txn"))
		})
	}
}

func TestColdQuotaWorkerResolveRPCOnceMarshalAndFixedCaps(t *testing.T) {
	request := coldQuotaRPCPointRequest("/resolve/point")
	codec := &coldQuotaWorkerRPCCodec{phase: coldQuotaRPCResolve, requestBytes: request.Size()}
	wire, err := codec.Marshal(request)
	require.NoError(t, err)
	require.NotEmpty(t, wire)
	_, err = codec.Marshal(request)
	require.Error(t, err)
	for _, phase := range []coldQuotaWorkerRPCPhase{coldQuotaRPCGuard, coldQuotaRPCCommit, coldQuotaRPCJournal, coldQuotaRPCResolve} {
		bounded := &coldQuotaWorkerRPCCodec{phase: phase, requestBytes: request.Size()}
		require.Equal(t, 16384, bounded.responseLimit())
		require.Error(t, bounded.Unmarshal(make([]byte, 16385), new(pb.TxnResponse)))
	}
	metadata := &coldQuotaWorkerRPCCodec{phase: coldQuotaRPCMetadata, requestBytes: request.Size()}
	require.Equal(t, 262144-request.Size(), metadata.responseLimit())
	require.Equal(t, 4096, coldQuotaWorkerRPCResponseLimit(coldQuotaRPCGrant))
	require.Equal(t, 1024, coldQuotaWorkerRPCResponseLimit(coldQuotaRPCRevoke))
}

// Apply a real native KV retry policy. The zero retry buffer must prevent a
// cached serialized Resolve payload from being replayed without Marshal.
func TestColdQuotaWorkerResolveRPCNativeRetryPolicyCannotResend(t *testing.T) {
	f, host := newColdQuotaRPCHost(t)
	original := f.b.client
	const policy = `{"methodConfig":[{"name":[{"service":"etcdserverpb.KV","method":"Txn"}],"retryPolicy":{"maxAttempts":3,"initialBackoff":"0.001s","maxBackoff":"0.002s","backoffMultiplier":1,"retryableStatusCodes":["UNAVAILABLE"]}}]}`
	transport, err := clientv3.New(clientv3.Config{Endpoints: original.Endpoints(), DialTimeout: time.Second, DialOptions: []grpc.DialOption{grpc.WithBlock(), grpc.WithDisableServiceConfig(), grpc.WithDefaultServiceConfig(policy)}})
	require.NoError(t, err)
	f.b.client = transport
	t.Cleanup(func() {
		f.b.client = original
		require.NoError(t, transport.Close())
	})
	require.NotNil(t, transport.ActiveConnection().GetMethodConfig("/etcdserverpb.KV/Txn").RetryPolicy)
	host.mu.Lock()
	host.fault = status.Error(codes.Unavailable, "lost Resolve ACK")
	host.mu.Unlock()
	call, _, err := f.begin()
	require.NoError(t, err)
	defer call.finish()
	_, err = f.b.coldQuotaWorkerResolveRPC(call, coldQuotaRPCPointRequest(f.b.identityKey))
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, 1, host.count("/etcdserverpb.KV/Txn"))
}
