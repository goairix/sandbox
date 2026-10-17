package etcd

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// coldQuotaReturnPrepared owns one copied mutation and its pre-Grant identity.
// It remains private to the quota return protocol: neither historical metadata
// nor its StageReference grants permission to construct another continuation.
// The original enrolled owner must remain registered until continuation joins.
type coldQuotaReturnPrepared struct {
	self    *coldQuotaReturnPrepared
	backend *Backend
	owner   *runtimeEnrolledPublicationOwner

	reference StageReference
	mutation  Mutation
	history   *coldQuotaReturnHistory
	returnKey string

	// journalAttempted admits one journal submitter. Losing duplicate calls
	// must not consume the successful submitter's in-flight continuation.
	journalAttempted uint32

	// consumed is claimed before validating the calling binding. Every attempt,
	// including cancellation or refusal, permanently closes this capability.
	consumed uint32
}

// coldQuotaReturnOwnerContext admits only the exact registered owner. All
// preparation and continuation RPCs inherit that owner's cancellation so Close
// can cancel them and join the same owner before closing the shared client.
func (b *Backend) coldQuotaReturnOwnerContext(o *runtimeEnrolledPublicationOwner) (context.Context, error) {
	if b == nil || b.archiveLife == nil || o == nil || o.backend != b || o.context == nil {
		return nil, ErrInvalidRecord
	}

	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if _, active := b.archiveLife.enrolled[o]; !active || b.archiveLife.closed {
		return nil, ErrConflict
	}
	if err := o.context.Err(); err != nil {
		return nil, err
	}
	return o.context, nil
}

// prepareColdRuntimeQuotaReturn authenticates the original history before
// checking for a committed replay. A replay returns before reading the mutable
// quota slot: that slot may already belong to a later runtime incarnation.
// Fresh preparation freezes the exact copied mutation without Grant, guard
// creation, or business writes; the caller still owns registration and finish.
func (b *Backend) prepareColdRuntimeQuotaReturn(o *runtimeEnrolledPublicationOwner, w WorkspaceIdentity, intent string, task TaskReference) (*coldQuotaReturnPrepared, ColdRuntimeQuotaReturnResult, error) {
	history, key, result, err := b.authenticateColdRuntimeQuotaReturn(o, w, intent, task)
	if err != nil || history == nil {
		return nil, result, err
	}
	return b.prepareColdRuntimeQuotaReturnHistory(o, history, key)
}

// authenticateColdRuntimeQuotaReturn retains the original history and replay
// ordering for both callers. A committed replay needs neither the pending
// journal nor the mutable quota slot, which may belong to a newer incarnation.
func (b *Backend) authenticateColdRuntimeQuotaReturn(o *runtimeEnrolledPublicationOwner, w WorkspaceIdentity, intent string, task TaskReference) (*coldQuotaReturnHistory, string, ColdRuntimeQuotaReturnResult, error) {
	result := ColdRuntimeQuotaReturnResult{Outcome: OutcomeUnknown}
	ctx, err := b.coldQuotaReturnOwnerContext(o)
	if err != nil {
		return nil, "", result, err
	}
	if b.localRuntimeRelease.validate() != nil || b.validateTaskReference(task) != nil || b.validateWorkspaceBinding(w) != nil || task.Partition != w.Partition() {
		return nil, "", result, ErrInvalidRecord
	}
	if _, _, err := b.coldProfile(); err != nil {
		return nil, "", result, err
	}

	history, err := b.readColdQuotaReturnHistory(ctx, w, intent, task)
	if err != nil {
		return nil, "", result, err
	}
	key, err := b.namespace.coldRuntimeQuotaReturnKey(w.Partition(), intent)
	if err != nil {
		return nil, "", result, err
	}
	points, err := b.readEnrolledPoints(ctx, []string{key}, []int{16384})
	if err != nil {
		return nil, "", result, err
	}
	if points[0] != nil {
		result.Reference, result.Entry, err = b.verifyColdQuotaReturn(ctx, history, points[0])
		if err == nil {
			result.Outcome = OutcomeCommitted
		}
		return nil, "", result, err
	}
	return history, key, result, nil
}

// Only authenticated history reaches this fresh-attempt builder. Keeping the
// slot reads here lets public recovery resolve its original journal first;
// an absent journal read alone never authorizes replacing a pending attempt.
func (b *Backend) prepareColdRuntimeQuotaReturnHistory(o *runtimeEnrolledPublicationOwner, history *coldQuotaReturnHistory, key string) (*coldQuotaReturnPrepared, ColdRuntimeQuotaReturnResult, error) {
	result := ColdRuntimeQuotaReturnResult{Outcome: OutcomeUnknown}
	ctx, err := b.coldQuotaReturnOwnerContext(o)
	if err != nil {
		return nil, result, err
	}
	if history == nil || !validNamespaceKey(b.namespace, key) {
		return nil, result, ErrInvalidRecord
	}

	_, ready, err := b.coldReady(ctx)
	if err != nil {
		return nil, result, err
	}
	slotKey, free, err := b.namespace.coldSlotKeys(history.reservation.Quota)
	if err != nil {
		return nil, result, err
	}
	slots, err := b.readEnrolledPoints(ctx, []string{slotKey, free}, []int{16384, 16384})
	if err != nil {
		return nil, result, err
	}
	var slot coldSlotRecord
	if coldDecode(slots[0], &slot) != nil || !b.validColdSlot(slot) || !slot.Occupied || slot.Owner != history.reservation.Owner || slot.Slot != history.reservation.Quota || slots[1] != nil {
		return nil, result, ErrConflict
	}

	locator := StageAttemptLocator{Namespace: b.namespace.Root(), Partition: history.reservation.Owner.Partition, RequestID: history.reservation.Owner.RequestID, StageID: "cold_runtime_quota_return", AttemptID: uuid.NewString(), RestoreEpoch: b.restoreEpoch}
	if err := locator.Validate(); err != nil {
		return nil, result, err
	}
	mutation, err := b.coldQuotaReturnMutation(history, ready, slots[0], key, free, locator)
	if err != nil {
		return nil, result, err
	}
	mutation, digest, err := prepareMutation(b.namespace, mutation)
	if err != nil {
		return nil, result, err
	}
	prepared := &coldQuotaReturnPrepared{backend: b, owner: o, reference: locator.reference(digest), mutation: mutation, history: history, returnKey: key}
	prepared.self = prepared
	result.Reference = prepared.reference
	return prepared, result, nil
}

// continueColdRuntimeQuotaReturn consumes one original prepared attempt. No
// error reopens it, even when Grant or guard acknowledgement is lost; callers
// can only arbitrate its original reference through ResolveStage. The owner is
// the context source in both phases, rather than accepting a replacement ctx.
func (b *Backend) continueColdRuntimeQuotaReturn(o *runtimeEnrolledPublicationOwner, prepared *coldQuotaReturnPrepared) (ColdRuntimeQuotaReturnResult, error) {
	result := ColdRuntimeQuotaReturnResult{Outcome: OutcomeUnknown}
	if prepared == nil {
		return result, ErrInvalidRecord
	}
	result.Reference = prepared.reference
	if !atomic.CompareAndSwapUint32(&prepared.consumed, 0, 1) {
		return result, ErrConflict
	}
	if prepared.self != prepared || prepared.backend != b || prepared.owner != o || prepared.history == nil || prepared.returnKey == "" {
		return result, ErrInvalidRecord
	}
	ctx, err := b.coldQuotaReturnOwnerContext(o)
	if err != nil {
		return result, err
	}

	ref := prepared.reference
	locator := StageAttemptLocator{Namespace: ref.Namespace, Partition: ref.Partition, RequestID: ref.RequestID, StageID: ref.StageID, AttemptID: ref.AttemptID, RestoreEpoch: ref.RestoreEpoch}
	if err := locator.Validate(); err != nil || locator.StageID != "cold_runtime_quota_return" {
		return result, ErrInvalidMutation
	}
	// Reuse this locator directly: the builder may validate/copy the frozen
	// bytes but must neither reauthenticate a new snapshot nor allocate a UUID.
	// Ten seconds retains the original quota-return guard lifetime.
	stage, err := b.beginStageWithLocator(ctx, locator, 10*time.Second, func(attempt StageAttemptLocator) (Mutation, error) {
		mutation, digest, err := prepareMutation(b.namespace, prepared.mutation)
		if err != nil {
			return Mutation{}, err
		}
		if attempt.reference(digest) != ref {
			return Mutation{}, ErrInvalidMutation
		}
		return mutation, nil
	}, nil)
	if err != nil {
		return result, err
	}
	return b.commitColdRuntimeQuotaReturn(ctx, prepared, stage)
}
