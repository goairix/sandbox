package etcd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

// B's canonical JSON is capped at 2048 bytes. It carries migration coverage,
// never Task execution authority.
const cleanupBackfillBytes = 2048

// Version 1 preserves the original legacy cutoff across view refreshes. The
// frontier is the last UUID covered by an atomic pending/coverage CAS; complete
// records exhaustion at the stored view. Field order is canonical and stable.
type cleanupBackfillRecord struct {
	Version              uint32 `json:"version"`
	Namespace            string `json:"namespace"`
	RestoreEpoch         string `json:"restore_epoch"`
	IdentityDigest       string `json:"identity_digest"`
	Partition            uint8  `json:"partition"`
	LegacyCutoffRevision int64  `json:"legacy_cutoff_revision"`
	ReadViewRevision     int64  `json:"read_view_revision"`
	Frontier             string `json:"frontier"`
	Phase                string `json:"phase"`
}

// Validate checks format and internal revision ordering. Only a guarded native
// observation can supply a cutoff/view or authenticate durable coverage.
func (r cleanupBackfillRecord) Validate() error {
	if r.Version != 1 || !validWorkerRoot(r.Namespace) || !validDomainSegment(r.RestoreEpoch) || !validHexDigest(r.IdentityDigest) {
		return ErrInvalidRecord
	}
	if r.LegacyCutoffRevision <= 0 || r.ReadViewRevision < r.LegacyCutoffRevision {
		return ErrInvalidRecord
	}
	if r.Frontier != "" && !validPreparationUUID(r.Frontier) || r.Phase != "list" && r.Phase != "complete" {
		return ErrInvalidRecord
	}
	return nil
}

func (n Namespace) cleanupBackfillKey(partition uint8) (string, error) {
	return n.Key("p", fmt.Sprintf("%02x", partition), "cleanup-pending-backfill")
}

func encodeCleanupBackfill(r cleanupBackfillRecord) (string, error) {
	return encodeOperationMetadata(r, cleanupBackfillBytes)
}

// Mutable B remains permanent and canonical. Its view was observed before its
// latest CAS, while its cutoff predates its first creation. Exact native fences
// separately reject ABA; a valid decoded record is not an adoptable owner.
func decodeCleanupBackfill(n Namespace, epoch, identityDigest string, partition uint8, kv *mvccpb.KeyValue, revision int64) (cleanupBackfillRecord, error) {
	var r cleanupBackfillRecord
	key, err := n.cleanupBackfillKey(partition)
	if err != nil || revision <= 0 || kv == nil || string(kv.Key) != key || kv.Lease != 0 || kv.Version < 1 {
		return r, ErrCorruptRecord
	}
	if kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || kv.ModRevision > revision {
		return r, ErrCorruptRecord
	}
	if (kv.Version == 1) != (kv.CreateRevision == kv.ModRevision) || len(kv.Value) == 0 || len(kv.Value) > cleanupBackfillBytes {
		return r, ErrCorruptRecord
	}
	if strictPreparationMetadata(kv.Value, reflect.TypeOf(r)) != nil || json.Unmarshal(kv.Value, &r) != nil || r.Validate() != nil {
		return cleanupBackfillRecord{}, ErrCorruptRecord
	}
	if r.Namespace != n.Root() || r.RestoreEpoch != epoch || r.IdentityDigest != identityDigest || r.Partition != partition {
		return cleanupBackfillRecord{}, ErrCorruptRecord
	}
	if r.LegacyCutoffRevision >= kv.CreateRevision || r.ReadViewRevision >= kv.ModRevision {
		return cleanupBackfillRecord{}, ErrCorruptRecord
	}
	wire, err := encodeCleanupBackfill(r)
	if err != nil || !bytes.Equal(kv.Value, []byte(wire)) {
		return cleanupBackfillRecord{}, ErrCorruptRecord
	}
	return r, nil
}

// This pure validator deliberately cannot advance coverage. Refresh may change
// only the current read view; actual CAS callers must also own the native
// observation that supplied it. Completed checkpoints cannot be reopened.
func validateCleanupBackfillRefresh(prior *cleanupBackfillRecord, next cleanupBackfillRecord) error {
	if next.Validate() != nil || next.Phase != "list" {
		return ErrInvalidRecord
	}
	if prior == nil {
		if next.Frontier != "" || next.ReadViewRevision != next.LegacyCutoffRevision {
			return ErrInvalidRecord
		}
		return nil
	}
	if prior.Validate() != nil || prior.Phase != "list" || next.ReadViewRevision <= prior.ReadViewRevision {
		return ErrInvalidRecord
	}
	expected := *prior
	expected.ReadViewRevision = next.ReadViewRevision
	if expected != next {
		return ErrInvalidRecord
	}
	return nil
}
