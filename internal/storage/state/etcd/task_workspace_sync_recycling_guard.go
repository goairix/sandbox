package etcd

import "go.etcd.io/etcd/api/v3/mvccpb"

// Mutable permanent slots use the captured birth and known original ACK
// generation. The immutable intent still has the original reserve birth.
func (b *Backend) validateRecyclingLocalSlot(d *taskWorkspaceSyncIntentDraft, kv *mvccpb.KeyValue) error {
	slot, err := decodeWorkspaceSyncRecyclingSlot(kv)
	if err != nil {
		return err
	}
	ref, pin := d.record.Reference, d.recycling
	if b.workspaceSyncBudget == nil || b.workspaceSyncBudget.profile != 2 || pin == nil || !d.acknowledged || d.revision <= pin.mod || kv.CreateRevision != pin.create || kv.ModRevision != d.revision || kv.Version != pin.version+1 || string(kv.Value) != d.slotValue || slot.State != "occupied" || slot.BudgetID != ref.BudgetID || slot.Partition != ref.Task.Partition || slot.Slot != ref.Slot || slot.IntentID != ref.IntentID || slot.IntentDigest != workspaceSyncIntentDigest(d.value) {
		return ErrCorruptRecord
	}
	return nil
}
