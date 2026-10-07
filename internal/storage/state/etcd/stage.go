package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Outcome is evidence about this exact metadata transaction attempt. Unknown
// includes pending attempts and must never be interpreted as an external abort.
type Outcome string

const (
	OutcomeUnknown   Outcome = "unknown"
	OutcomeCommitted Outcome = "committed"
	OutcomeAborted   Outcome = "aborted"
)

// StageReference is the durable identity needed to arbitrate an attempt after
// its creator restarts. It grants no permission to submit business mutations.
type StageReference struct {
	Namespace    string `json:"namespace"`
	Partition    uint8  `json:"partition"`
	RequestID    string `json:"request_id"`
	StageID      string `json:"stage_id"`
	AttemptID    string `json:"attempt_id"`
	Digest       string `json:"digest"`
	RestoreEpoch string `json:"restore_epoch"`
}

// Stage is an immutable mutation capability tied to one original Lease. A
// revoked/expired stage is never rebuilt; a legal retry creates a new attempt.
type Stage struct {
	origin         *Backend
	reference      StageReference
	mutation       Mutation
	leaseID        clientv3.LeaseID
	guardKey       string
	guardValue     string
	guardRevision  int64
	receiptKey     string
	committedValue string
	reservation    *taskQuiescenceReservationPin
}

// Reference returns a copy suitable for a durable intent/checkpoint.
func (s *Stage) Reference() StageReference { return s.reference }

// BeginStage validates and copies the mutation before allocating a dedicated
// Lease. Guard creation with an unknown result never returns a capability.
func (b *Backend) BeginStage(ctx context.Context, partition uint8, requestID, stageID string, input Mutation, ttl time.Duration) (*Stage, error) {
	return b.beginStageWithBuilder(ctx, partition, requestID, stageID, ttl, func(StageAttemptLocator) (Mutation, error) { return input, nil })
}

// stageBeginFailure transports cleanup separately to higher-level result APIs.
// Public Stage callers still see both causes through errors.Is, as with Join.
type stageBeginFailure struct {
	cause, cleanup error
}

func (e *stageBeginFailure) Error() string   { return errors.Join(e.cause, e.cleanup).Error() }
func (e *stageBeginFailure) Unwrap() []error { return []error{e.cause, e.cleanup} }
func joinStageBeginCleanup(cause, cleanup error) error {
	if cleanup == nil {
		return cause
	}
	return &stageBeginFailure{cause: cause, cleanup: cleanup}
}

// beginStageWithBuilder invokes build once, before any RPC, with a fresh
// attempt. The resulting mutation is validated and copied before Lease grant.
func (b *Backend) beginStageWithBuilder(ctx context.Context, partition uint8, requestID, stageID string, ttl time.Duration, build func(StageAttemptLocator) (Mutation, error)) (*Stage, error) {
	if ctx == nil || build == nil || ttl <= 0 || ttl > 24*time.Hour {
		return nil, fmt.Errorf("%w: invalid stage identity, context or lease TTL", ErrInvalidMutation)
	}
	locator := StageAttemptLocator{Namespace: b.namespace.Root(), Partition: partition, RequestID: requestID, StageID: stageID, AttemptID: uuid.NewString(), RestoreEpoch: b.restoreEpoch}
	if err := locator.Validate(); err != nil {
		return nil, err
	}
	return b.beginStageWithLocator(ctx, locator, ttl, build, nil)
}

// Generic callers retain fresh locator allocation and their original behavior.
func (b *Backend) beginStageWithLocator(ctx context.Context, locator StageAttemptLocator, ttl time.Duration, build func(StageAttemptLocator) (Mutation, error), reservation *taskQuiescenceReservationPin) (*Stage, error) {
	input, err := build(locator)
	if err != nil {
		return nil, err
	}
	mutation, digest, err := prepareMutation(b.namespace, input)
	if err != nil {
		return nil, err
	}
	ref := locator.reference(digest)
	guardKey, receiptKey, err := b.stageKeys(ref)
	if err != nil {
		return nil, err
	}
	committedValue, err := encodeReceipt(ref, OutcomeCommitted)
	if err != nil {
		return nil, err
	}
	if reservation != nil {
		if err := b.preflightReservedTaskStage(ref, mutation, guardKey, receiptKey, committedValue, *reservation); err != nil {
			return nil, err
		}
	}
	requestCtx, cancel := b.requestContext(ctx)
	defer cancel()
	seconds := int64(ttl / time.Second)
	if ttl%time.Second != 0 {
		seconds++
	}
	lease, err := b.client.Grant(requestCtx, seconds)
	if err != nil {
		return nil, fmt.Errorf("%w: grant stage lease: %w", ErrOutcomeUnknown, err)
	}
	// A malformed grant never authorizes guard creation. Only an identified
	// native grant can authorize best-effort cleanup of its lease.
	trustedGrant := lease != nil && lease.ResponseHeader != nil && lease.ClusterId == b.clusterID && lease.Revision > 0 && lease.ID > 0
	if !trustedGrant || lease.TTL <= 0 || lease.Error != "" || requestCtx.Err() != nil {
		cause := error(ErrOutcomeUnknown)
		if lease != nil && lease.ResponseHeader != nil && lease.ClusterId != b.clusterID {
			cause = errors.Join(cause, ErrIdentityMismatch)
		}
		if requestCtx.Err() != nil {
			cause = errors.Join(cause, requestCtx.Err())
		}
		if trustedGrant {
			return nil, joinStageBeginCleanup(cause, b.revokeStageLease(lease.ID))
		}
		return nil, cause
	}
	s := &Stage{origin: b, reference: ref, mutation: mutation, leaseID: lease.ID, guardKey: guardKey, receiptKey: receiptKey, committedValue: committedValue, reservation: reservation}
	guard, err := json.Marshal(struct {
		StageReference
		LeaseID int64 `json:"lease_id"`
	}{ref, int64(lease.ID)})
	if err != nil {
		return nil, joinStageBeginCleanup(err, b.revokeStageLease(lease.ID))
	}
	s.guardValue = string(guard)
	compares := append(b.baseComparisons(), clientv3.Compare(clientv3.CreateRevision(guardKey), "=", 0), clientv3.Compare(clientv3.CreateRevision(receiptKey), "=", 0))
	response, err := b.client.Txn(requestCtx).If(compares...).Then(clientv3.OpPut(guardKey, s.guardValue, clientv3.WithLease(lease.ID))).Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey)).Commit()
	if err != nil {
		return nil, joinStageBeginCleanup(fmt.Errorf("%w: create stage guard: %w", ErrOutcomeUnknown, err), b.revokeStageLease(lease.ID))
	}
	if err := b.operationResponseHeader(response); err != nil {
		return nil, joinStageBeginCleanup(errors.Join(ErrOutcomeUnknown, err), b.revokeStageLease(lease.ID))
	}
	if !response.Succeeded {
		cause := error(ErrConflict)
		if _, err := b.stageEvidencePoints(response, []string{b.identityKey, b.restoreKey}); err != nil {
			cause = err
		}
		return nil, joinStageBeginCleanup(cause, b.revokeStageLease(lease.ID))
	}
	if !operationPutResponses(response, 1) {
		return nil, joinStageBeginCleanup(ErrOutcomeUnknown, b.revokeStageLease(lease.ID))
	}
	s.guardRevision = response.Header.Revision
	return s, nil
}

// CommitStage atomically applies the copied permanent mutation and its durable
// committed receipt. An existing receipt wins; retries never reapply writes.
func (b *Backend) CommitStage(ctx context.Context, s *Stage) (Outcome, error) {
	if ctx == nil || s == nil || s.origin != b {
		return OutcomeUnknown, fmt.Errorf("%w: stage belongs to a different backend", ErrInvalidMutation)
	}
	requestCtx, cancel := b.requestContext(ctx)
	defer cancel()
	compares := append(b.baseComparisons(),
		clientv3.Compare(clientv3.Value(s.guardKey), "=", s.guardValue),
		clientv3.Compare(clientv3.LeaseValue(s.guardKey), "=", int64(s.leaseID)),
		clientv3.Compare(clientv3.CreateRevision(s.guardKey), "=", s.guardRevision),
		clientv3.Compare(clientv3.CreateRevision(s.receiptKey), "=", 0),
	)
	if s.reservation != nil {
		compares = append(compares, s.reservation.comparison())
	}
	compares = append(compares, s.mutation.Comparisons...)
	operations := s.businessOperations()
	operations = append(operations, clientv3.OpPut(s.receiptKey, s.committedValue))
	response, err := b.client.Txn(requestCtx).If(compares...).Then(operations...).Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey), clientv3.OpGet(s.receiptKey), clientv3.OpGet(s.guardKey)).Commit()
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("%w: commit stage: %w", ErrOutcomeUnknown, err)
	}
	if err := b.operationResponseHeader(response); err != nil {
		return OutcomeUnknown, errors.Join(ErrOutcomeUnknown, err)
	}
	if response.Succeeded {
		if !stageCommitResponses(response, s.mutation.Writes) {
			return OutcomeUnknown, ErrOutcomeUnknown
		}
		return OutcomeCommitted, nil
	}
	points, err := b.stageEvidencePoints(response, []string{b.identityKey, b.restoreKey, s.receiptKey, s.guardKey})
	if err != nil {
		return OutcomeUnknown, err
	}
	if points[2] != nil {
		return decodeReceipt(points[2], s.reference)
	}
	guard := points[3]
	if guard == nil || string(guard.Value) != s.guardValue || guard.Lease != int64(s.leaseID) || guard.CreateRevision != s.guardRevision || guard.ModRevision != guard.CreateRevision {
		return OutcomeUnknown, ErrGuardExpired
	}
	return OutcomeUnknown, ErrConflict
}

func (s *Stage) businessOperations() []clientv3.Op {
	operations := make([]clientv3.Op, 0, len(s.mutation.Writes)+1)
	for _, write := range s.mutation.Writes {
		if write.Delete {
			operations = append(operations, clientv3.OpDelete(write.Key))
		} else {
			operations = append(operations, clientv3.OpPut(write.Key, string(write.Value)))
		}
	}
	return operations
}

// ReleaseStage revokes only the original guard Lease. Durable business state
// and receipt remain; unknown revocation is reported without creating a Lease.
func (b *Backend) ReleaseStage(ctx context.Context, s *Stage) error {
	if ctx == nil || s == nil || s.origin != b {
		return ErrInvalidMutation
	}
	requestCtx, cancel := b.requestContext(ctx)
	defer cancel()
	_, err := b.client.Revoke(requestCtx, s.leaseID)
	if errors.Is(err, rpctypes.ErrLeaseNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: revoke stage lease: %w", ErrOutcomeUnknown, err)
	}
	return nil
}

func (b *Backend) revokeStageLease(id clientv3.LeaseID) error {
	// The allocation request may already be cancelled. Best-effort cleanup gets
	// its own bounded context; expiry is the fallback, never guard recreation.
	ctx, cancel := b.requestContext(context.Background())
	defer cancel()
	_, err := b.client.Revoke(ctx, id)
	if errors.Is(err, rpctypes.ErrLeaseNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: cleanup stage lease: %w", ErrOutcomeUnknown, err)
	}
	return nil
}
