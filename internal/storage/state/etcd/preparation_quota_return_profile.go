package etcd

import (
	"bytes"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

// coldQuotaReleaseBinding is an ephemeral view of an already authenticated
// private history, not a capability reconstructed from diagnostic fields. Only
// the original loader/cursor can supply its concrete local or detached proof.
type coldQuotaReleaseBinding struct {
	task  TaskReference
	birth int64
	kv    *mvccpb.KeyValue

	local    *localOwnerReleaseProof
	detached *detachedOwnerReleaseProof
}

// Keep v1's original field, but never accept B-union.local as a second spelling
// of it. Detached must have its whole authenticated graph and match the paid
// cold publication/reservation; an empty or mixed history always fails closed.
func coldQuotaReturnRelease(h *coldQuotaReturnHistory) (coldQuotaReleaseBinding, error) {
	if h == nil {
		return coldQuotaReleaseBinding{}, ErrInvalidRecord
	}

	var binding coldQuotaReleaseBinding
	if h.release != nil {
		if h.profileRelease != nil {
			return binding, ErrInvalidRecord
		}
		binding = coldQuotaReleaseBinding{
			task: h.release.record.Task, birth: h.release.record.TaskBirth,
			kv: h.release.kv, local: h.release,
		}
	} else {
		proof := h.profileRelease
		if proof == nil || proof.local != nil || proof.detached == nil || !coldQuotaDetachedHistoryOriginMatches(h, proof) {
			return binding, ErrInvalidRecord
		}
		detached := proof.detached
		binding = coldQuotaReleaseBinding{
			task: detached.record.Task, birth: detached.record.TaskBirth,
			kv: detached.kv, detached: detached,
		}
	}

	if binding.task.Validate() != nil || binding.birth <= 0 || binding.kv == nil {
		return coldQuotaReleaseBinding{}, ErrInvalidRecord
	}
	return binding, nil
}

func coldQuotaReturnHistoryOriginMatches(h *coldQuotaReturnHistory) bool {
	binding, err := coldQuotaReturnRelease(h)
	if err != nil || h.publication == nil || h.publication.entry == nil {
		return false
	}
	if binding.local != nil {
		return coldQuotaReturnOriginMatches(h.reservation, h.publication.entry.Record, binding.local)
	}
	return coldQuotaDetachedHistoryOriginMatches(h, h.profileRelease)
}

// These are exactly the original six journal/source snapshots. Selecting a
// detached seal changes neither their order nor their native compare count;
// full signed history authenticates that immutable seal before it is borrowed.
func coldQuotaReturnHistorySources(h *coldQuotaReturnHistory) ([6]*mvccpb.KeyValue, error) {
	binding, err := coldQuotaReturnRelease(h)
	if err != nil || h.publication == nil {
		return [6]*mvccpb.KeyValue{}, ErrInvalidRecord
	}
	return [6]*mvccpb.KeyValue{h.publication.journal, h.publication.receipt, h.reservedKV, h.workKV, h.settledKV, binding.kv}, nil
}

// Independent v2 encoding reuses the accepted codec without changing any v1
// bytes. Callers first select the concrete private proof, never a wire domain.
func (b *Backend) coldQuotaReturnWire(h *coldQuotaReturnHistory, locator StageAttemptLocator) (string, error) {
	binding, err := coldQuotaReturnRelease(h)
	if err != nil || h.publication == nil || h.publication.entry == nil {
		return "", ErrInvalidRecord
	}
	if binding.local != nil {
		return encodeColdRuntimeQuotaReturn(b.coldQuotaReturnRecord(h, locator))
	}
	return encodeDetachedColdRuntimeQuotaReturn(b.detachedColdQuotaReturnRecord(h, locator, binding.detached))
}

// The selected proof is borrowed from the checked binding above. This builder
// supplies only expected persisted data and creates no new release authority.
func (b *Backend) detachedColdQuotaReturnRecord(h *coldQuotaReturnHistory, locator StageAttemptLocator, release *detachedOwnerReleaseProof) detachedColdRuntimeQuotaReturnRecord {
	r, pub := h.reservation, h.publication.entry
	return detachedColdRuntimeQuotaReturnRecord{
		Version: 2, Domain: "sandbox.cold-runtime-quota-return.v2",
		Namespace: b.namespace.Root(), RestoreEpoch: b.restoreEpoch, ProfileDigest: b.preparationDigest,
		Owner: r.Owner, Quota: r.Quota, Preparation: r.Preparation,

		Runtime: pub.Record.Runtime, DataGateEpoch: pub.Record.DataGateEpoch,
		NodeUID: pub.Record.NodeUID, NodeBootID: pub.Record.NodeBootID,
		Publication: pub.Reference, PublicationStamp: localRuntimeStamp(h.publication.journal),
		Reservation: localRuntimeStamp(h.reservedKV), PreparationWork: localRuntimeStamp(h.workKV), PreparationSettlement: h.settlement,

		Release: release.record, ReleaseStamp: localRuntimeStamp(release.kv), Attempt: locator,
	}
}

// Frozen journal attribution must bind the complete expected variant bytes,
// not merely a decoded version or locator. The outer mutation digest/reference
// is checked by the original caller; this check allocates no UUID or Stage.
func (b *Backend) validateColdQuotaReturnFrozenValue(prepared *coldQuotaReturnPrepared) error {
	if prepared == nil || prepared.history == nil {
		return ErrInvalidMutation
	}

	ref := prepared.reference
	locator := StageAttemptLocator{Namespace: ref.Namespace, Partition: ref.Partition, RequestID: ref.RequestID, StageID: ref.StageID, AttemptID: ref.AttemptID, RestoreEpoch: ref.RestoreEpoch}
	expected, err := b.coldQuotaReturnWire(prepared.history, locator)
	if err != nil {
		return ErrInvalidMutation
	}

	matched := false
	for _, write := range prepared.mutation.Writes {
		if write.Key != prepared.returnKey {
			continue
		}
		if matched || write.Delete || !bytes.Equal(write.Value, []byte(expected)) {
			return ErrInvalidMutation
		}
		matched = true
	}
	if !matched {
		return ErrInvalidMutation
	}
	return nil
}
