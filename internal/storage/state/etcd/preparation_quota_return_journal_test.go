package etcd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// These tests reuse decoded codec history from coldQuotaPreparedModel. They
// exercise actual bounded gRPC metadata requests, not public release-history
// authentication, physical death, native etcd, or automatic worker wiring.
func coldQuotaJournalKey(t *testing.T, b *Backend, p *coldQuotaReturnPrepared) string {
	t.Helper()
	key, err := b.namespace.coldRuntimeQuotaAttemptKey(p.history.reservation.Owner.Partition, p.history.reservation.Owner.IntentID)
	require.NoError(t, err)
	return key
}

func coldQuotaJournalInstall(t *testing.T, b *Backend, server *workerTestServer, p *coldQuotaReturnPrepared) *mvccpb.KeyValue {
	t.Helper()
	wire, err := encodeColdRuntimeQuotaAttempt(b.coldQuotaAttemptRecord(p))
	require.NoError(t, err)
	key := coldQuotaJournalKey(t, b, p)
	server.put(key, wire, 0)
	return server.point(key)
}

// A distinct candidate retains the exact original history and slot fences.
// Only its quota-return record's locator changes before its digest is frozen.
func coldQuotaJournalNext(t *testing.T, b *Backend, owner *runtimeEnrolledPublicationOwner, original *coldQuotaReturnPrepared) *coldQuotaReturnPrepared {
	t.Helper()
	ref := original.reference
	locator := StageAttemptLocator{Namespace: ref.Namespace, Partition: ref.Partition, RequestID: ref.RequestID, StageID: ref.StageID, AttemptID: uuid.NewString(), RestoreEpoch: ref.RestoreEpoch}
	wire, err := encodeColdRuntimeQuotaReturn(b.coldQuotaReturnRecord(original.history, locator))
	require.NoError(t, err)
	mutation, _, err := prepareMutation(b.namespace, original.mutation)
	require.NoError(t, err)
	mutation.Writes[2].Value = []byte(wire)
	mutation, digest, err := prepareMutation(b.namespace, mutation)
	require.NoError(t, err)
	p := &coldQuotaReturnPrepared{backend: b, owner: owner, reference: locator.reference(digest), mutation: mutation, history: original.history, returnKey: original.returnKey}
	p.self = p
	return p
}

func coldQuotaJournalWrites(server *workerTestServer, key string) []*pb.TxnRequest {
	server.mu.Lock()
	defer server.mu.Unlock()
	var requests []*pb.TxnRequest
	for _, request := range server.requests {
		for _, operation := range request.Success {
			if put := operation.GetRequestPut(); put != nil && string(put.Key) == key {
				requests = append(requests, request)
			}
		}
	}
	return requests
}

func coldQuotaJournalAwait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("bounded journal test did not reach its gate")
	}
}

// The fluent Txn methods must retain this wrapper so a lost Commit reply is
// injected after the real server has applied the journal, not before the RPC.
type coldQuotaJournalKV struct {
	clientv3.KV
	after func(*clientv3.TxnResponse, error) (*clientv3.TxnResponse, error)
}

func (kv coldQuotaJournalKV) Txn(ctx context.Context) clientv3.Txn {
	return &coldQuotaJournalTxn{Txn: kv.KV.Txn(ctx), after: kv.after}
}

type coldQuotaJournalTxn struct {
	clientv3.Txn
	after func(*clientv3.TxnResponse, error) (*clientv3.TxnResponse, error)
}

func (txn *coldQuotaJournalTxn) If(cmps ...clientv3.Cmp) clientv3.Txn {
	txn.Txn = txn.Txn.If(cmps...)
	return txn
}

func (txn *coldQuotaJournalTxn) Then(ops ...clientv3.Op) clientv3.Txn {
	txn.Txn = txn.Txn.Then(ops...)
	return txn
}

func (txn *coldQuotaJournalTxn) Else(ops ...clientv3.Op) clientv3.Txn {
	txn.Txn = txn.Txn.Else(ops...)
	return txn
}

func (txn *coldQuotaJournalTxn) Commit() (*clientv3.TxnResponse, error) {
	response, err := txn.Txn.Commit()
	return txn.after(response, err)
}

// Calling the continuation before a trusted permanent journal ACK would make
// Grant observe missing attribution. Check the actual boundary, then the
// committed receipt and immutable return, rather than a hand-written flow.
func TestColdQuotaJournalPersistsBeforeGrant(t *testing.T) {
	b, server, owner, prepared := coldQuotaPreparedModel(t)
	key := coldQuotaJournalKey(t, b, prepared)
	replyBytes := 0
	server.txnReply = func(request *pb.TxnRequest, response *pb.TxnResponse) {
		if len(request.Success) == 1 && request.Success[0].GetRequestPut() != nil && string(request.Success[0].GetRequestPut().Key) == key {
			replyBytes = response.Size()
		}
	}
	lease := b.client.Lease
	b.client.Lease = &workerTestLease{Lease: lease, grant: func(ctx context.Context, ttl int64) (*clientv3.LeaseGrantResponse, error) {
		kv := server.point(key)
		if kv == nil {
			return nil, errors.New("Grant preceded permanent journal")
		}
		record, err := b.decodeColdRuntimeQuotaAttempt(prepared.history, kv)
		if err != nil || record.Reference != prepared.reference || kv.Lease != 0 {
			return nil, errors.New("Grant lacks its original durable reference")
		}
		return lease.Grant(ctx, ttl)
	}}
	result, err := b.journalAndContinueColdRuntimeQuotaReturn(owner, prepared, nil, nil)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.Equal(t, prepared.reference, result.Reference)
	require.NotNil(t, result.Entry)
	journal := server.point(key)
	require.NotNil(t, journal)
	require.Less(t, journal.CreateRevision, server.point(prepared.returnKey).CreateRevision)
	requests := coldQuotaJournalWrites(server, key)
	require.Len(t, requests, 1)
	request := requests[0]
	require.Len(t, request.Compare, 30)
	require.Len(t, request.Success, 1)
	require.Len(t, request.Failure, 3)
	require.Zero(t, request.Success[0].GetRequestPut().Lease)
	require.LessOrEqual(t, request.Size(), 262144)
	server.mu.Lock()
	successReplyBytes := replyBytes
	server.mu.Unlock()
	require.Positive(t, successReplyBytes)
	require.LessOrEqual(t, successReplyBytes, 262144)
	t.Logf("fresh journal actual nodes=34 compares=30 success=1 failure=3 request_bytes=%d success_reply_bytes=%d", request.Size(), successReplyBytes)
	for _, kv := range []*mvccpb.KeyValue{prepared.history.publication.journal, prepared.history.publication.receipt, prepared.history.reservedKV, prepared.history.workKV, prepared.history.settledKV, prepared.history.release.kv} {
		coldQuotaJournalRequireFullFence(t, request, kv)
	}
	for _, name := range []string{key, prepared.returnKey} {
		found := 0
		for _, comparison := range request.Compare {
			if string(comparison.Key) == name && comparison.Target == pb.Compare_CREATE && comparison.Result == pb.Compare_EQUAL && comparison.GetCreateRevision() == 0 {
				found++
			}
		}
		require.Equal(t, 1, found, "journal admission must pin each absent point")
	}
	grants, _, revokes := server.counts()
	require.Equal(t, 1, grants)
	require.Equal(t, 1, revokes)
	_, err = b.journalAndContinueColdRuntimeQuotaReturn(owner, prepared, nil, nil)
	require.Error(t, err)
	require.Len(t, coldQuotaJournalWrites(server, key), 1, "the same prepared cannot submit its journal twice")
}

func coldQuotaJournalRequireFullFence(t *testing.T, request *pb.TxnRequest, kv *mvccpb.KeyValue) {
	t.Helper()
	targets := make(map[pb.Compare_CompareTarget]int)
	for _, comparison := range request.Compare {
		if string(comparison.Key) != string(kv.Key) {
			continue
		}
		require.Empty(t, comparison.RangeEnd)
		require.Equal(t, pb.Compare_EQUAL, comparison.Result)
		targets[comparison.Target]++
		switch comparison.Target {
		case pb.Compare_VALUE:
			require.Equal(t, kv.Value, comparison.GetValue())
		case pb.Compare_CREATE:
			require.Equal(t, kv.CreateRevision, comparison.GetCreateRevision())
		case pb.Compare_MOD:
			require.Equal(t, kv.ModRevision, comparison.GetModRevision())
		case pb.Compare_LEASE:
			require.Zero(t, comparison.GetLease())
		default:
			t.Fatal("unexpected full-fence target")
		}
	}
	require.Equal(t, map[pb.Compare_CompareTarget]int{pb.Compare_VALUE: 1, pb.Compare_CREATE: 1, pb.Compare_MOD: 1, pb.Compare_LEASE: 1}, targets)
}

// Every untrusted admission ACK burns this prepared capability. Otherwise a
// later direct continue or retry could Grant an attempt whose CAS was unknown.
func TestColdQuotaJournalUnknownBurnsPrepared(t *testing.T) {
	for _, mode := range []string{"transport", "header", "cluster", "put-shape", "cancelled-after-ack"} {
		t.Run(mode, func(t *testing.T) {
			b, server, owner, prepared := coldQuotaPreparedModel(t)
			key := coldQuotaJournalKey(t, b, prepared)
			hits := 0
			kv := b.client.KV
			b.client.KV = coldQuotaJournalKV{KV: kv, after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				if server.point(key) == nil || hits != 0 {
					return response, err
				}
				hits++
				switch mode {
				case "transport":
					return nil, errors.New("model lost durable journal acknowledgement")
				case "header":
					response.Header = nil
				case "cluster":
					response.Header.ClusterId++
				case "put-shape":
					response.Responses = nil
				case "cancelled-after-ack":
					owner.cancel()
				}
				return response, err
			}}
			result, err := b.journalAndContinueColdRuntimeQuotaReturn(owner, prepared, nil, nil)
			require.Error(t, err)
			require.Equal(t, 1, hits, "fault must reach the actual durable admission reply")
			require.Equal(t, OutcomeUnknown, result.Outcome)
			require.Equal(t, prepared.reference, result.Reference)
			require.Nil(t, result.Entry)
			require.NotNil(t, server.point(key), "unknown is not proof that journal admission aborted")
			b.client.KV = kv
			_, err = b.continueColdRuntimeQuotaReturn(owner, prepared)
			require.Error(t, err)
			_, err = b.journalAndContinueColdRuntimeQuotaReturn(owner, prepared, nil, nil)
			require.Error(t, err)
			require.Len(t, coldQuotaJournalWrites(server, key), 1)
			grants, _, _ := server.counts()
			require.Zero(t, grants)
			require.Nil(t, server.point(prepared.returnKey))
		})
	}
}

func TestColdQuotaJournalOwnerAdmissionBurnsPrepared(t *testing.T) {
	for _, mode := range []string{"nil", "foreign", "unregistered", "cancelled", "finished", "copy"} {
		t.Run(mode, func(t *testing.T) {
			b, server, owner, prepared := coldQuotaPreparedModel(t)
			caller := owner
			switch mode {
			case "nil":
				caller = nil
			case "foreign":
				caller = &runtimeEnrolledPublicationOwner{backend: &Backend{}, context: context.Background()}
			case "unregistered":
				caller = &runtimeEnrolledPublicationOwner{backend: b, context: context.Background()}
			case "cancelled":
				owner.cancel()
			case "finished":
				owner.finish()
			case "copy":
				copy := *prepared
				prepared = &copy
			}
			result, err := b.journalAndContinueColdRuntimeQuotaReturn(caller, prepared, nil, nil)
			require.Error(t, err)
			require.Equal(t, OutcomeUnknown, result.Outcome)
			require.Equal(t, prepared.reference, result.Reference)
			_, err = b.continueColdRuntimeQuotaReturn(owner, prepared)
			require.Error(t, err, "refused admission must burn this exact prepared capability")
			_, err = b.journalAndContinueColdRuntimeQuotaReturn(owner, prepared, nil, nil)
			require.Error(t, err)
			coldQuotaPreparedNoWrites(t, server)
		})
	}
}

func TestColdQuotaJournalConcurrentPreparedOnce(t *testing.T) {
	b, server, owner, prepared := coldQuotaPreparedModel(t)
	const contenders = 8
	start := make(chan struct{})
	results := make(chan ColdRuntimeQuotaReturnResult, contenders)
	var done sync.WaitGroup
	for i := 0; i < contenders; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			<-start
			result, _ := b.journalAndContinueColdRuntimeQuotaReturn(owner, prepared, nil, nil)
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
	require.Len(t, coldQuotaJournalWrites(server, coldQuotaJournalKey(t, b, prepared)), 1)
	grants, _, _ := server.counts()
	require.Equal(t, 1, grants)
}

// Losing admission cannot Grant the losing reference. The caller can perform
// one bounded read/arbitration of the winner; it cannot create another UUID.
func TestColdQuotaJournalWinnerDoesNotGrantLoser(t *testing.T) {
	b, server, owner, candidate := coldQuotaPreparedModel(t)
	winner := coldQuotaJournalNext(t, b, owner, candidate)
	journal := coldQuotaJournalInstall(t, b, server, winner)
	failureBytes := 0
	server.txnReply = func(request *pb.TxnRequest, response *pb.TxnResponse) {
		if len(request.Success) == 1 && request.Success[0].GetRequestPut() != nil && string(request.Success[0].GetRequestPut().Key) == string(journal.Key) {
			failureBytes = response.Size()
		}
	}
	result, err := b.journalAndContinueColdRuntimeQuotaReturn(owner, candidate, nil, nil)
	require.Error(t, err)
	require.Equal(t, OutcomeUnknown, result.Outcome)
	require.Equal(t, candidate.reference, result.Reference)
	_, err = b.continueColdRuntimeQuotaReturn(owner, candidate)
	require.Error(t, err)
	_, err = b.journalAndContinueColdRuntimeQuotaReturn(owner, candidate, nil, nil)
	require.Error(t, err)
	require.Len(t, coldQuotaJournalWrites(server, string(journal.Key)), 1)
	server.mu.Lock()
	actualFailureBytes := failureBytes
	server.mu.Unlock()
	require.Positive(t, actualFailureBytes)
	require.LessOrEqual(t, actualFailureBytes, 262144)
	t.Logf("journal loser actual failure_reply_bytes=%d", actualFailureBytes)
	grants, _, _ := server.counts()
	require.Zero(t, grants)
	require.Equal(t, journal, server.point(string(journal.Key)))
	result, receipt, err := b.resolveColdRuntimeQuotaAttempt(owner, winner.history, journal)
	require.NoError(t, err)
	require.Equal(t, winner.reference, result.Reference)
	require.Equal(t, OutcomeAborted, result.Outcome)
	require.NotNil(t, receipt)
	_, loserReceipt, err := b.stageKeys(candidate.reference)
	require.NoError(t, err)
	require.Nil(t, server.point(loserReceipt))
}

func TestColdQuotaJournalPendingResolveKeepsOriginalReference(t *testing.T) {
	for _, corruptReply := range []bool{false, true} {
		t.Run(map[bool]string{false: "aborted", true: "unknown"}[corruptReply], func(t *testing.T) {
			b, server, owner, original := coldQuotaPreparedModel(t)
			journal := coldQuotaJournalInstall(t, b, server, original)
			_, receiptKey, err := b.stageKeys(original.reference)
			require.NoError(t, err)
			hits := 0
			if corruptReply {
				server.txnReply = func(request *pb.TxnRequest, response *pb.TxnResponse) {
					for _, operation := range request.Success {
						if put := operation.GetRequestPut(); put != nil && string(put.Key) == receiptKey {
							hits++
							response.Header = nil
						}
					}
				}
			}
			result, receipt, err := b.resolveColdRuntimeQuotaAttempt(owner, original.history, journal)
			require.Equal(t, original.reference, result.Reference)
			if corruptReply {
				require.Error(t, err)
				require.Equal(t, OutcomeUnknown, result.Outcome)
				require.Nil(t, receipt)
				server.mu.Lock()
				faults := hits
				server.txnReply = nil
				server.mu.Unlock()
				require.Equal(t, 1, faults)
			} else {
				require.NoError(t, err)
				require.Equal(t, OutcomeAborted, result.Outcome)
				require.NotNil(t, receipt)
				outcome, err := decodeReceipt(receipt, original.reference)
				require.NoError(t, err)
				require.Equal(t, OutcomeAborted, outcome)
			}
			grants, _, _ := server.counts()
			require.Zero(t, grants)
			require.Nil(t, server.point(original.returnKey))
			require.Equal(t, journal, server.point(string(journal.Key)), "arbitration never marks or replaces the pending journal")
		})
	}
}

// An original aborted receipt permits one new candidate, but replacement
// must still fence that precise receipt and the previous journal snapshot.
func TestColdQuotaJournalReplaceRequiresOriginalAbort(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "foreign", "changed-receipt", "changed-journal"} {
		t.Run(mode, func(t *testing.T) {
			b, server, owner, original := coldQuotaPreparedModel(t)
			journal := coldQuotaJournalInstall(t, b, server, original)
			result, receipt, err := b.resolveColdRuntimeQuotaAttempt(owner, original.history, journal)
			require.NoError(t, err)
			require.Equal(t, OutcomeAborted, result.Outcome)
			require.NotNil(t, receipt)
			candidate := coldQuotaJournalNext(t, b, owner, original)
			switch mode {
			case "missing":
				receipt = nil
			case "foreign":
				foreign := coldQuotaJournalNext(t, b, owner, original)
				_, key, err := b.stageKeys(foreign.reference)
				require.NoError(t, err)
				wire, err := encodeReceipt(foreign.reference, OutcomeAborted)
				require.NoError(t, err)
				server.put(key, wire, 0)
				receipt = server.point(key)
			case "changed-receipt":
				server.put(string(receipt.Key), string(receipt.Value), 0)
			case "changed-journal":
				server.put(string(journal.Key), string(journal.Value), 0)
			}
			result, err = b.journalAndContinueColdRuntimeQuotaReturn(owner, candidate, journal, receipt)
			if mode == "valid" {
				require.NoError(t, err)
				require.Equal(t, OutcomeCommitted, result.Outcome)
				require.Equal(t, candidate.reference, result.Reference)
				require.NotNil(t, result.Entry)
				requests := coldQuotaJournalWrites(server, string(journal.Key))
				require.Len(t, requests, 1)
				require.Len(t, requests[0].Compare, 37)
				require.Len(t, requests[0].Success, 1)
				require.Len(t, requests[0].Failure, 3)
				coldQuotaJournalRequireFullFence(t, requests[0], journal)
				coldQuotaJournalRequireFullFence(t, requests[0], receipt)
				require.LessOrEqual(t, requests[0].Size(), 262144)
				t.Logf("replacement journal actual nodes=41 compares=37 success=1 failure=3 request_bytes=%d", requests[0].Size())
				current, err := b.decodeColdRuntimeQuotaAttempt(candidate.history, server.point(string(journal.Key)))
				require.NoError(t, err)
				require.Equal(t, candidate.reference, current.Reference)
			} else {
				require.Error(t, err)
				require.Equal(t, OutcomeUnknown, result.Outcome)
				require.Equal(t, candidate.reference, result.Reference)
				_, err = b.continueColdRuntimeQuotaReturn(owner, candidate)
				require.Error(t, err)
				require.Nil(t, server.point(candidate.returnKey))
			}
			grants, _, _ := server.counts()
			if mode == "valid" {
				require.Equal(t, 1, grants)
			} else {
				require.Zero(t, grants)
			}
		})
	}
}

// Resolve and an already admitted, delayed continuation share the original
// receipt. If Resolve wins, releasing the Grant gate cannot refund that slot.
func TestColdQuotaJournalLateOriginalContinuationLosesToResolve(t *testing.T) {
	b, server, owner, original := coldQuotaPreparedModel(t)
	entered, allow := make(chan struct{}), make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(allow) })
	lease := b.client.Lease
	b.client.Lease = &workerTestLease{Lease: lease, grant: func(ctx context.Context, ttl int64) (*clientv3.LeaseGrantResponse, error) {
		close(entered)
		select {
		case <-allow:
			return lease.Grant(ctx, ttl)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	finished := make(chan struct{})
	var result ColdRuntimeQuotaReturnResult
	var callErr error
	go func() {
		defer close(finished)
		result, callErr = b.journalAndContinueColdRuntimeQuotaReturn(owner, original, nil, nil)
	}()
	coldQuotaJournalAwait(t, entered)
	journal := server.point(coldQuotaJournalKey(t, b, original))
	require.NotNil(t, journal)
	resolved, receipt, err := b.resolveColdRuntimeQuotaAttempt(owner, original.history, journal)
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, resolved.Outcome)
	require.Equal(t, original.reference, resolved.Reference)
	require.NotNil(t, receipt)
	release.Do(func() { close(allow) })
	coldQuotaJournalAwait(t, finished)
	require.Error(t, callErr)
	require.NotEqual(t, OutcomeCommitted, result.Outcome)
	require.Equal(t, original.reference, result.Reference)
	require.Nil(t, server.point(original.returnKey))
	slotKey, free, err := b.namespace.coldSlotKeys(original.history.reservation.Quota)
	require.NoError(t, err)
	var slot coldSlotRecord
	require.NoError(t, coldDecode(server.point(slotKey), &slot))
	require.True(t, slot.Occupied)
	require.Nil(t, server.point(free))
}

func TestColdQuotaJournalCommittedRequiresOriginalReturn(t *testing.T) {
	for _, removeReturn := range []bool{false, true} {
		t.Run(map[bool]string{false: "authenticated-return", true: "receipt-alone"}[removeReturn], func(t *testing.T) {
			b, server, owner, prepared := coldQuotaPreparedModel(t)
			result, err := b.journalAndContinueColdRuntimeQuotaReturn(owner, prepared, nil, nil)
			require.NoError(t, err)
			require.Equal(t, OutcomeCommitted, result.Outcome)
			journal := server.point(coldQuotaJournalKey(t, b, prepared))
			server.mu.Lock()
			server.requests = nil
			if removeReturn {
				delete(server.values, prepared.returnKey)
			}
			server.mu.Unlock()
			result, receipt, err := b.resolveColdRuntimeQuotaAttempt(owner, prepared.history, journal)
			require.Equal(t, prepared.reference, result.Reference)
			require.Nil(t, receipt)
			if removeReturn {
				require.Error(t, err)
				require.Equal(t, OutcomeUnknown, result.Outcome)
				require.Nil(t, result.Entry)
			} else {
				require.NoError(t, err)
				require.Equal(t, OutcomeCommitted, result.Outcome)
				require.NotNil(t, result.Entry)
			}
			slot, free, err := b.namespace.coldSlotKeys(prepared.history.reservation.Quota)
			require.NoError(t, err)
			server.mu.Lock()
			requests := append([]*pb.TxnRequest(nil), server.requests...)
			server.mu.Unlock()
			for _, request := range requests {
				for _, branch := range [][]*pb.RequestOp{request.Success, request.Failure} {
					for _, op := range branch {
						if point := op.GetRequestRange(); point != nil {
							require.NotEqual(t, slot, string(point.Key))
							require.NotEqual(t, free, string(point.Key))
						}
					}
				}
			}
			grants, _, _ := server.counts()
			require.Equal(t, 1, grants, "committed arbitration never reconstructs the original Stage")
			require.Equal(t, journal, server.point(string(journal.Key)), "the immutable return/receipt is the commit proof; no completion CAS")
		})
	}
}

// Close owns cancellation, while the public caller owns finish after the
// complete chain including ReleaseStage. Revoke must join before client close.
func TestColdQuotaJournalCloseJoinsOriginalOwnerThroughRelease(t *testing.T) {
	b, _, owner, prepared := coldQuotaPreparedModel(t)
	entered, allow := make(chan struct{}), make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(allow) })
	lease := b.client.Lease
	b.client.Lease = &workerTestLease{Lease: lease, revoke: func(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
		close(entered)
		select {
		case <-allow:
			return lease.Revoke(ctx, id)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	finished := make(chan struct{})
	var callErr error
	go func() {
		defer close(finished)
		defer owner.finish()
		_, callErr = b.journalAndContinueColdRuntimeQuotaReturn(owner, prepared, nil, nil)
	}()
	coldQuotaJournalAwait(t, entered)
	closed := make(chan struct{})
	var closeErr error
	go func() {
		defer close(closed)
		closeErr = b.Close()
	}()
	coldQuotaJournalAwait(t, owner.context.Done())
	select {
	case <-closed:
		t.Fatal("Close returned before original ReleaseStage joined")
	default:
	}
	b.archiveLife.mu.Lock()
	_, registered := b.archiveLife.enrolled[owner]
	b.archiveLife.mu.Unlock()
	require.True(t, registered)
	release.Do(func() { close(allow) })
	coldQuotaJournalAwait(t, finished)
	coldQuotaJournalAwait(t, closed)
	require.NoError(t, callErr)
	require.NoError(t, closeErr)
}

func TestColdQuotaJournalCodecAndOriginalBinding(t *testing.T) {
	b, _, _, prepared := coldQuotaPreparedModel(t)
	record := b.coldQuotaAttemptRecord(prepared)
	wire, err := encodeColdRuntimeQuotaAttempt(record)
	require.NoError(t, err)
	require.LessOrEqual(t, len(wire), 4096)
	key := coldQuotaJournalKey(t, b, prepared)
	require.Equal(t, fmt.Sprintf("%sp/%02x/intents/%s/cold-runtime-quota-attempt", b.namespace.Root(), prepared.history.reservation.Owner.Partition, prepared.history.reservation.Owner.IntentID), key)
	kv := &mvccpb.KeyValue{Key: []byte(key), Value: []byte(wire), Version: 2, CreateRevision: 301, ModRevision: 310}
	got, err := b.decodeColdRuntimeQuotaAttempt(prepared.history, kv)
	require.NoError(t, err, "pending journal is replaceable permanent metadata")
	require.Equal(t, record, got)
	require.Equal(t, prepared.reference, got.Reference)
	require.Equal(t, prepared.history.release.record.Task, got.Task)
	require.Equal(t, prepared.history.release.record.TaskBirth, got.TaskBirth)
	for _, value := range []string{" " + wire, wire + " ", "null", strings.Repeat("x", 4097), strings.Replace(wire, `"version":1`, `"version":1,"version":1`, 1), strings.Replace(wire, `"version":1`, `"version":1,"mutation":{}`, 1)} {
		bad := workerTestKV(kv)
		bad.Value = []byte(value)
		_, err := b.decodeColdRuntimeQuotaAttempt(prepared.history, bad)
		require.Error(t, err)
	}
	for _, change := range []func(*mvccpb.KeyValue){
		func(k *mvccpb.KeyValue) { k.Lease = 1 },
		func(k *mvccpb.KeyValue) { k.Version = 0 },
		func(k *mvccpb.KeyValue) { k.CreateRevision = 0 },
		func(k *mvccpb.KeyValue) { k.ModRevision = k.CreateRevision - 1 },
		func(k *mvccpb.KeyValue) { k.Key = append(k.Key, []byte("/child")...) },
	} {
		bad := workerTestKV(kv)
		change(bad)
		_, err := b.decodeColdRuntimeQuotaAttempt(prepared.history, bad)
		require.Error(t, err)
	}
	for _, change := range []func(*coldRuntimeQuotaAttemptRecord){
		func(r *coldRuntimeQuotaAttemptRecord) { r.Version++ },
		func(r *coldRuntimeQuotaAttemptRecord) { r.Domain = "other" },
		func(r *coldRuntimeQuotaAttemptRecord) { r.Namespace += "other/" },
		func(r *coldRuntimeQuotaAttemptRecord) { r.RestoreEpoch = "other" },
		func(r *coldRuntimeQuotaAttemptRecord) { r.ProfileDigest = strings.Repeat("0", 64) },
		func(r *coldRuntimeQuotaAttemptRecord) { r.Owner.Generation++ },
		func(r *coldRuntimeQuotaAttemptRecord) { r.Owner.IntentID = "other" },
		func(r *coldRuntimeQuotaAttemptRecord) { r.Owner.SandboxID = "other" },
		func(r *coldRuntimeQuotaAttemptRecord) { r.Owner.RequestID = "other" },
		func(r *coldRuntimeQuotaAttemptRecord) { r.Owner.WorkspaceHash = strings.Repeat("0", 64) },
		func(r *coldRuntimeQuotaAttemptRecord) { r.Owner.Partition++ },
		func(r *coldRuntimeQuotaAttemptRecord) { r.Quota.Epoch++ },
		func(r *coldRuntimeQuotaAttemptRecord) { r.Quota.Kind = "preparation" },
		func(r *coldRuntimeQuotaAttemptRecord) { r.Task.TaskID = uuid.NewString() },
		func(r *coldRuntimeQuotaAttemptRecord) { r.TaskBirth++ },
		func(r *coldRuntimeQuotaAttemptRecord) { r.ReleaseStamp.SHA256 = strings.Repeat("0", 64) },
		func(r *coldRuntimeQuotaAttemptRecord) { r.Reference.StageID = "other" },
		func(r *coldRuntimeQuotaAttemptRecord) { r.Reference.Digest = "" },
		func(r *coldRuntimeQuotaAttemptRecord) { r.Reference.RequestID = "other" },
	} {
		badRecord := record
		change(&badRecord)
		bad := workerTestKV(kv)
		badWire, encodeErr := encodeColdRuntimeQuotaAttempt(badRecord)
		if encodeErr != nil {
			continue
		}
		bad.Value = []byte(badWire)
		_, err := b.decodeColdRuntimeQuotaAttempt(prepared.history, bad)
		require.Error(t, err, "canonical bytes must still bind original authenticated history")
	}
}

// The private journal key family must not be writable through generic Stage,
// including delete and descendant writes; adjacent intent state stays usable.
func TestColdQuotaJournalReservedStageKey(t *testing.T) {
	b, server, _, prepared := coldQuotaPreparedModel(t)
	key := coldQuotaJournalKey(t, b, prepared)
	for _, relative := range []string{key, key + "/child"} {
		for _, deleteKey := range []bool{false, true} {
			write := Write{Key: relative, Delete: deleteKey}
			if !deleteKey {
				write.Value = []byte("untrusted")
			}
			stage, err := b.BeginStage(context.Background(), prepared.reference.Partition, "request", "generic", Mutation{Writes: []Write{write}}, time.Second)
			require.ErrorIs(t, err, ErrInvalidMutation)
			require.Nil(t, stage)
		}
	}
	for _, adjacent := range []string{key + "-other", strings.TrimSuffix(key, "cold-runtime-quota-attempt") + "cold-runtime-quota-return"} {
		_, _, err := prepareMutation(b.namespace, Mutation{Writes: []Write{{Key: adjacent, Value: []byte("domain")}}})
		require.NoError(t, err)
	}
	grants, _, _ := server.counts()
	require.Zero(t, grants)
}
