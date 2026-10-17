package etcd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// The existing decoded protocol history is borrowed input. Original registry
// members and all paid calls use the real backend/gRPC boundary; this fixture
// proves neither physical release nor production worker integration.
func coldQuotaRecoveryFixture(t *testing.T, committed bool) (*coldQuotaWorkerCohortFixture, *coldQuotaWorkerRecovery) {
	t.Helper()
	var f *coldQuotaWorkerCohortFixture
	if committed {
		f = newColdQuotaWorkerCohortFixture(t)
		_, err := f.run()
		require.NoError(t, err)
	} else {
		j := newColdQuotaWorkerJournalFixture(t)
		f = &coldQuotaWorkerCohortFixture{b: j.b, server: j.server, owner: j.owner, prepared: j.prepared, session: j.session, part: j.part, ranges: j.ranges, claims: j.claims}
	}
	f.fund()
	f.grants, _, f.revokes = f.server.counts()
	f.server.mu.Lock()
	f.server.requests = nil
	f.server.mu.Unlock()
	state, err := f.b.newColdQuotaWorkerRecovery(f.owner, f.prepared.history, f.session, f.part, f.ranges, f.claims)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Empty(t, coldQuotaPostreadRequests(f), "construction must not read, pay, or resolve")
	return f, state
}

func coldQuotaRecoveryStep(f *coldQuotaWorkerCohortFixture, state *coldQuotaWorkerRecovery) (*coldQuotaWorkerRecoveryResult, error) {
	return f.b.stepColdQuotaWorkerRecovery(f.owner, f.prepared.history, state, f.session, f.part, f.ranges, f.claims)
}

func coldQuotaRecoveryYield(t *testing.T, f *coldQuotaWorkerCohortFixture, state *coldQuotaWorkerRecovery) {
	t.Helper()
	f.fund()
	before := len(coldQuotaPostreadRequests(f))
	result, err := coldQuotaRecoveryStep(f, state)
	require.NoError(t, err)
	require.Nil(t, result, "one paid operation must yield before its next phase")
	require.Len(t, coldQuotaPostreadRequests(f), before+1)
}

func coldQuotaRecoveryRequirePoint(t *testing.T, f *coldQuotaWorkerCohortFixture, index int, key string) {
	t.Helper()
	request := coldQuotaPostreadRequests(f)[index]
	require.Equal(t, taskProtoPoints([]string{f.b.identityKey, f.b.restoreKey, key}), request.Success)
	require.Equal(t, request.Success, request.Failure)
	require.Len(t, request.Compare, 20)
	for _, fence := range []taskFence{f.b.workerControl.profile, f.session.guard, f.part.fence, f.ranges.fence} {
		coldQuotaWorkerJournalRequireFence(t, request, fence)
	}
}

func coldQuotaRecoveryRequireRetained(t *testing.T, f *coldQuotaWorkerCohortFixture) {
	t.Helper()
	grants, _, revokes := f.server.counts()
	require.Equal(t, f.grants, grants, "recovery never Grants a new attempt")
	require.Equal(t, f.revokes, revokes)
	f.b.archiveLife.mu.Lock()
	_, registered := f.b.archiveLife.enrolled[f.owner]
	operation := f.session.operation
	f.b.archiveLife.mu.Unlock()
	require.True(t, registered)
	require.Nil(t, operation)
}

// Skipping the first return check, combining reads, spending Claim on metadata,
// or constructing an attempt before the existing builder breaks this trace.
func TestColdQuotaWorkerRecoveryAbsentReturnsOrdinaryFreshData(t *testing.T) {
	f, state := coldQuotaRecoveryFixture(t, false)
	claimBefore := f.claims.bucket
	result, err := coldQuotaRecoveryStep(f, state)
	require.NoError(t, err)
	require.Nil(t, result, "one paid operation must yield before its next phase")
	require.Len(t, coldQuotaPostreadRequests(f), 1)
	coldQuotaRecoveryRequirePoint(t, f, 0, f.prepared.returnKey)
	require.Equal(t, claimBefore, f.claims.bucket)
	require.Zero(t, f.ranges.bucket.tokens)
	result, err = coldQuotaRecoveryStep(f, state)
	require.ErrorIs(t, err, ErrConflict)
	require.Nil(t, result)
	require.Len(t, coldQuotaPostreadRequests(f), 1)
	// Only Range earns this metadata retry. Preserve the original future
	// earning origin and every Claim field across the complete recovery path.
	f.b.archiveLife.mu.Lock()
	f.ranges.bucket.tokens = 262144
	f.b.archiveLife.mu.Unlock()
	result, err = coldQuotaRecoveryStep(f, state)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Nil(t, result.entry)
	require.Nil(t, result.previous)
	require.Nil(t, result.aborted)
	coldQuotaRecoveryRequirePoint(t, f, 1, coldQuotaJournalKey(t, f.b, f.prepared))
	require.Equal(t, claimBefore, f.claims.bucket)
	result.previous = &mvccpb.KeyValue{Value: []byte("caller changed result")}
	result, err = coldQuotaRecoveryStep(f, state)
	require.NoError(t, err)
	require.Nil(t, result.previous)
	require.Len(t, coldQuotaPostreadRequests(f), 2)
	guard, receipt, err := f.b.stageKeys(f.prepared.reference)
	require.NoError(t, err)
	require.Nil(t, f.server.point(guard))
	require.Nil(t, f.server.point(receipt))
	require.Nil(t, f.server.point(coldQuotaJournalKey(t, f.b, f.prepared)))
	coldQuotaRecoveryRequireRetained(t, f)
}

// A committed return wins before journal/slot inspection, including after slot
// reuse. No Commit permit is passed to this independent historical recovery.
func TestColdQuotaWorkerRecoveryEarlyReplayBeforeJournalAndSlot(t *testing.T) {
	f, state := coldQuotaRecoveryFixture(t, true)
	want := f.server.point(f.prepared.returnKey)
	f.server.put(coldQuotaJournalKey(t, f.b, f.prepared), "malformed newer journal", 0)
	slot, _, err := f.b.namespace.coldSlotKeys(f.prepared.history.reservation.Quota)
	require.NoError(t, err)
	f.server.put(slot, "occupied by later incarnation", 0)
	coldQuotaRecoveryYield(t, f, state)
	_, receipt, err := f.b.stageKeys(f.prepared.reference)
	require.NoError(t, err)
	f.fund()
	result, err := coldQuotaRecoveryStep(f, state)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.entry)
	require.Equal(t, want.CreateRevision, result.entry.CreateRevision)
	require.Equal(t, f.prepared.history.reservation.Owner, result.entry.Owner)
	require.Nil(t, result.previous)
	require.Nil(t, result.aborted)
	coldQuotaRecoveryRequirePoint(t, f, 0, f.prepared.returnKey)
	coldQuotaRecoveryRequirePoint(t, f, 1, receipt)
	wantEntry := *result.entry
	result.entry.SHA256 = "caller mutation"
	result, err = coldQuotaRecoveryStep(f, state)
	require.NoError(t, err)
	require.Equal(t, &wantEntry, result.entry)
	require.Len(t, coldQuotaPostreadRequests(f), 2)
	coldQuotaRecoveryRequireRetained(t, f)
}

// Arbitration's aborted outcome is ordinary data. Returning fresh-ready at
// that point, rereading a replacement journal, or omitting the paid receipt
// authentication would permit replacing the wrong original attempt.
func TestColdQuotaWorkerRecoveryAbortNeedsSeparateOriginalReceiptRead(t *testing.T) {
	f, state := coldQuotaRecoveryFixture(t, false)
	journal := coldQuotaJournalInstall(t, f.b, f.server, f.prepared)
	ref := f.prepared.reference
	_, receipt, err := f.b.stageKeys(ref)
	require.NoError(t, err)
	coldQuotaRecoveryYield(t, f, state)
	coldQuotaRecoveryYield(t, f, state)
	require.Equal(t, ref, state.reference)
	require.Equal(t, journal, state.journal)
	for _, kind := range []string{"range", "claim"} {
		f.fund()
		if kind == "range" {
			f.ranges.bucket.tokens = 262143
		} else {
			f.claims.bucket.tokens = 0
		}
		beforeRange, beforeClaim := f.ranges.bucket, f.claims.bucket
		result, err := coldQuotaRecoveryStep(f, state)
		require.ErrorIs(t, err, ErrConflict)
		require.Nil(t, result)
		require.Equal(t, beforeRange, f.ranges.bucket)
		require.Equal(t, beforeClaim, f.claims.bucket)
		require.Equal(t, ref, state.reference)
		require.Len(t, coldQuotaPostreadRequests(f), 2)
	}
	coldQuotaRecoveryYield(t, f, state)
	request := coldQuotaPostreadRequests(f)[2]
	require.Len(t, request.Compare, 25)
	require.Len(t, request.Success, 1)
	require.Equal(t, receipt, string(request.Success[0].GetRequestPut().Key))
	require.Zero(t, f.ranges.bucket.tokens)
	require.Zero(t, f.claims.bucket.tokens)
	for _, fence := range []taskFence{f.b.workerControl.profile, f.session.guard, f.part.fence, f.ranges.fence, f.claims.fence} {
		coldQuotaWorkerJournalRequireFence(t, request, fence)
	}
	aborted := f.server.point(receipt)
	require.NotNil(t, aborted)
	// The cursor owns the original paid snapshot even if another actor changes
	// the durable journal after arbitration. Recovery never rereads that key.
	f.server.put(string(journal.Key), "later actor's journal", 0)
	f.fund()
	result, err := coldQuotaRecoveryStep(f, state)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Nil(t, result.entry)
	require.Same(t, state.journal, result.previous)
	require.Equal(t, journal, result.previous)
	require.Equal(t, aborted, result.aborted)
	require.NoError(t, coldQuotaAttemptAbortedReceipt(result.previous, result.aborted, ref))
	coldQuotaRecoveryRequirePoint(t, f, 3, receipt)
	require.Len(t, coldQuotaPostreadRequests(f), 4)
	require.Nil(t, f.server.point(f.prepared.returnKey))
	coldQuotaRecoveryRequireRetained(t, f)
}

// The original pending reference must survive Resolve and constrain the full
// final receipt, including digest fields absent from a locator-only stamp.
func TestColdQuotaWorkerRecoveryCommittedResolveUsesOriginalFullReference(t *testing.T) {
	for _, corruptDigest := range []bool{false, true} {
		t.Run(fmt.Sprintf("different-digest-%t", corruptDigest), func(t *testing.T) {
			f, state := coldQuotaRecoveryFixture(t, true)
			returned := f.server.point(f.prepared.returnKey)
			f.server.mu.Lock()
			delete(f.server.values, f.prepared.returnKey)
			f.server.mu.Unlock()
			coldQuotaRecoveryYield(t, f, state)
			coldQuotaRecoveryYield(t, f, state)
			ref := f.prepared.reference
			require.Equal(t, ref, state.reference)
			coldQuotaRecoveryYield(t, f, state)
			f.server.mu.Lock()
			f.server.values[f.prepared.returnKey] = returned
			f.server.mu.Unlock()
			coldQuotaRecoveryYield(t, f, state)
			_, receipt, err := f.b.stageKeys(ref)
			require.NoError(t, err)
			if corruptDigest {
				other := ref
				other.Digest = strings.Repeat("0", 64)
				wire, err := encodeReceipt(other, OutcomeCommitted)
				require.NoError(t, err)
				f.server.mu.Lock()
				f.server.values[receipt].Value = []byte(wire)
				f.server.mu.Unlock()
			}
			f.fund()
			result, err := coldQuotaRecoveryStep(f, state)
			if corruptDigest {
				require.Error(t, err)
				require.Nil(t, result)
			} else {
				require.NoError(t, err)
				require.NotNil(t, result.entry)
				require.Equal(t, returned.CreateRevision, result.entry.CreateRevision)
			}
			require.Equal(t, ref, state.reference)
			coldQuotaRecoveryRequirePoint(t, f, 0, f.prepared.returnKey)
			coldQuotaRecoveryRequirePoint(t, f, 1, coldQuotaJournalKey(t, f.b, f.prepared))
			coldQuotaRecoveryRequirePoint(t, f, 3, f.prepared.returnKey)
			coldQuotaRecoveryRequirePoint(t, f, 4, receipt)
			require.Len(t, coldQuotaPostreadRequests(f), 5)
			coldQuotaRecoveryRequireRetained(t, f)
		})
	}
}

// Each paid point can refuse credit without restarting or skipping the saved
// phase. Typed malformed records also leave that exact point retryable.
func TestColdQuotaWorkerRecoveryPointFailuresKeepExactPhase(t *testing.T) {
	for _, phase := range []string{"initial-return", "journal", "early-receipt", "aborted-receipt", "committed-return", "committed-receipt"} {
		t.Run(phase, func(t *testing.T) {
			committed := phase == "early-receipt" || phase == "committed-return" || phase == "committed-receipt"
			f, state := coldQuotaRecoveryFixture(t, committed)
			key := f.prepared.returnKey
			var returned *mvccpb.KeyValue
			if phase == "journal" || phase == "aborted-receipt" {
				coldQuotaJournalInstall(t, f.b, f.server, f.prepared)
			}
			if strings.HasPrefix(phase, "committed-") {
				returned = f.server.point(key)
				f.server.mu.Lock()
				delete(f.server.values, key)
				f.server.mu.Unlock()
			}
			if phase != "initial-return" {
				coldQuotaRecoveryYield(t, f, state)
				key = coldQuotaJournalKey(t, f.b, f.prepared)
			}
			if phase == "aborted-receipt" || strings.HasPrefix(phase, "committed-") {
				coldQuotaRecoveryYield(t, f, state)
				coldQuotaRecoveryYield(t, f, state)
				_, key, _ = f.b.stageKeys(f.prepared.reference)
			}
			if strings.HasPrefix(phase, "committed-") {
				f.server.mu.Lock()
				f.server.values[f.prepared.returnKey] = returned
				f.server.mu.Unlock()
				key = f.prepared.returnKey
				if phase == "committed-receipt" {
					coldQuotaRecoveryYield(t, f, state)
					_, key, _ = f.b.stageKeys(f.prepared.reference)
				}
			}
			if phase == "early-receipt" {
				_, key, _ = f.b.stageKeys(f.prepared.reference)
			}
			before := len(coldQuotaPostreadRequests(f))
			f.fund()
			f.ranges.bucket.tokens = 262143
			beforeRange, beforeClaim := f.ranges.bucket, f.claims.bucket
			result, err := coldQuotaRecoveryStep(f, state)
			require.ErrorIs(t, err, ErrConflict)
			require.Nil(t, result)
			require.Equal(t, beforeRange, f.ranges.bucket)
			require.Equal(t, beforeClaim, f.claims.bucket)
			require.Len(t, coldQuotaPostreadRequests(f), before)
			original := f.server.point(key)
			f.server.put(key, "malformed typed point", 0)
			f.fund()
			result, err = coldQuotaRecoveryStep(f, state)
			require.Error(t, err)
			require.Nil(t, result)
			coldQuotaRecoveryRequirePoint(t, f, before, key)
			f.server.mu.Lock()
			if original == nil {
				delete(f.server.values, key)
			} else {
				f.server.values[key] = original
			}
			f.server.mu.Unlock()
			f.fund()
			_, err = coldQuotaRecoveryStep(f, state)
			require.NoError(t, err)
			coldQuotaRecoveryRequirePoint(t, f, before+1, key)
			require.Len(t, coldQuotaPostreadRequests(f), before+2)
		})
	}
}

// Canonical history validation must precede paid Resolve; unknown arbitration
// keeps the original durable journal rather than making a replacement attempt.
func TestColdQuotaWorkerRecoveryUnknownRetainsOriginalJournalAndReference(t *testing.T) {
	f, state := coldQuotaRecoveryFixture(t, false)
	journal := coldQuotaJournalInstall(t, f.b, f.server, f.prepared)
	coldQuotaRecoveryYield(t, f, state)
	coldQuotaRecoveryYield(t, f, state)
	trace := coldQuotaWorkerCohortIntercept(t, f, func(_ context.Context, rpc coldQuotaWorkerCohortRPC, err error) error {
		if _, ok := rpc.request.(*pb.TxnRequest); ok && err == nil {
			return errors.New("lost original recovery Resolve acknowledgement")
		}
		return err
	})
	f.fund()
	result, err := coldQuotaRecoveryStep(f, state)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Nil(t, result)
	require.Equal(t, f.prepared.reference, state.reference)
	require.Equal(t, journal, state.journal)
	require.Equal(t, journal, f.server.point(string(journal.Key)))
	f.fund()
	result, err = coldQuotaRecoveryStep(f, state)
	require.Error(t, err, "a lost original session cannot continue recovery")
	require.Nil(t, result)
	require.Len(t, trace(), 1)
	require.Equal(t, f.prepared.reference, state.reference)
	coldQuotaRecoveryRequireRetained(t, f)
}

// A bounded journal point is still untrusted until exact canonical attribution
// is decoded. No malformed record may reach the paid Resolve transaction.
func TestColdQuotaWorkerRecoveryCanonicalJournalBeforeResolve(t *testing.T) {
	for _, fault := range []string{"noncanonical", "different-stage"} {
		t.Run(fault, func(t *testing.T) {
			f, state := coldQuotaRecoveryFixture(t, false)
			original := coldQuotaJournalInstall(t, f.b, f.server, f.prepared)
			coldQuotaRecoveryYield(t, f, state)
			f.server.mu.Lock()
			kv := f.server.values[string(original.Key)]
			if fault == "noncanonical" {
				kv.Value = append(kv.Value, ' ')
			} else {
				kv.Value = []byte(strings.Replace(string(kv.Value), "cold_runtime_quota_return", "other_stage", 1))
			}
			f.server.mu.Unlock()
			f.fund()
			result, err := coldQuotaRecoveryStep(f, state)
			require.Error(t, err)
			require.Nil(t, result)
			require.Zero(t, state.reference)
			require.Nil(t, state.journal)
			require.Equal(t, uint64(1), f.claims.bucket.tokens)
			require.Len(t, coldQuotaPostreadRequests(f), 2)
			coldQuotaRecoveryRequirePoint(t, f, 1, string(original.Key))
			f.server.mu.Lock()
			f.server.values[string(original.Key)] = original
			f.server.mu.Unlock()
			coldQuotaRecoveryYield(t, f, state)
			require.Equal(t, f.prepared.reference, state.reference)
			require.Equal(t, original, state.journal)
			coldQuotaRecoveryRequirePoint(t, f, 2, string(original.Key))
		})
	}
}

// An outcome from Resolve cannot stand in for the next independently paid,
// immutable canonical receipt. Invalid rereads never release fresh-ready data.
func TestColdQuotaWorkerRecoveryAbortedReceiptMustAuthenticateOriginalJournal(t *testing.T) {
	for _, fault := range []string{"absent", "too-old", "mutable", "committed", "different-digest", "noncanonical"} {
		t.Run(fault, func(t *testing.T) {
			f, state := coldQuotaRecoveryFixture(t, false)
			journal := coldQuotaJournalInstall(t, f.b, f.server, f.prepared)
			coldQuotaRecoveryYield(t, f, state)
			coldQuotaRecoveryYield(t, f, state)
			coldQuotaRecoveryYield(t, f, state)
			ref := f.prepared.reference
			_, key, err := f.b.stageKeys(ref)
			require.NoError(t, err)
			original := f.server.point(key)
			f.server.mu.Lock()
			kv := f.server.values[key]
			switch fault {
			case "absent":
				delete(f.server.values, key)
			case "too-old":
				kv.CreateRevision, kv.ModRevision = journal.ModRevision, journal.ModRevision
			case "mutable":
				kv.Version = 2
			case "committed":
				wire, err := encodeReceipt(ref, OutcomeCommitted)
				require.NoError(t, err)
				kv.Value = []byte(wire)
			case "different-digest":
				other := ref
				other.Digest = strings.Repeat("0", 64)
				wire, err := encodeReceipt(other, OutcomeAborted)
				require.NoError(t, err)
				kv.Value = []byte(wire)
			case "noncanonical":
				kv.Value = append(kv.Value, ' ')
			}
			f.server.mu.Unlock()
			f.fund()
			result, err := coldQuotaRecoveryStep(f, state)
			require.Error(t, err)
			require.Nil(t, result)
			require.Equal(t, ref, state.reference)
			require.Equal(t, journal, state.journal)
			coldQuotaRecoveryRequirePoint(t, f, 3, key)
			f.server.mu.Lock()
			f.server.values[key] = original
			f.server.mu.Unlock()
			f.fund()
			result, err = coldQuotaRecoveryStep(f, state)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Nil(t, result.entry)
			require.Equal(t, original, result.aborted)
			coldQuotaRecoveryRequirePoint(t, f, 4, key)
			require.Len(t, coldQuotaPostreadRequests(f), 5)
		})
	}
}

// Local construction rejects incomplete or mismatched borrowed history before
// reading or spending credit. No identity can be repaired with native reads.
func TestColdQuotaWorkerRecoveryConstructorRejectsIncompleteHistoryAndMembers(t *testing.T) {
	for _, fault := range []string{"nil-history", "nil-publication", "nil-entry", "nil-publication-journal", "nil-publication-receipt", "nil-release", "nil-release-kv", "nil-reserved", "nil-work", "nil-settled", "partition", "owner-copy", "range-copy", "nil-claim", "claim-as-range"} {
		t.Run(fault, func(t *testing.T) {
			f, _ := coldQuotaRecoveryFixture(t, false)
			h, owner, ranges, claims := f.prepared.history, f.owner, f.ranges, f.claims
			switch fault {
			case "nil-history":
				h = nil
			case "nil-publication":
				h.publication = nil
			case "nil-entry":
				h.publication.entry = nil
			case "nil-publication-journal":
				h.publication.journal = nil
			case "nil-publication-receipt":
				h.publication.receipt = nil
			case "nil-release":
				h.release = nil
			case "nil-release-kv":
				h.release.kv = nil
			case "nil-reserved":
				h.reservedKV = nil
			case "nil-work":
				h.workKV = nil
			case "nil-settled":
				h.settledKV = nil
			case "partition":
				h.reservation.Owner.Partition++
			case "owner-copy":
				copy := *owner
				owner = &copy
			case "range-copy":
				copy := *ranges
				ranges = &copy
			case "nil-claim":
				claims = nil
			case "claim-as-range":
				ranges = claims
			}
			beforeRange, beforeClaim := f.ranges.bucket, f.claims.bucket
			state, err := f.b.newColdQuotaWorkerRecovery(owner, h, f.session, f.part, ranges, claims)
			require.Error(t, err)
			require.Nil(t, state)
			require.Empty(t, coldQuotaPostreadRequests(f))
			require.Equal(t, beforeRange, f.ranges.bucket)
			require.Equal(t, beforeClaim, f.claims.bucket)
		})
	}
}

// Business caps apply on each path, below metadata's larger transport bound.
// A one-byte excess loses the old session and publishes no result or phase.
func TestColdQuotaWorkerRecoveryBusinessPointCaps(t *testing.T) {
	for _, phase := range []string{"initial-return", "journal", "early-receipt", "aborted-receipt", "committed-return", "committed-receipt"} {
		t.Run(phase, func(t *testing.T) {
			committed := phase == "early-receipt" || strings.HasPrefix(phase, "committed-")
			f, state := coldQuotaRecoveryFixture(t, committed)
			key, limit := f.prepared.returnKey, 16384
			var returned *mvccpb.KeyValue
			if phase == "aborted-receipt" {
				coldQuotaJournalInstall(t, f.b, f.server, f.prepared)
			}
			if strings.HasPrefix(phase, "committed-") {
				returned = f.server.point(key)
				f.server.mu.Lock()
				delete(f.server.values, key)
				f.server.mu.Unlock()
			}
			if phase != "initial-return" {
				coldQuotaRecoveryYield(t, f, state)
				key, limit = coldQuotaJournalKey(t, f.b, f.prepared), 4096
			}
			if phase == "aborted-receipt" || strings.HasPrefix(phase, "committed-") {
				coldQuotaRecoveryYield(t, f, state)
				coldQuotaRecoveryYield(t, f, state)
				_, key, _ = f.b.stageKeys(f.prepared.reference)
			}
			if strings.HasPrefix(phase, "committed-") {
				f.server.mu.Lock()
				f.server.values[f.prepared.returnKey] = returned
				f.server.mu.Unlock()
				key, limit = f.prepared.returnKey, 16384
				if phase == "committed-receipt" {
					coldQuotaRecoveryYield(t, f, state)
					_, key, _ = f.b.stageKeys(f.prepared.reference)
					limit = 4096
				}
			}
			if phase == "early-receipt" {
				_, key, _ = f.b.stageKeys(f.prepared.reference)
			}
			f.server.put(key, strings.Repeat("x", limit+1), 0)
			f.fund()
			before := len(coldQuotaPostreadRequests(f))
			result, err := coldQuotaRecoveryStep(f, state)
			require.ErrorIs(t, err, ErrOutcomeUnknown)
			require.Nil(t, result)
			require.Len(t, coldQuotaPostreadRequests(f), before+1)
			f.b.archiveLife.mu.Lock()
			lost := f.session.lost
			f.b.archiveLife.mu.Unlock()
			require.True(t, lost)
			f.fund()
			result, err = coldQuotaRecoveryStep(f, state)
			require.Error(t, err)
			require.Nil(t, result)
			require.Len(t, coldQuotaPostreadRequests(f), before+1)
		})
	}
}

// Copies, pointer rebinding, owner-context substitution and cached done cannot
// extend the original enrollment or switch to another valid worker cohort.
func TestColdQuotaWorkerRecoveryRejectsRebindingInEveryState(t *testing.T) {
	for _, progress := range []string{"initial", "journal", "resolve", "done"} {
		for _, fault := range []string{"state-copy", "history-copy", "history-owner", "history-task", "owner-copy", "owner-context", "other-session", "other-partition", "other-range", "other-claim", "finished"} {
			t.Run(progress+"/"+fault, func(t *testing.T) {
				f, state := coldQuotaRecoveryFixture(t, false)
				if progress == "resolve" {
					coldQuotaJournalInstall(t, f.b, f.server, f.prepared)
				}
				if progress != "initial" {
					coldQuotaRecoveryYield(t, f, state)
				}
				if progress == "resolve" || progress == "done" {
					f.fund()
					_, err := coldQuotaRecoveryStep(f, state)
					require.NoError(t, err)
				}
				h, owner, ranges, claims := f.prepared.history, f.owner, f.ranges, f.claims
				session, partition := f.session, f.part
				switch fault {
				case "state-copy":
					copy := *state
					state = &copy
				case "history-copy":
					copy := *h
					h = &copy
				case "history-owner":
					h.reservation.Owner.IntentID = "different-intent"
				case "history-task":
					h.release.record.TaskBirth++
				case "owner-copy":
					copy := *owner
					owner = &copy
				case "owner-context":
					owner.context = context.Background()
				case "other-session":
					var err error
					session, err = f.b.OpenWorkerSession(context.Background(), "other-recovery-session")
					require.NoError(t, err)
				case "other-partition":
					var err error
					partition, err = f.b.ClaimWorkerPartition(context.Background(), f.session, f.part.partition+1)
					require.NoError(t, err)
				case "other-range":
					var err error
					ranges, err = f.b.AcquireWorkerBudget(context.Background(), f.session, WorkerBudgetRange, 1)
					require.NoError(t, err)
				case "other-claim":
					var err error
					claims, err = f.b.AcquireWorkerBudget(context.Background(), f.session, WorkerBudgetClaim, 1)
					require.NoError(t, err)
				case "finished":
					owner.finish()
				}
				f.fund()
				before := len(coldQuotaPostreadRequests(f))
				beforeRange, beforeClaim := f.ranges.bucket, f.claims.bucket
				result, err := f.b.stepColdQuotaWorkerRecovery(owner, h, state, session, partition, ranges, claims)
				require.Error(t, err)
				require.Nil(t, result)
				require.Len(t, coldQuotaPostreadRequests(f), before)
				require.Equal(t, beforeRange, f.ranges.bucket)
				require.Equal(t, beforeClaim, f.claims.bucket)
			})
		}
	}
}

// Actual client return, rather than cancellation or server-side completion,
// ends the nonblocking busy interval and releases the original worker call.
func TestColdQuotaWorkerRecoveryReleaseAndCloseJoinActualReturn(t *testing.T) {
	for _, mode := range []string{"release-session", "close"} {
		for _, resolving := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/resolve-%t", mode, resolving), func(t *testing.T) {
				f, state := coldQuotaRecoveryFixture(t, false)
				if resolving {
					coldQuotaJournalInstall(t, f.b, f.server, f.prepared)
					coldQuotaRecoveryYield(t, f, state)
					coldQuotaRecoveryYield(t, f, state)
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
					_, err := coldQuotaRecoveryStep(f, state)
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
				result, err := coldQuotaRecoveryStep(f, state)
				require.ErrorIs(t, err, ErrConflict)
				require.Nil(t, result)
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
					t.Fatal("recovery call joined before its actual native return")
				default:
				}
				select {
				case err := <-stopped:
					t.Fatalf("%s returned before recovery joined: %v", mode, err)
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
