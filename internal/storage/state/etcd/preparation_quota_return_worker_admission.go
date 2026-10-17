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
	if !b.validWorkerSession(s) || !b.validWorkerPartition(p) || !b.validWorkerBudget(ranges) || !b.validWorkerBudget(claims) || p.session != s || ranges.session != s || claims.session != s || ranges.kind != WorkerBudgetRange || claims.kind != WorkerBudgetClaim {
		return nil, nil, ErrInvalidRecord
	}

	if b.archiveLife.closed {
		return nil, nil, context.Canceled
	}
	if s.lost || s.released || s.release != nil || p.lost || p.released || p.release != nil || ranges.lost || ranges.released || ranges.release != nil || claims.lost || claims.released || claims.release != nil {
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

	// Retain W24: base4 plus the full profile, session, partition, Range and
	// Claim fences (four comparisons each). Later transaction composition owns
	// base deduplication; this admission cannot weaken any original fence.
	comparisons := append(b.baseComparisons(), b.workerControl.profile.comparisons()...)
	for _, fence := range []taskFence{s.guard, p.fence, ranges.fence, claims.fence} {
		comparisons = append(comparisons, fence.comparisons()...)
	}

	// Trial charges retain the original bucket's earned fractions and earning
	// origin. Neither a failed trial nor failed call registration publishes any
	// bucket change. One Range turn costs the existing 256 KiB wire allowance;
	// the accompanying Claim turn costs exactly one original Claim credit.
	rangeBucket, claimBucket := ranges.bucket, claims.bucket
	if !rangeBucket.take(now, maxMutationBytes) || !claimBucket.take(now, 1) {
		return nil, nil, ErrConflict
	}

	// This is the last fallible step. Reuse the original call registration and
	// its session-cancellation join, including s.live's existing invalidation
	// if the deadline crosses here. No credit is published on that failure.
	call, err := b.beginWorkerCallLocked(o.context, s)
	if err != nil {
		return nil, nil, err
	}
	ranges.bucket, claims.bucket = rangeBucket, claimBucket
	return call, comparisons, nil
}
