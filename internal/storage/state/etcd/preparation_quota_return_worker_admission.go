package etcd

import (
	"context"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// beginColdQuotaWorkerCall pays for one quota-return worker turn with the
// original Range and Claim shares. The enrolled owner remains the sole context
// source and must outlive the returned call's finish. This is local scheduling
// admission only; the caller must compose all returned fences into its actual
// bounded transaction and join native work before finishing the original call.
//
// The existing lifecycle mutex owns membership, session-call registration and
// both buckets. Keeping this private admission in etcd preserves that shared
// invariant without exposing reconstructible worker or quota authority.
func (b *Backend) beginColdQuotaWorkerCall(o *runtimeEnrolledPublicationOwner, s *WorkerSession, p *WorkerPartitionClaim, ranges, claims *WorkerBudgetShare) (*workerCall, []clientv3.Cmp, error) {
	if claims == nil {
		return nil, nil, ErrInvalidRecord
	}
	return b.beginColdQuotaWorkerTurn(o, s, p, ranges, claims)
}

// beginColdQuotaWorkerMetadataCall pays only the original Range lane. A nil
// Claim is a private fixed-cohort choice, never reconstructed share authority.
func (b *Backend) beginColdQuotaWorkerMetadataCall(o *runtimeEnrolledPublicationOwner, s *WorkerSession, p *WorkerPartitionClaim, ranges *WorkerBudgetShare) (*workerCall, []clientv3.Cmp, error) {
	return b.beginColdQuotaWorkerTurn(o, s, p, ranges, nil)
}

// beginColdQuotaWorkerTurn keeps original owner membership, session lifetime,
// call registration and trial bucket publication in one lifecycle critical
// section. Its two fixed callers select Range-only metadata or Range+Claim
// mutation; metadata never inspects an unrelated Claim capability or bucket.
func (b *Backend) beginColdQuotaWorkerTurn(o *runtimeEnrolledPublicationOwner, s *WorkerSession, p *WorkerPartitionClaim, ranges, claims *WorkerBudgetShare) (*workerCall, []clientv3.Cmp, error) {
	if !b.validWorkerBackend() {
		return nil, nil, ErrInvalidRecord
	}

	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if o == nil || o.backend != b || o.context == nil {
		return nil, nil, ErrInvalidRecord
	}
	if _, active := b.archiveLife.enrolled[o]; !active {
		return nil, nil, ErrInvalidRecord
	}
	if !b.validWorkerSession(s) || !b.validWorkerPartition(p) || !b.validWorkerBudget(ranges) || p.session != s || ranges.session != s || ranges.kind != WorkerBudgetRange {
		return nil, nil, ErrInvalidRecord
	}
	if claims != nil && (!b.validWorkerBudget(claims) || claims.session != s || claims.kind != WorkerBudgetClaim) {
		return nil, nil, ErrInvalidRecord
	}

	if b.archiveLife.closed {
		return nil, nil, context.Canceled
	}
	if s.lost || s.released || s.release != nil || p.lost || p.released || p.release != nil || ranges.lost || ranges.released || ranges.release != nil {
		return nil, nil, ErrGuardExpired
	}
	if claims != nil && (claims.lost || claims.released || claims.release != nil) {
		return nil, nil, ErrGuardExpired
	}
	if err := o.context.Err(); err != nil {
		return nil, nil, err
	}
	if err := s.ctx.Err(); err != nil {
		return nil, nil, err
	}
	now := time.Now()
	if s.reference.LeaseID <= 0 || !now.Before(s.deadline) {
		return nil, nil, ErrGuardExpired
	}
	if s.operation != nil {
		return nil, nil, ErrConflict
	}

	// Metadata retains W20: base4 plus full profile/session/partition/Range
	// fences. Mutation additionally retains the full Claim fence for W24.
	// Later composition owns base deduplication, never fence weakening.
	comparisons := append(b.baseComparisons(), b.workerControl.profile.comparisons()...)
	for _, fence := range []taskFence{s.guard, p.fence, ranges.fence} {
		comparisons = append(comparisons, fence.comparisons()...)
	}
	if claims != nil {
		comparisons = append(comparisons, claims.fence.comparisons()...)
	}

	// Trial charges retain the original bucket's earned fractions and earning
	// origin. Neither a failed trial nor failed call registration publishes any
	// bucket change. One Range turn costs the existing 256 KiB wire allowance;
	// an accompanying mutation Claim costs exactly one original Claim credit.
	rangeBucket := ranges.bucket
	if !rangeBucket.take(now, maxMutationBytes) {
		return nil, nil, ErrConflict
	}
	var claimBucket workerBucket
	if claims != nil {
		claimBucket = claims.bucket
		if !claimBucket.take(now, 1) {
			return nil, nil, ErrConflict
		}
	}

	// This is the last fallible step. Reuse the original call registration and
	// its session-cancellation join, including s.live's existing invalidation
	// if the deadline crosses here. No credit is published on that failure.
	call, err := b.beginWorkerCallLocked(o.context, s)
	if err != nil {
		return nil, nil, err
	}
	ranges.bucket = rangeBucket
	if claims != nil {
		claims.bucket = claimBucket
	}
	return call, comparisons, nil
}
