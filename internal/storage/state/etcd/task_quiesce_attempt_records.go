package etcd

import (
	"encoding/json"
	"fmt"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"reflect"
)

// TaskQuiescenceAttemptReference locates an original reservation, without an
// invented business digest or any authority to sign, allocate, or send.
type TaskQuiescenceAttemptReference struct {
	Task      TaskReference
	CommandID string
	Attempt   StageAttemptLocator
}
type TaskQuiescenceAttemptRecord struct {
	Version            uint32
	Task               TaskRecord
	Claim              TaskClaimReference
	DestroyingRevision int64
	CommandID          string
	Attempt            StageAttemptLocator
}
type TaskQuiescenceAttemptEntry struct {
	Reference TaskQuiescenceAttemptReference
	Record    *TaskQuiescenceAttemptRecord
	Revision  int64
}
type taskQuiescenceAttemptWire struct {
	Version            uint32              `json:"version"`
	Task               TaskRecord          `json:"task"`
	Claim              taskCloseClaimWire  `json:"claim"`
	DestroyingRevision int64               `json:"destroying_revision"`
	CommandID          string              `json:"command_id"`
	Attempt            StageAttemptLocator `json:"attempt"`
}

func (r TaskQuiescenceAttemptReference) Validate() error {
	if r.Task.Validate() != nil || !validPreparationUUID(r.CommandID) || !validTaskQuiescenceAttempt(r.Attempt, r.Task, r.Attempt.RequestID) {
		return ErrInvalidRecord
	}
	return nil
}
func (r TaskQuiescenceAttemptRecord) Validate() error {
	if r.Version != 1 || r.Task.Validate() != nil || r.Claim.Task != r.Task.Reference || !validPreparationUUID(r.Claim.ClaimID) || !validDomainSegment(r.Claim.WorkerID) || r.Claim.LeaseID <= 0 || r.Claim.CreateRevision <= r.DestroyingRevision || r.DestroyingRevision <= r.Task.ControlRevision || !validPreparationUUID(r.CommandID) || !validTaskQuiescenceAttempt(r.Attempt, r.Task.Reference, r.Claim.ClaimID) {
		return ErrInvalidRecord
	}
	return nil
}
func (r TaskQuiescenceAttemptRecord) reference() TaskQuiescenceAttemptReference {
	return TaskQuiescenceAttemptReference{r.Task.Reference, r.CommandID, r.Attempt}
}
func encodeTaskQuiescenceAttempt(r TaskQuiescenceAttemptRecord) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	c := r.Claim
	w, err := json.Marshal(taskQuiescenceAttemptWire{r.Version, r.Task, taskCloseClaimWire{c.Task, c.ClaimID, c.WorkerID, c.CreateRevision, c.LeaseID}, r.DestroyingRevision, r.CommandID, r.Attempt})
	if err != nil || len(w) > 8192 {
		return "", ErrInvalidRecord
	}
	return string(w), nil
}
func decodeTaskQuiescenceAttempt(kv *mvccpb.KeyValue, dst *TaskQuiescenceAttemptRecord) error {
	if dst == nil {
		return ErrCorruptRecord
	}
	*dst = TaskQuiescenceAttemptRecord{}
	if !immutableDispatchKV(kv) || len(kv.Value) > 8192 || !taskCloseJSON(kv.Value) || strictPreparationMetadata(kv.Value, reflect.TypeOf(taskQuiescenceAttemptWire{})) != nil {
		return ErrCorruptRecord
	}
	var w taskQuiescenceAttemptWire
	if json.Unmarshal(kv.Value, &w) != nil {
		return ErrCorruptRecord
	}
	c := w.Claim
	r := TaskQuiescenceAttemptRecord{w.Version, w.Task, TaskClaimReference{Task: c.Task, ClaimID: c.ClaimID, WorkerID: c.WorkerID, CreateRevision: c.CreateRevision, LeaseID: c.LeaseID}, w.DestroyingRevision, w.CommandID, w.Attempt}
	if r.Validate() != nil || kv.CreateRevision <= r.Claim.CreateRevision {
		return ErrCorruptRecord
	}
	key := fmt.Sprintf("%sp/%02x/tasks/%s/quiesce-users-attempt", r.Task.Reference.Namespace, r.Task.Reference.Partition, r.Task.Reference.TaskID)
	if string(kv.Key) != key {
		return ErrCorruptRecord
	}
	*dst = r
	return nil
}
