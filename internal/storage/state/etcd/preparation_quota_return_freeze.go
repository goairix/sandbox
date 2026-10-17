package etcd

import "go.etcd.io/etcd/api/v3/mvccpb"

// validateColdQuotaReturnSlot admits only the original occupied quota and an
// absent free marker. Both public and worker builders run it before allocating
// an attempt UUID; a stale slot or an existing free marker is a conflict, not
// permission to manufacture another candidate or return quota twice.
func (b *Backend) validateColdQuotaReturnSlot(history *coldQuotaReturnHistory, slotKV, freeKV *mvccpb.KeyValue) error {
	var slot coldSlotRecord
	if history == nil || coldDecode(slotKV, &slot) != nil || !b.validColdSlot(slot) || !slot.Occupied || slot.Owner != history.reservation.Owner || slot.Slot != history.reservation.Quota || freeKV != nil {
		return ErrConflict
	}
	return nil
}

// freezeColdQuotaReturnPrepared copies the original mutation once and seals
// its exact digest/reference. It performs no RPC, admission, UUID allocation,
// journal write or Stage operation. The caller supplies its already allocated
// locator only after ready and the original slot/free pair have been checked;
// authenticated history remains borrowed and immutable through continuation.
func (b *Backend) freezeColdQuotaReturnPrepared(o *runtimeEnrolledPublicationOwner, history *coldQuotaReturnHistory, ready, slotKV, freeKV *mvccpb.KeyValue, key string, locator StageAttemptLocator) (*coldQuotaReturnPrepared, ColdRuntimeQuotaReturnResult, error) {
	result := ColdRuntimeQuotaReturnResult{Outcome: OutcomeUnknown}
	if err := b.validateColdQuotaReturnSlot(history, slotKV, freeKV); err != nil {
		return nil, result, err
	}
	if err := locator.Validate(); err != nil {
		return nil, result, err
	}
	_, free, err := b.namespace.coldSlotKeys(history.reservation.Quota)
	if err != nil {
		return nil, result, err
	}

	mutation, err := b.coldQuotaReturnMutation(history, ready, slotKV, key, free, locator)
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
