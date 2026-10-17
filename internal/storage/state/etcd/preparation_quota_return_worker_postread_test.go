package etcd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
)

// Each fixture earns the original journal and Commit permits through native
// calls. The host model proves protocol ownership and read boundaries; its
// decoded source history supplies no physical termination authority.
func coldQuotaPostreadFixture(t *testing.T) (*coldQuotaWorkerCohortFixture, *coldQuotaWorkerCommitPermit, *coldQuotaWorkerPostread) {
	t.Helper()
	f := newColdQuotaWorkerCohortFixture(t)
	permit, err := f.run()
	require.NoError(t, err)
	require.NotNil(t, permit)
	state, err := f.b.newColdQuotaWorkerPostread(f.owner, permit, f.session, f.part, f.ranges)
	require.NoError(t, err)
	require.NotNil(t, state)
	f.fund()
	f.server.mu.Lock()
	f.server.requests = nil
	f.server.mu.Unlock()
	return f, permit, state
}

func coldQuotaPostreadStep(f *coldQuotaWorkerCohortFixture, permit *coldQuotaWorkerCommitPermit, state *coldQuotaWorkerPostread) (*ColdRuntimeQuotaReturnEntry, error) {
	return f.b.stepColdQuotaWorkerPostread(f.owner, permit, state, f.session, f.part, f.ranges)
}

func coldQuotaPostreadRequests(f *coldQuotaWorkerCohortFixture) []*pb.TxnRequest {
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	return append([]*pb.TxnRequest(nil), f.server.requests...)
}

// Accidentally combining the phases, rereading Q1 after credit refusal, charging
// Claim, or returning mutable retained proof data breaks this progression.
func TestColdQuotaWorkerPostreadPaidYieldAndResume(t *testing.T) {
	f, permit, state := coldQuotaPostreadFixture(t)
	originalRef, originalContext := permit.reference, f.owner.context
	grants, _, revokes := f.server.counts()
	_, receiptKey, err := f.b.stageKeys(originalRef)
	require.NoError(t, err)
	returnKV := f.server.point(f.prepared.returnKey)
	require.NotNil(t, returnKV)
	hash := sha256.Sum256(returnKV.Value)
	h := f.prepared.history
	want := ColdRuntimeQuotaReturnEntry{
		Task: h.release.record.Task, TaskBirth: h.release.record.TaskBirth,
		Owner: h.reservation.Owner, Quota: h.reservation.Quota,
		Runtime: h.publication.entry.Record.Runtime, NodeUID: h.publication.entry.Record.NodeUID,
		NodeBootID:     h.publication.entry.Record.NodeBootID,
		CreateRevision: returnKV.CreateRevision, SHA256: hex.EncodeToString(hash[:]),
	}
	f.b.archiveLife.mu.Lock()
	claimBefore := f.claims.bucket
	f.b.archiveLife.mu.Unlock()

	entry, err := coldQuotaPostreadStep(f, permit, state)
	require.NoError(t, err)
	require.Nil(t, entry, "Q1 must yield before reading a receipt")
	requests := coldQuotaPostreadRequests(f)
	require.Len(t, requests, 1)
	require.Equal(t, taskProtoPoints([]string{f.b.identityKey, f.b.restoreKey, f.prepared.returnKey}), requests[0].Success)
	require.Len(t, requests[0].Compare, 20)
	f.b.archiveLife.mu.Lock()
	rangeBefore := f.ranges.bucket
	f.b.archiveLife.mu.Unlock()
	require.Zero(t, rangeBefore.tokens)

	entry, err = coldQuotaPostreadStep(f, permit, state)
	require.ErrorIs(t, err, ErrConflict)
	require.Nil(t, entry)
	require.Len(t, coldQuotaPostreadRequests(f), 1)
	f.b.archiveLife.mu.Lock()
	rangeAfter, claimAfter := f.ranges.bucket, f.claims.bucket
	f.b.archiveLife.mu.Unlock()
	require.Equal(t, rangeBefore, rangeAfter, "Q2 refusal must not alter the earning origin")
	require.Equal(t, claimBefore, claimAfter)

	// Raw Q1 storage may change after the validated stamp is retained. Q2 must
	// consume its own paid receipt point instead of starting history over.
	f.server.put(f.prepared.returnKey, "changed after validated Q1", 0)
	f.b.archiveLife.mu.Lock()
	f.ranges.bucket.tokens = maxMutationBytes
	f.b.archiveLife.mu.Unlock()
	entry, err = coldQuotaPostreadStep(f, permit, state)
	require.NoError(t, err)
	require.Equal(t, &want, entry)
	requests = coldQuotaPostreadRequests(f)
	require.Len(t, requests, 2)
	require.Equal(t, taskProtoPoints([]string{f.b.identityKey, f.b.restoreKey, receiptKey}), requests[1].Success)
	f.b.archiveLife.mu.Lock()
	paidRange, untouchedClaim := f.ranges.bucket, f.claims.bucket
	f.b.archiveLife.mu.Unlock()
	require.Zero(t, paidRange.tokens)
	require.Equal(t, claimBefore, untouchedClaim)
	for _, request := range requests {
		require.Equal(t, request.Success, request.Failure)
		for _, op := range request.Success {
			require.Nil(t, op.GetRequestPut())
			require.Nil(t, op.GetRequestDeleteRange())
		}
	}
	require.NotNil(t, entry)
	entry.SHA256 = "caller mutation"
	entry, err = coldQuotaPostreadStep(f, permit, state)
	require.NoError(t, err)
	require.Equal(t, &want, entry)
	require.Len(t, coldQuotaPostreadRequests(f), 2, "done must not need another paid read")
	require.Equal(t, originalRef, permit.reference)
	require.Same(t, originalContext, f.owner.context)
	afterGrants, _, afterRevokes := f.server.counts()
	require.Equal(t, grants, afterGrants)
	require.Equal(t, revokes, afterRevokes)
	coldQuotaWorkerCohortRequireRetained(t, f, originalRef)
}

// Typed failures cannot advance to the next phase. Correcting the same point
// and funding one turn must retry that point, rather than restart or skip it.
func TestColdQuotaWorkerPostreadTypedFailureKeepsPhase(t *testing.T) {
	for _, phase := range []string{"return", "receipt"} {
		t.Run(phase, func(t *testing.T) {
			f, permit, state := coldQuotaPostreadFixture(t)
			key := f.prepared.returnKey
			if phase == "receipt" {
				entry, err := coldQuotaPostreadStep(f, permit, state)
				require.NoError(t, err)
				require.Nil(t, entry)
				_, key, err = f.b.stageKeys(permit.reference)
				require.NoError(t, err)
				f.fund()
			}
			original := f.server.point(key)
			f.server.mu.Lock()
			f.server.values[key].Value = []byte("malformed typed history")
			f.server.mu.Unlock()
			entry, err := coldQuotaPostreadStep(f, permit, state)
			require.Error(t, err)
			require.Nil(t, entry)
			before := len(coldQuotaPostreadRequests(f))
			f.server.mu.Lock()
			f.server.values[key] = original
			f.server.mu.Unlock()
			f.fund()
			entry, err = coldQuotaPostreadStep(f, permit, state)
			require.NoError(t, err)
			require.Equal(t, phase == "receipt", entry != nil)
			requests := coldQuotaPostreadRequests(f)
			require.Len(t, requests, before+1)
			require.Equal(t, key, string(requests[len(requests)-1].Success[2].GetRequestRange().Key))
		})
	}
}

// A canonical committed receipt for the same locator but a different digest
// passes locator decoding; only exact original-reference equality rejects it.
func TestColdQuotaWorkerPostreadReceiptMustBindOriginalReference(t *testing.T) {
	for _, fault := range []string{"digest", "attempt", "birth", "mutable", "aborted", "noncanonical"} {
		t.Run(fault, func(t *testing.T) {
			f, permit, state := coldQuotaPostreadFixture(t)
			entry, err := coldQuotaPostreadStep(f, permit, state)
			require.NoError(t, err)
			require.Nil(t, entry)
			_, key, err := f.b.stageKeys(permit.reference)
			require.NoError(t, err)
			ref, outcome := permit.reference, OutcomeCommitted
			switch fault {
			case "digest":
				ref.Digest = strings.Repeat("0", 64)
			case "attempt":
				ref.AttemptID = "ffffffff-ffff-4fff-bfff-ffffffffffff"
			case "aborted":
				outcome = OutcomeAborted
			}
			wire, err := encodeReceipt(ref, outcome)
			require.NoError(t, err)
			f.server.mu.Lock()
			kv := f.server.values[key]
			kv.Value = []byte(wire)
			switch fault {
			case "birth":
				kv.CreateRevision--
				kv.ModRevision = kv.CreateRevision
			case "mutable":
				kv.Version = 2
			case "noncanonical":
				kv.Value = append(kv.Value, ' ')
			}
			f.server.mu.Unlock()
			f.fund()
			entry, err = coldQuotaPostreadStep(f, permit, state)
			require.Error(t, err)
			require.Nil(t, entry)
			require.Len(t, coldQuotaPostreadRequests(f), 2)
			coldQuotaWorkerCohortRequireRetained(t, f, permit.reference)
		})
	}
}

// Sealed state and original member identity are checked even after Q1. None of
// these local refusals may pay, dispatch, or adopt an unrelated valid member.
func TestColdQuotaWorkerPostreadRejectsRebinding(t *testing.T) {
	for _, afterQ1 := range []bool{false, true} {
		for _, fault := range []string{"nil-state", "fabricated-state", "state-copy", "permit-copy", "permit-reference", "permit-ack", "permit-ack-before-journal", "prepared-copy", "journal-copy", "mutation", "consumed", "journal-attempted", "owner-copy", "owner-context", "other-owner", "other-session", "other-partition", "other-range", "owner-finished"} {
			t.Run(fmt.Sprintf("afterQ1-%t/%s", afterQ1, fault), func(t *testing.T) {
				f, permit, state := coldQuotaPostreadFixture(t)
				if afterQ1 {
					entry, err := coldQuotaPostreadStep(f, permit, state)
					require.NoError(t, err)
					require.Nil(t, entry)
				}
				f.fund()
				owner, session, partition, ranges := f.owner, f.session, f.part, f.ranges
				switch fault {
				case "nil-state":
					state = nil
				case "fabricated-state":
					state = &coldQuotaWorkerPostread{}
				case "state-copy":
					copy := *state
					state = &copy
				case "permit-copy":
					copy := *permit
					permit = &copy
				case "permit-reference":
					permit.reference.Digest = strings.Repeat("0", 64)
				case "permit-ack":
					permit.commitACK.Revision = 0
				case "permit-ack-before-journal":
					permit.commitACK.Revision = permit.journal.journalACK.Revision - 1
				case "prepared-copy":
					copy := *permit.prepared
					permit.prepared = &copy
				case "journal-copy":
					copy := *permit.journal
					permit.journal = &copy
				case "mutation":
					f.prepared.mutation.Writes[0].Value = []byte("changed frozen mutation")
				case "consumed":
					atomic.StoreUint32(&f.prepared.consumed, 0)
				case "journal-attempted":
					atomic.StoreUint32(&f.prepared.journalAttempted, 0)
				case "owner-copy":
					copy := *owner
					owner = &copy
				case "owner-context":
					owner.context = context.Background()
				case "other-owner":
					var err error
					owner, err = f.b.registerEnrolledPublication(context.Background())
					require.NoError(t, err)
					defer owner.finish()
				case "other-session":
					var err error
					session, err = f.b.OpenWorkerSession(context.Background(), "postread-rebound")
					require.NoError(t, err)
					partition, err = f.b.ClaimWorkerPartition(context.Background(), session, f.part.partition+1)
					require.NoError(t, err)
					ranges, err = f.b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetRange, 1)
					require.NoError(t, err)
				case "other-partition":
					var err error
					partition, err = f.b.ClaimWorkerPartition(context.Background(), session, f.part.partition+1)
					require.NoError(t, err)
				case "other-range":
					var err error
					ranges, err = f.b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetRange, 1)
					require.NoError(t, err)
				case "owner-finished":
					owner.finish()
				}
				before := len(coldQuotaPostreadRequests(f))
				f.b.archiveLife.mu.Lock()
				// Give a different real Range member ample credit, so refusal
				// proves original binding rather than an unpaid substitute.
				if ranges != f.ranges {
					ranges.bucket = f.ranges.bucket
				}
				originalRange, originalClaim := f.ranges.bucket, f.claims.bucket
				f.b.archiveLife.mu.Unlock()
				entry, err := f.b.stepColdQuotaWorkerPostread(owner, permit, state, session, partition, ranges)
				require.Error(t, err)
				require.Nil(t, entry)
				require.Len(t, coldQuotaPostreadRequests(f), before)
				f.b.archiveLife.mu.Lock()
				afterRange, afterClaim := f.ranges.bucket, f.claims.bucket
				f.b.archiveLife.mu.Unlock()
				require.Equal(t, originalRange, afterRange)
				require.Equal(t, originalClaim, afterClaim)
			})
		}
	}
}

// A constructor cannot bind a trusted Commit from a different original worker
// cohort. Its lack of native work also leaves all admission credit untouched.
func TestColdQuotaWorkerPostreadConstructorRejectsForeignMembers(t *testing.T) {
	f, permit, _ := coldQuotaPostreadFixture(t)
	otherRange, err := f.b.AcquireWorkerBudget(context.Background(), f.session, WorkerBudgetRange, 1)
	require.NoError(t, err)
	f.b.archiveLife.mu.Lock()
	otherRange.bucket = f.ranges.bucket
	before := f.ranges.bucket
	f.b.archiveLife.mu.Unlock()
	requests := len(coldQuotaPostreadRequests(f))
	state, err := f.b.newColdQuotaWorkerPostread(f.owner, permit, f.session, f.part, otherRange)
	require.Error(t, err)
	require.Nil(t, state)
	require.Len(t, coldQuotaPostreadRequests(f), requests)
	f.b.archiveLife.mu.Lock()
	after := f.ranges.bucket
	f.b.archiveLife.mu.Unlock()
	require.Equal(t, before, after)
}

// Cached proof is ordinary data, and caching does not extend owner authority.
// An old caller cannot use done to bypass registration/context validation.
func TestColdQuotaWorkerPostreadDoneStillRequiresOriginalOwner(t *testing.T) {
	for _, mode := range []string{"finished", "cancelled", "permit-reference", "other-range"} {
		t.Run(mode, func(t *testing.T) {
			f, permit, state := coldQuotaPostreadFixture(t)
			entry, err := coldQuotaPostreadStep(f, permit, state)
			require.NoError(t, err)
			require.Nil(t, entry)
			f.fund()
			entry, err = coldQuotaPostreadStep(f, permit, state)
			require.NoError(t, err)
			require.NotNil(t, entry)
			switch mode {
			case "finished":
				f.owner.finish()
			case "cancelled":
				f.owner.cancel()
			case "permit-reference":
				permit.reference.Digest = strings.Repeat("0", 64)
			case "other-range":
				f.ranges, err = f.b.AcquireWorkerBudget(context.Background(), f.session, WorkerBudgetRange, 1)
				require.NoError(t, err)
			}
			before := len(coldQuotaPostreadRequests(f))
			entry, err = coldQuotaPostreadStep(f, permit, state)
			require.Error(t, err)
			require.Nil(t, entry)
			require.Len(t, coldQuotaPostreadRequests(f), before)
		})
	}
}

// The two business caps are narrower than metadata's transport envelope. A
// one-byte overrun must lose the session without publishing a quota proof.
func TestColdQuotaWorkerPostreadBusinessCaps(t *testing.T) {
	for _, afterQ1 := range []bool{false, true} {
		t.Run(fmt.Sprintf("afterQ1-%t", afterQ1), func(t *testing.T) {
			f, permit, state := coldQuotaPostreadFixture(t)
			key, cap := f.prepared.returnKey, 16384
			if afterQ1 {
				entry, err := coldQuotaPostreadStep(f, permit, state)
				require.NoError(t, err)
				require.Nil(t, entry)
				_, key, err = f.b.stageKeys(permit.reference)
				require.NoError(t, err)
				cap = 4096
				f.fund()
			}
			f.server.mu.Lock()
			f.server.values[key].Value = []byte(strings.Repeat("x", cap+1))
			f.server.mu.Unlock()
			entry, err := coldQuotaPostreadStep(f, permit, state)
			require.ErrorIs(t, err, ErrOutcomeUnknown)
			require.Nil(t, entry)
			f.b.archiveLife.mu.Lock()
			lost := f.session.lost
			f.b.archiveLife.mu.Unlock()
			require.True(t, lost)
		})
	}
}

// A lost native acknowledgement cannot become a saved typed phase or authorize
// a second call under the old session, even when the server already replied.
func TestColdQuotaWorkerPostreadNativeUnknownRetainsOriginalAttempt(t *testing.T) {
	for _, afterQ1 := range []bool{false, true} {
		t.Run(fmt.Sprintf("afterQ1-%t", afterQ1), func(t *testing.T) {
			f, permit, state := coldQuotaPostreadFixture(t)
			if afterQ1 {
				entry, err := coldQuotaPostreadStep(f, permit, state)
				require.NoError(t, err)
				require.Nil(t, entry)
				f.fund()
			}
			trace := coldQuotaWorkerCohortIntercept(t, f, func(_ context.Context, rpc coldQuotaWorkerCohortRPC, err error) error {
				if _, ok := rpc.request.(*pb.TxnRequest); ok && err == nil {
					return errors.New("lost original postread acknowledgement")
				}
				return err
			})
			entry, err := coldQuotaPostreadStep(f, permit, state)
			require.ErrorIs(t, err, ErrOutcomeUnknown)
			require.Nil(t, entry)
			f.fund()
			entry, err = coldQuotaPostreadStep(f, permit, state)
			require.Error(t, err)
			require.Nil(t, entry)
			require.Len(t, trace(), 1)
			coldQuotaWorkerCohortRequireRetained(t, f, permit.reference)
		})
	}
}

// Cancellation never releases state ownership or the worker call before the
// actual native return. A concurrent step cannot publish another phase/read.
func TestColdQuotaWorkerPostreadReleaseAndCloseJoinActualReturn(t *testing.T) {
	for _, mode := range []string{"release-session", "close"} {
		for _, afterQ1 := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/afterQ1-%t", mode, afterQ1), func(t *testing.T) {
				f, permit, state := coldQuotaPostreadFixture(t)
				if afterQ1 {
					entry, err := coldQuotaPostreadStep(f, permit, state)
					require.NoError(t, err)
					require.Nil(t, entry)
					f.fund()
				}
				entered, allow := make(chan struct{}), make(chan struct{})
				var release sync.Once
				defer release.Do(func() { close(allow) })
				trace := coldQuotaWorkerCohortIntercept(t, f, func(_ context.Context, rpc coldQuotaWorkerCohortRPC, err error) error {
					if _, ok := rpc.request.(*pb.TxnRequest); ok {
						close(entered)
						<-allow
					}
					return err
				})
				finished := make(chan error, 1)
				go func() {
					_, err := coldQuotaPostreadStep(f, permit, state)
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
				entry, err := coldQuotaPostreadStep(f, permit, state)
				require.ErrorIs(t, err, ErrConflict)
				require.Nil(t, entry)
				require.Len(t, trace(), 1)
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
					t.Fatal("postread finished before actual native return")
				default:
				}
				select {
				case err := <-stopped:
					t.Fatalf("%s returned before original postread: %v", mode, err)
				default:
				}
				release.Do(func() { close(allow) })
				require.ErrorIs(t, workerTestReceive(t, finished), ErrOutcomeUnknown)
				require.NoError(t, workerTestReceive(t, stopped))
				coldQuotaJournalAwait(t, call.done)
			})
		}
	}
}
