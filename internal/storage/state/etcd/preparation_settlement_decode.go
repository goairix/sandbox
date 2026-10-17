package etcd

import "go.etcd.io/etcd/api/v3/mvccpb"

// validateColdSettlementSource binds the settled reservation and canonical work
// marker to already-authenticated publication history. The shared check also
// gates public receipt reads, so source corruption retains error precedence.
func (b *Backend) validateColdSettlementSource(r ColdPreparationRecord, reservation, work *mvccpb.KeyValue, history *coldPublicationHistory) error {
	if r.Phase != "settled" || history == nil || !coldCompletionMatches(r, history.entry.Record) || reservation == nil || work == nil || reservation.ModRevision != work.ModRevision || reservation.ModRevision <= history.journal.CreateRevision {
		return ErrCorruptRecord
	}

	var marker coldPreparationWork
	if coldDecode(work, &marker) != nil || marker != (coldPreparationWork{Version: 1, RestoreEpoch: b.restoreEpoch, ProfileDigest: b.preparationDigest, IntentID: r.Owner.IntentID, Phase: "settled"}) {
		return ErrCorruptRecord
	}
	return nil
}

// decodeColdSettlement verifies supplied metadata without reading another
// receipt. Its caller must supply authenticated publication history; the exact
// key and shared transaction birth bind the immutable committed receipt even
// when no point reader has checked attribution. The reference grants no runtime
// authority and does not establish quota release or physical runtime death.
func (b *Backend) decodeColdSettlement(r ColdPreparationRecord, reservation, work *mvccpb.KeyValue, history *coldPublicationHistory, receipt *mvccpb.KeyValue) (StageReference, error) {
	if err := b.validateColdSettlementSource(r, reservation, work, history); err != nil {
		return StageReference{}, err
	}

	key, err := b.runtimeDispatchReceiptKey(r.Settlement)
	if err != nil {
		return StageReference{}, err
	}
	if !enrolledImmutableKV(receipt) || string(receipt.Key) != key || receipt.CreateRevision != reservation.ModRevision {
		return StageReference{}, ErrCorruptReceipt
	}

	ref, err := decodeRuntimeDispatchReceipt(receipt, r.Settlement)
	if err != nil {
		return StageReference{}, err
	}
	wire, err := encodeReceipt(ref, OutcomeCommitted)
	if err != nil || wire != string(receipt.Value) {
		return StageReference{}, ErrCorruptReceipt
	}
	return ref, nil
}
