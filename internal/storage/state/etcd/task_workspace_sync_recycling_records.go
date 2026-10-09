package etcd

import (
	"fmt"
	"math"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

func (r taskWorkspaceSyncRecyclingSlotRecord) Validate() error {
	if r.Version != 2 || !validPreparationUUID(r.BudgetID) || r.Slot >= 64 {
		return ErrInvalidRecord
	}
	switch r.State {
	case "free":
		if r.IntentID != "" || r.IntentDigest != "" || r.IntentBirth != 0 || r.EvidenceDigest != "" {
			return ErrInvalidRecord
		}
	case "occupied":
		if !validPreparationUUID(r.IntentID) || !validHexDigest(r.IntentDigest) || r.IntentBirth != 0 || r.EvidenceDigest != "" {
			return ErrInvalidRecord
		}
	case "terminal":
		if !validPreparationUUID(r.IntentID) || !validHexDigest(r.IntentDigest) || r.IntentBirth <= 0 || !validHexDigest(r.EvidenceDigest) {
			return ErrInvalidRecord
		}
	default:
		return ErrInvalidRecord
	}
	return nil
}

func encodeWorkspaceSyncRecyclingSlot(r taskWorkspaceSyncRecyclingSlotRecord) (string, error) {
	return encodeOperationMetadata(r, 512)
}

// Permanent slots can change value and native version; their next generation
// must remain representable. An unchanged free value is never a generation pin.
func decodeWorkspaceSyncRecyclingSlot(kv *mvccpb.KeyValue) (taskWorkspaceSyncRecyclingSlotRecord, error) {
	var r taskWorkspaceSyncRecyclingSlotRecord
	if kv == nil || kv.Lease != 0 || kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || kv.ModRevision == math.MaxInt64 || kv.Version <= 0 || kv.Version == math.MaxInt64 || decodePreparationDomain(kv, &r, 512) != nil {
		return r, ErrCorruptRecord
	}
	value, err := encodeWorkspaceSyncRecyclingSlot(r)
	if err != nil || value != string(kv.Value) {
		return r, ErrCorruptRecord
	}
	return r, nil
}

func (b *Backend) workspaceSyncRecyclingKeys(r TaskWorkspaceSyncIntentReference) (string, string, error) {
	if r.Validate() != nil {
		return "", "", ErrInvalidRecord
	}
	if r.Task.Namespace != b.namespace.Root() || r.Task.RestoreEpoch != b.restoreEpoch {
		return "", "", ErrIdentityMismatch
	}
	i, err := b.namespace.Key("p", fmt.Sprintf("%02x", r.Task.Partition), "workspace-sync-recycling-intents", r.BudgetID, r.IntentID)
	if err != nil {
		return "", "", err
	}
	s, err := b.namespace.Key("p", fmt.Sprintf("%02x", r.Task.Partition), "workspace-sync-recycling-slots", r.BudgetID, fmt.Sprintf("%02d", r.Slot))
	return i, s, err
}
