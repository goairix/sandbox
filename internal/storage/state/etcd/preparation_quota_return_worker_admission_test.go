package etcd

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type coldQuotaAdmissionContextKey struct{}

// The host model creates the original leased session and child capabilities.
// Only earned bucket state is installed locally; admission itself performs no
// native operation and this fixture supplies no quota or physical authority.
type coldQuotaAdmissionFixture struct {
	b       *Backend
	server  *workerTestServer
	owner   *runtimeEnrolledPublicationOwner
	session *WorkerSession
	part    *WorkerPartitionClaim
	ranges  *WorkerBudgetShare
	claims  *WorkerBudgetShare
}

func newColdQuotaAdmissionFixture(t *testing.T) *coldQuotaAdmissionFixture {
	t.Helper()
	b, server := workerTestBackend(t)
	b.enrolledPublication = &EnrolledPublicationOptions{MaxActive: 4}
	parent := context.WithValue(context.Background(), coldQuotaAdmissionContextKey{}, "original-enrolled-owner")
	owner, err := b.registerEnrolledPublication(parent)
	require.NoError(t, err)
	t.Cleanup(func() { owner.finish() })

	session, err := b.OpenWorkerSession(context.Background(), "quota-admission")
	require.NoError(t, err)
	part, err := b.ClaimWorkerPartition(context.Background(), session, 7)
	require.NoError(t, err)
	ranges, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetRange, 0)
	require.NoError(t, err)
	claims, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetClaim, 0)
	require.NoError(t, err)

	// Keep earning frozen during the assertions. Pure workerBucket tests cover
	// earning; no wall-clock sleep or bucket replacement occurs in admission.
	b.archiveLife.mu.Lock()
	frozen := time.Now().Add(time.Hour)
	ranges.bucket.last, ranges.bucket.tokens = frozen, 262144
	claims.bucket.last, claims.bucket.tokens = frozen, 1
	b.archiveLife.mu.Unlock()
	return &coldQuotaAdmissionFixture{b: b, server: server, owner: owner, session: session, part: part, ranges: ranges, claims: claims}
}

func (f *coldQuotaAdmissionFixture) begin() (*workerCall, []clientv3.Cmp, error) {
	return f.b.beginColdQuotaWorkerCall(f.owner, f.session, f.part, f.ranges, f.claims)
}

func TestColdQuotaWorkerAdmissionChargesBothOriginalBuckets(t *testing.T) {
	f := newColdQuotaAdmissionFixture(t)
	originalContext := f.owner.context
	call, comparisons, err := f.begin()
	require.NoError(t, err)
	defer call.finish()

	f.b.archiveLife.mu.Lock()
	current := f.session.operation
	rangeTokens, claimTokens := f.ranges.bucket.tokens, f.claims.bucket.tokens
	expected := append(f.b.baseComparisons(), f.b.workerControl.profile.comparisons()...)
	for _, fence := range []taskFence{f.session.guard, f.part.fence, f.ranges.fence, f.claims.fence} {
		expected = append(expected, fence.comparisons()...)
	}
	f.b.archiveLife.mu.Unlock()
	require.Same(t, call, current)
	require.Same(t, f.session, call.session)
	require.Same(t, originalContext, f.owner.context)
	require.Equal(t, "original-enrolled-owner", call.ctx.Value(coldQuotaAdmissionContextKey{}))
	require.Zero(t, rangeTokens)
	require.Zero(t, claimTokens)
	require.Len(t, comparisons, 24, "W24 retains base4 and every full original fence")
	require.Equal(t, taskProtoComparisons(expected), taskProtoComparisons(comparisons))
	require.False(t, call.started, "paid local admission must not invent a native RPC")

	second, secondComparisons, err := f.begin()
	require.ErrorIs(t, err, ErrConflict)
	require.Nil(t, second)
	require.Nil(t, secondComparisons)
}

func TestColdQuotaWorkerAdmissionInsufficientCreditIsAtomic(t *testing.T) {
	for _, deficient := range []string{"range", "claim"} {
		t.Run(deficient, func(t *testing.T) {
			f := newColdQuotaAdmissionFixture(t)
			f.b.archiveLife.mu.Lock()
			if deficient == "range" {
				f.ranges.bucket.tokens = 262143
			} else {
				f.claims.bucket.tokens = 0
			}
			ranges, claims := f.ranges.bucket, f.claims.bucket
			f.b.archiveLife.mu.Unlock()

			call, comparisons, err := f.begin()
			require.ErrorIs(t, err, ErrConflict)
			require.Nil(t, call)
			require.Nil(t, comparisons)
			f.b.archiveLife.mu.Lock()
			afterRanges, afterClaims, operation := f.ranges.bucket, f.claims.bucket, f.session.operation
			f.b.archiveLife.mu.Unlock()
			require.Equal(t, ranges, afterRanges)
			require.Equal(t, claims, afterClaims)
			require.Nil(t, operation)
		})
	}
}

func TestColdQuotaWorkerAdmissionRefusesInvalidBindingsWithoutCharge(t *testing.T) {
	tests := map[string]func(*testing.T, *coldQuotaAdmissionFixture){
		"nil-owner":       func(_ *testing.T, f *coldQuotaAdmissionFixture) { f.owner = nil },
		"nil-session":     func(_ *testing.T, f *coldQuotaAdmissionFixture) { f.session = nil },
		"nil-partition":   func(_ *testing.T, f *coldQuotaAdmissionFixture) { f.part = nil },
		"nil-range":       func(_ *testing.T, f *coldQuotaAdmissionFixture) { f.ranges = nil },
		"nil-claim":       func(_ *testing.T, f *coldQuotaAdmissionFixture) { f.claims = nil },
		"foreign-backend": func(t *testing.T, f *coldQuotaAdmissionFixture) { f.b, _ = workerTestBackend(t) },
		"copied-owner": func(_ *testing.T, f *coldQuotaAdmissionFixture) {
			copy := *f.owner
			f.owner = &copy
		},
		"copied-session": func(_ *testing.T, f *coldQuotaAdmissionFixture) {
			copy := *f.session
			copy.self = &copy
			f.session = &copy
		},
		"copied-partition": func(_ *testing.T, f *coldQuotaAdmissionFixture) {
			copy := *f.part
			copy.self = &copy
			f.part = &copy
		},
		"copied-range": func(_ *testing.T, f *coldQuotaAdmissionFixture) {
			copy := *f.ranges
			copy.self = &copy
			f.ranges = &copy
		},
		"copied-claim": func(_ *testing.T, f *coldQuotaAdmissionFixture) {
			copy := *f.claims
			copy.self = &copy
			f.claims = &copy
		},
		"swapped-roles": func(_ *testing.T, f *coldQuotaAdmissionFixture) { f.ranges, f.claims = f.claims, f.ranges },
		"nil-owner-context": func(t *testing.T, f *coldQuotaAdmissionFixture) {
			owner, original := f.owner, f.owner.context
			owner.context = nil
			t.Cleanup(func() { owner.context = original })
		},
		"unregistered-owner": func(t *testing.T, f *coldQuotaAdmissionFixture) {
			b, owner := f.b, f.owner
			delete(b.archiveLife.enrolled, owner)
			t.Cleanup(func() { b.archiveLife.enrolled[owner] = struct{}{} })
		},
		"canceled-owner":   func(_ *testing.T, f *coldQuotaAdmissionFixture) { f.owner.cancel() },
		"canceled-session": func(_ *testing.T, f *coldQuotaAdmissionFixture) { f.session.cancel() },
		"expired-deadline": func(_ *testing.T, f *coldQuotaAdmissionFixture) { f.session.deadline = time.Now().Add(-time.Second) },
		"invalid-lease": func(t *testing.T, f *coldQuotaAdmissionFixture) {
			s, original := f.session, f.session.reference.LeaseID
			s.reference.LeaseID = 0
			t.Cleanup(func() { s.reference.LeaseID = original })
		},
		"busy-original-call": func(t *testing.T, f *coldQuotaAdmissionFixture) {
			call, err := f.b.beginWorkerCall(context.Background(), f.session)
			require.NoError(t, err)
			t.Cleanup(call.finish)
		},
		"different-session": func(t *testing.T, f *coldQuotaAdmissionFixture) {
			s, err := f.b.OpenWorkerSession(context.Background(), "other-quota-admission")
			require.NoError(t, err)
			f.claims, err = f.b.AcquireWorkerBudget(context.Background(), s, WorkerBudgetClaim, 1)
			require.NoError(t, err)
			// The foreign share is funded, so an unrelated empty bucket cannot
			// accidentally hide a missing same-session authority check.
			f.claims.bucket.last, f.claims.bucket.tokens = time.Now().Add(time.Hour), 1
		},
	}
	for _, state := range []string{"closed", "session-lost", "session-released", "session-release", "partition-lost", "partition-released", "partition-release", "range-lost", "range-released", "range-release", "claim-lost", "claim-released", "claim-release"} {
		state := state
		tests[state] = func(t *testing.T, f *coldQuotaAdmissionFixture) {
			// Restore synthetic release seals before fixture cleanup. Real
			// release/join behavior is tested separately below.
			switch state {
			case "closed":
				f.b.archiveLife.closed = true
				t.Cleanup(func() { f.b.archiveLife.closed = false })
			case "session-lost":
				f.session.lost = true
			case "session-released":
				f.session.released = true
			case "session-release":
				f.session.release = &workerRelease{}
				t.Cleanup(func() { f.session.release = nil })
			case "partition-lost":
				f.part.lost = true
			case "partition-released":
				f.part.released = true
			case "partition-release":
				f.part.release = &workerRelease{}
				t.Cleanup(func() { f.part.release = nil })
			case "range-lost":
				f.ranges.lost = true
			case "range-released":
				f.ranges.released = true
			case "range-release":
				f.ranges.release = &workerRelease{}
				t.Cleanup(func() { f.ranges.release = nil })
			case "claim-lost":
				f.claims.lost = true
			case "claim-released":
				f.claims.released = true
			case "claim-release":
				f.claims.release = &workerRelease{}
				t.Cleanup(func() { f.claims.release = nil })
			}
		}
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			f := newColdQuotaAdmissionFixture(t)
			original := *f
			mutate(t, f)
			original.b.archiveLife.mu.Lock()
			ranges, claims := original.ranges.bucket, original.claims.bucket
			operation, lost := original.session.operation, original.session.lost
			ownerContext := original.owner.context
			original.b.archiveLife.mu.Unlock()
			original.server.mu.Lock()
			requests := len(original.server.requests)
			original.server.mu.Unlock()

			call, comparisons, err := f.begin()
			require.Error(t, err)
			require.Nil(t, call)
			require.Nil(t, comparisons)
			original.b.archiveLife.mu.Lock()
			afterRanges, afterClaims := original.ranges.bucket, original.claims.bucket
			afterOperation, afterLost := original.session.operation, original.session.lost
			afterContext := original.owner.context
			original.b.archiveLife.mu.Unlock()
			require.Equal(t, ranges, afterRanges)
			require.Equal(t, claims, afterClaims)
			require.True(t, operation == afterOperation, "refusal must preserve the original operation pointer")
			require.Equal(t, lost, afterLost)
			require.Equal(t, ownerContext, afterContext)
			original.server.mu.Lock()
			afterRequests := len(original.server.requests)
			original.server.mu.Unlock()
			require.Equal(t, requests, afterRequests, "refusal is local and cannot send an RPC")
		})
	}
}

func TestColdQuotaWorkerAdmissionConcurrentCallsChargeOnce(t *testing.T) {
	f := newColdQuotaAdmissionFixture(t)
	start := make(chan struct{})
	type result struct {
		call *workerCall
		err  error
	}
	results := make(chan result, 2)
	var joined sync.WaitGroup
	for i := 0; i < 2; i++ {
		joined.Add(1)
		go func() {
			defer joined.Done()
			<-start
			call, _, err := f.begin()
			results <- result{call: call, err: err}
		}()
	}
	close(start)
	joined.Wait()
	first, second := <-results, <-results
	if first.err != nil {
		first, second = second, first
	}
	require.NoError(t, first.err)
	defer first.call.finish()
	require.ErrorIs(t, second.err, ErrConflict)
	require.Nil(t, second.call)
	f.b.archiveLife.mu.Lock()
	ranges, claims, operation := f.ranges.bucket.tokens, f.claims.bucket.tokens, f.session.operation
	f.b.archiveLife.mu.Unlock()
	require.Zero(t, ranges)
	require.Zero(t, claims)
	require.Same(t, first.call, operation)
}

func TestColdQuotaWorkerAdmissionCancellationRetainsOriginalCall(t *testing.T) {
	for _, source := range []string{"enrolled-owner", "worker-session"} {
		t.Run(source, func(t *testing.T) {
			f := newColdQuotaAdmissionFixture(t)
			call, _, err := f.begin()
			require.NoError(t, err)
			var finish sync.Once
			t.Cleanup(func() { finish.Do(call.finish) })
			wait, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			var released chan error
			if source == "enrolled-owner" {
				f.owner.cancel()
			} else {
				released = make(chan error, 1)
				go func() { released <- f.b.ReleaseWorkerSession(context.Background(), f.session) }()
			}
			select {
			case <-call.ctx.Done():
			case <-wait.Done():
				t.Fatal("original cancellation did not reach the paid call")
			}
			f.b.archiveLife.mu.Lock()
			operation := f.session.operation
			ranges, claims := f.ranges.bucket, f.claims.bucket
			f.b.archiveLife.mu.Unlock()
			require.Same(t, call, operation)
			require.Zero(t, ranges.tokens)
			require.Zero(t, claims.tokens)
			require.False(t, ranges.take(ranges.last, 262144))
			require.False(t, claims.take(claims.last, 1))
			select {
			case <-call.done:
				t.Fatal("cancellation must not publish physical call completion")
			default:
			}
			if released != nil {
				select {
				case err := <-released:
					t.Fatalf("release returned before original call finish: %v", err)
				default:
				}
			}

			finish.Do(call.finish)
			select {
			case <-call.done:
			default:
				t.Fatal("original finish did not publish call completion")
			}
			f.b.archiveLife.mu.Lock()
			afterOperation := f.session.operation
			afterRanges, afterClaims := f.ranges.bucket, f.claims.bucket
			f.b.archiveLife.mu.Unlock()
			require.Nil(t, afterOperation)
			if source == "enrolled-owner" {
				require.ErrorIs(t, f.owner.context.Err(), context.Canceled)
			} else {
				require.NoError(t, f.owner.context.Err(), "session cancellation cannot replace or cancel the enrolled owner context")
			}
			require.Equal(t, ranges.tokens, afterRanges.tokens)
			require.Equal(t, claims.tokens, afterClaims.tokens)
			if released != nil {
				select {
				case err := <-released:
					require.NoError(t, err)
				case <-wait.Done():
					t.Fatal("session release did not join original finish")
				}
			}
		})
	}
}
