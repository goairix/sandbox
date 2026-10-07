package etcd

import (
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// TaskClaimReference is owned diagnostic identity, never an adoptable claim.
type TaskClaimReference struct {
	Task                    TaskReference
	ClaimID, WorkerID       string
	CreateRevision, LeaseID int64
}

type taskClaimRecord struct {
	Version  uint32        `json:"version"`
	Task     TaskReference `json:"task"`
	ClaimID  string        `json:"claim_id"`
	WorkerID string        `json:"worker_id"`
	LeaseID  int64         `json:"lease_id"`
}

func (r taskClaimRecord) Validate() error {
	if r.Version != 1 || r.Task.Validate() != nil || !validPreparationUUID(r.ClaimID) || !validDomainSegment(r.WorkerID) || r.LeaseID <= 0 {
		return ErrInvalidRecord
	}
	return nil
}
func encodeTaskClaimRecord(r taskClaimRecord) (string, error) {
	return encodeOperationMetadata(r, 4096)
}
func decodeTaskClaimRecord(kv *mvccpb.KeyValue, dst *taskClaimRecord) error {
	if kv == nil || dst == nil || kv.Lease <= 0 || kv.CreateRevision <= 0 || kv.ModRevision != kv.CreateRevision {
		return ErrCorruptRecord
	}
	// Reuse the strict permanent JSON walker on a local envelope copy; the actual
	// leased immutable envelope and encoded original Lease are checked here.
	permanent := *kv
	permanent.Lease = 0
	var fresh taskClaimRecord
	if err := decodePreparationDomain(&permanent, &fresh, 4096); err != nil {
		return err
	}
	if fresh.LeaseID != kv.Lease {
		return ErrCorruptRecord
	}
	*dst = fresh
	return nil
}

type taskFence struct {
	key, value         string
	create, mod, lease int64
}

func ownTaskFence(kv *mvccpb.KeyValue) taskFence {
	return taskFence{string(kv.Key), string(kv.Value), kv.CreateRevision, kv.ModRevision, kv.Lease}
}
func (f taskFence) comparisons() []clientv3.Cmp {
	return []clientv3.Cmp{clientv3.Compare(clientv3.Value(f.key), "=", f.value), clientv3.Compare(clientv3.LeaseValue(f.key), "=", f.lease), clientv3.Compare(clientv3.CreateRevision(f.key), "=", f.create), clientv3.Compare(clientv3.ModRevision(f.key), "=", f.mod)}
}
func (f taskFence) matches(kv *mvccpb.KeyValue) bool {
	return kv != nil && string(kv.Key) == f.key && string(kv.Value) == f.value && kv.Lease == f.lease && kv.CreateRevision == f.create && kv.ModRevision == f.mod
}

func decodeTaskCheckpointAt(kv *mvccpb.KeyValue, ref TaskReference, birth int64, initial StageAttemptLocator) (TaskCheckpointRecord, error) {
	var record TaskCheckpointRecord
	if err := decodeTaskCheckpointRecord(kv, &record); err != nil {
		return record, err
	}
	if record.Reference != ref || kv.CreateRevision != birth {
		return TaskCheckpointRecord{}, ErrCorruptRecord
	}
	if kv.ModRevision == birth {
		if record.ClaimID != "" || record.DetailDigest != "" || record.State != TaskCheckpointPending || record.Attempt != initial {
			return TaskCheckpointRecord{}, ErrCorruptRecord
		}
	} else if record.ClaimID == "" || record.DetailDigest == "" {
		return TaskCheckpointRecord{}, ErrCorruptRecord
	}
	return record, nil
}
