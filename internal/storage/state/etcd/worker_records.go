package etcd

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

const workerProfileID = "partition-worker-v1"
const workerRecordBytes = 2048

// WorkerControlOptions enables the existing operator-provisioned control
// profile. SessionTTL is integral seconds in [3,60]; zero selects 15 seconds.
// These controls provide scheduling admission, never task/runtime authority.
type WorkerControlOptions struct{ SessionTTL time.Duration }

// WorkerSessionReference is diagnostic identity. It cannot be adopted to gain
// authority, and its Lease and incarnation never change during renewal.
type WorkerSessionReference struct {
	Namespace, RestoreEpoch, IdentityDigest, ProfileID string
	WorkerID, SessionID                                string
	LeaseID, CreateRevision                            int64
}

// WorkerPartitionReference identifies one original partition claim.
type WorkerPartitionReference struct {
	Session        WorkerSessionReference
	Partition      uint8
	CreateRevision int64
}

type workerProfileRecord struct {
	Version             uint32 `json:"version"`
	ProfileID           string `json:"profile_id"`
	Namespace           string `json:"namespace"`
	RestoreEpoch        string `json:"restore_epoch"`
	IdentityDigest      string `json:"identity_digest"`
	PartitionCount      uint32 `json:"partition_count"`
	RelistSlots         uint32 `json:"relist_slots"`
	RangeSlots          uint32 `json:"range_slots"`
	RangeBytesPerSecond uint32 `json:"range_bytes_per_second"`
	RangeBurstBytes     uint32 `json:"range_burst_bytes"`
	ClaimSlots          uint32 `json:"claim_slots"`
	ClaimPerSecond      uint32 `json:"claim_per_second"`
	ClaimBurst          uint32 `json:"claim_burst"`
}

func workerIdentityDigest(value string) string {
	d := sha256.Sum256([]byte(value))
	return hex.EncodeToString(d[:])
}

func validWorkerRoot(root string) bool {
	if len(root) < 2 || !strings.HasPrefix(root, "/") || !strings.HasSuffix(root, "/") {
		return false
	}
	parts := strings.Split(strings.TrimSuffix(root, "/")[1:], "/")
	if len(parts) < 3 {
		return false
	}
	n, err := NewNamespace("/"+strings.Join(parts[:len(parts)-2], "/"), parts[len(parts)-2], parts[len(parts)-1])
	return err == nil && n.Root() == root
}

func validWorkerDigest(d string) bool {
	if len(d) != 64 || d != strings.ToLower(d) {
		return false
	}
	_, err := hex.DecodeString(d)
	return err == nil
}

func (r workerProfileRecord) Validate() error {
	if r.Version != 1 || r.ProfileID != workerProfileID || !validWorkerRoot(r.Namespace) || !validDomainSegment(r.RestoreEpoch) || !validWorkerDigest(r.IdentityDigest) || r.PartitionCount != 256 || r.RelistSlots != 4 || r.RangeSlots != 20 || r.RangeBytesPerSecond != 1048576 || r.RangeBurstBytes != 262144 || r.ClaimSlots != 100 || r.ClaimPerSecond != 1 || r.ClaimBurst != 1 {
		return ErrInvalidRecord
	}
	return nil
}

type workerSessionRecord struct {
	Version        uint32 `json:"version"`
	Namespace      string `json:"namespace"`
	RestoreEpoch   string `json:"restore_epoch"`
	IdentityDigest string `json:"identity_digest"`
	ProfileID      string `json:"profile_id"`
	WorkerID       string `json:"worker_id"`
	SessionID      string `json:"session_id"`
	LeaseID        int64  `json:"lease_id"`
}

func (r workerSessionRecord) Validate() error {
	if r.Version != 1 || !validWorkerRoot(r.Namespace) || !validDomainSegment(r.RestoreEpoch) || !validWorkerDigest(r.IdentityDigest) || r.ProfileID != workerProfileID || !validDomainSegment(r.WorkerID) || !validPreparationUUID(r.SessionID) || r.LeaseID <= 0 {
		return ErrInvalidRecord
	}
	return nil
}

type workerPartitionRecord struct {
	workerSessionRecord
	Partition uint8 `json:"partition"`
}

func encodeWorkerSessionRecord(r workerSessionRecord) (string, error) {
	return encodeOperationMetadata(r, workerRecordBytes)
}
func encodeWorkerPartitionRecord(r workerPartitionRecord) (string, error) {
	return encodeOperationMetadata(r, workerRecordBytes)
}

func decodeWorkerSessionRecord(n Namespace, kv *mvccpb.KeyValue, dst *workerSessionRecord) error {
	if dst == nil {
		return ErrCorruptRecord
	}
	var fresh workerSessionRecord
	if workerDecodeLeased(kv, &fresh) != nil || fresh.Namespace != n.Root() {
		return ErrCorruptRecord
	}
	key, err := n.workerSessionKey(fresh.SessionID)
	if err != nil || string(kv.Key) != key || fresh.LeaseID != kv.Lease {
		return ErrCorruptRecord
	}
	*dst = fresh
	return nil
}

func decodeWorkerPartitionRecord(n Namespace, kv *mvccpb.KeyValue, dst *workerPartitionRecord) error {
	if dst == nil {
		return ErrCorruptRecord
	}
	var fresh workerPartitionRecord
	if workerDecodeLeased(kv, &fresh) != nil || fresh.Namespace != n.Root() {
		return ErrCorruptRecord
	}
	key, err := n.workerPartitionKey(fresh.Partition)
	if err != nil || string(kv.Key) != key || fresh.LeaseID != kv.Lease {
		return ErrCorruptRecord
	}
	*dst = fresh
	return nil
}

func workerDecodeLeased[T interface{ Validate() error }](kv *mvccpb.KeyValue, dst *T) error {
	if kv == nil || dst == nil || kv.Lease <= 0 || kv.CreateRevision <= 0 || kv.ModRevision != kv.CreateRevision || kv.Version != 1 {
		return ErrCorruptRecord
	}
	copy := *kv
	copy.Lease = 0
	var fresh T
	if decodePreparationDomain(&copy, &fresh, workerRecordBytes) != nil {
		return ErrCorruptRecord
	}
	wire, err := encodeOperationMetadata(fresh, workerRecordBytes)
	if err != nil || wire != string(kv.Value) {
		return ErrCorruptRecord
	}
	*dst = fresh
	return nil
}

func validateWorkerControl(o *WorkerControlOptions) error {
	if o != nil && o.SessionTTL != 0 && (o.SessionTTL < 3*time.Second || o.SessionTTL > 60*time.Second || o.SessionTTL%time.Second != 0) {
		return ErrInvalidConfiguration
	}
	return nil
}
