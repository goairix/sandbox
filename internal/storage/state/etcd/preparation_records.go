package etcd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

var ErrPreparationNotReady = errors.New("etcd state: preparation inventory not ready")
var ErrPreparationCapacity = errors.New("etcd state: preparation capacity unavailable")

// PreparationInventoryOptions names fixed contiguous slot IDs in each quota
// partition. The array is deployment-sized, never runtime population-sized.
type PreparationInventoryOptions struct {
	QuotaSlots [256]uint32 `json:"quota_slots"`
}

func validatePreparationInventory(o *PreparationInventoryOptions) error {
	if o == nil {
		return nil
	}
	var total uint64
	for _, count := range o.QuotaSlots {
		if count > 1000000 {
			return ErrInvalidConfiguration
		}
		total += uint64(count)
	}
	if total == 0 {
		return ErrInvalidConfiguration
	}
	return nil
}

func preparationProfileDigest(n Namespace, identity Identity, profile *PreparationInventoryOptions) string {
	wire, _ := coldEncode(struct {
		Domain       string                      `json:"domain"`
		Namespace    string                      `json:"namespace"`
		Identity     Identity                    `json:"identity"`
		RestoreEpoch string                      `json:"restore_epoch"`
		Inventory    PreparationInventoryOptions `json:"inventory"`
	}{Domain: "sandbox.cold-preparation.inventory.v1", Namespace: n.Root(), Identity: identity, RestoreEpoch: identity.RestoreEpoch, Inventory: *profile})
	return workspaceSyncIntentDigest(wire)
}

type preparationInventoryRecord struct {
	Version       int    `json:"version"`
	RestoreEpoch  string `json:"restore_epoch"`
	ProfileDigest string `json:"profile_digest"`
	Next          uint64 `json:"next"`
	Total         uint64 `json:"total"`
	Ready         bool   `json:"ready"`
}

type ColdSlotOwner struct {
	IntentID      string `json:"intent_id"`
	SandboxID     string `json:"sandbox_id"`
	WorkspaceHash string `json:"workspace_hash"`
	RequestID     string `json:"request_id"`
	Generation    int64  `json:"generation"`
	Partition     uint8  `json:"partition"`
}

type ColdSlotReference struct {
	Kind      string `json:"kind"`
	Partition uint8  `json:"partition"`
	ID        uint32 `json:"id"`
	Epoch     uint64 `json:"epoch"`
}

type coldSlotRecord struct {
	Version       int               `json:"version"`
	RestoreEpoch  string            `json:"restore_epoch"`
	ProfileDigest string            `json:"profile_digest"`
	Slot          ColdSlotReference `json:"slot"`
	Occupied      bool              `json:"occupied"`
	Owner         ColdSlotOwner     `json:"owner"`
}

type ColdPreparationRecord struct {
	Version       int                 `json:"version"`
	RestoreEpoch  string              `json:"restore_epoch"`
	ProfileDigest string              `json:"profile_digest"`
	Owner         ColdSlotOwner       `json:"owner"`
	Quota         ColdSlotReference   `json:"quota"`
	Preparation   ColdSlotReference   `json:"preparation"`
	Phase         string              `json:"phase"`
	Settlement    StageAttemptLocator `json:"settlement"`
}

type coldPreparationWork struct {
	Version       int    `json:"version"`
	RestoreEpoch  string `json:"restore_epoch"`
	ProfileDigest string `json:"profile_digest"`
	IntentID      string `json:"intent_id"`
	Phase         string `json:"phase"`
}

type PreparationInventoryProgress struct {
	Ready       bool
	Next, Total uint64
}

type ColdPreparationResult struct {
	Outcome Outcome
	Replay  bool
	Record  *ColdPreparationRecord
}

func coldEncode(value any) (string, error) {
	wire, err := json.Marshal(value)
	if err != nil || len(wire) > 16384 {
		return "", ErrInvalidRecord
	}
	return string(wire), nil
}

func coldDecode(kv *mvccpb.KeyValue, value any) error {
	if kv == nil || kv.Lease != 0 || kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || len(kv.Value) > 16384 || strictPreparationMetadata(kv.Value, reflect.TypeOf(value)) != nil {
		return ErrCorruptRecord
	}
	if json.Unmarshal(kv.Value, value) != nil {
		return ErrCorruptRecord
	}
	wire, err := coldEncode(value)
	if err != nil || !bytes.Equal(kv.Value, []byte(wire)) {
		return ErrCorruptRecord
	}
	return nil
}

func (b *Backend) coldProfile() (string, uint64, error) {
	if b.preparationInventory == nil {
		return "", 0, ErrPreparationNotReady
	}
	return b.preparationDigest, b.preparationTotal, nil
}

func (n Namespace) coldSlotKeys(ref ColdSlotReference) (string, string, error) {
	if ref.Kind == "preparation" && ref.Partition == 0 && ref.ID < 32 {
		key, err := n.Key("preparation", "slots", fmt.Sprintf("%02d", ref.ID))
		free, _ := n.Key("preparation", "free", fmt.Sprintf("%02d", ref.ID))
		return key, free, err
	}
	if ref.Kind != "quota" || ref.ID >= 1000000 {
		return "", "", ErrInvalidRecord
	}
	key, err := n.Key("quota", fmt.Sprintf("%02x", ref.Partition), "slots", fmt.Sprintf("%08d", ref.ID))
	free, _ := n.Key("quota", fmt.Sprintf("%02x", ref.Partition), "free", fmt.Sprintf("%02x", ref.ID%16), fmt.Sprintf("%08d", ref.ID))
	return key, free, err
}

func (n Namespace) coldPreparationKeys(partition uint8, intent string) (string, string, error) {
	key, err := n.Key("p", fmt.Sprintf("%02x", partition), "intents", intent, "cold-preparation")
	work, _ := n.Key("p", fmt.Sprintf("%02x", partition), "intents", intent, "cold-work")
	return key, work, err
}
