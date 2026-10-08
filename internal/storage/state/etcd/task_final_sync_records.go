package etcd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

// TaskFinalSyncPlan declares the intended mode. Mode none is not evidence of
// absent remote effects, and the digest grants no authority to send requests.
type TaskFinalSyncPlan struct {
	Mode   string `json:"mode"`
	Digest string `json:"digest"`
}
type TaskFinalSyncAttemptReference struct {
	Task      TaskReference `json:"task"`
	AttemptID string        `json:"attempt_id"`
}

// TaskFinalSyncAttemptRecord is permanent metadata attribution. Its worker is
// the original claim's worker, not an OS incarnation or runtime Boot proof.
type TaskFinalSyncAttemptRecord struct {
	Version            uint32
	Task               TaskRecord
	Claim              TaskClaimReference
	DestroyingRevision int64
	Quiescence         TaskQuiescenceReference
	QuiescenceRevision int64
	ReceiptDigest      string
	Plan               TaskFinalSyncPlan
	AttemptID          string
}
type TaskFinalSyncAttemptEntry struct {
	Reference TaskFinalSyncAttemptReference
	Record    *TaskFinalSyncAttemptRecord
	Revision  int64
}

// Outcome concerns only the native metadata reservation, never provider effects.
type TaskFinalSyncReservationResult struct {
	Reference TaskFinalSyncAttemptReference
	Outcome   Outcome
}
type taskFinalSyncDraft struct {
	prepared       *PreparedTaskQuiescence
	record         TaskFinalSyncAttemptRecord
	value          string
	deadline       time.Time
	writeAttempted bool
	revision       int64
	lost           bool
	archive        *taskArchiveDraft
}

type taskFinalSyncQuiescenceWire struct {
	Task      TaskReference  `json:"task"`
	CommandID string         `json:"command_id"`
	Stage     StageReference `json:"stage"`
}
type taskFinalSyncRecordWire struct {
	Version            uint32                      `json:"version"`
	Task               TaskRecord                  `json:"task"`
	Claim              taskCloseClaimWire          `json:"claim"`
	DestroyingRevision int64                       `json:"destroying_revision"`
	Quiescence         taskFinalSyncQuiescenceWire `json:"quiescence"`
	QuiescenceRevision int64                       `json:"quiescence_revision"`
	ReceiptDigest      string                      `json:"receipt_digest"`
	Plan               TaskFinalSyncPlan           `json:"plan"`
	AttemptID          string                      `json:"attempt_id"`
}

func taskFinalSyncWire(r TaskFinalSyncAttemptRecord) taskFinalSyncRecordWire {
	c, q := r.Claim, r.Quiescence
	return taskFinalSyncRecordWire{r.Version, r.Task, taskCloseClaimWire{c.Task, c.ClaimID, c.WorkerID, c.CreateRevision, c.LeaseID}, r.DestroyingRevision, taskFinalSyncQuiescenceWire{q.Task, q.CommandID, q.Stage}, r.QuiescenceRevision, r.ReceiptDigest, r.Plan, r.AttemptID}
}
func (w taskFinalSyncRecordWire) record() TaskFinalSyncAttemptRecord {
	c, q := w.Claim, w.Quiescence
	return TaskFinalSyncAttemptRecord{w.Version, w.Task, TaskClaimReference{Task: c.Task, ClaimID: c.ClaimID, WorkerID: c.WorkerID, CreateRevision: c.CreateRevision, LeaseID: c.LeaseID}, w.DestroyingRevision, TaskQuiescenceReference{q.Task, q.CommandID, q.Stage}, w.QuiescenceRevision, w.ReceiptDigest, w.Plan, w.AttemptID}
}
func (p TaskFinalSyncPlan) Validate() error {
	if (p.Mode != "sync" && p.Mode != "fuse" && p.Mode != "none") || !validHexDigest(p.Digest) {
		return ErrInvalidRecord
	}
	return nil
}
func (r TaskFinalSyncAttemptReference) Validate() error {
	if r.Task.Validate() != nil || !validPreparationUUID(r.AttemptID) {
		return ErrInvalidRecord
	}
	return nil
}
func (r TaskFinalSyncAttemptRecord) Validate() error {
	if r.Version != 1 || r.Task.Validate() != nil || r.Claim.Task != r.Task.Reference || !validPreparationUUID(r.Claim.ClaimID) || !validDomainSegment(r.Claim.WorkerID) || r.Claim.LeaseID <= 0 || r.DestroyingRevision <= r.Task.ControlRevision || r.Claim.CreateRevision <= r.DestroyingRevision || r.Quiescence.Validate() != nil || r.Quiescence.Task != r.Task.Reference || r.QuiescenceRevision <= r.DestroyingRevision || !validHexDigest(r.ReceiptDigest) || r.Plan.Validate() != nil || !validPreparationUUID(r.AttemptID) {
		return ErrInvalidRecord
	}
	return nil
}
func (r TaskFinalSyncAttemptRecord) reference() TaskFinalSyncAttemptReference {
	return TaskFinalSyncAttemptReference{r.Task.Reference, r.AttemptID}
}
func encodeTaskFinalSyncAttempt(r TaskFinalSyncAttemptRecord) (string, error) {
	if r.Validate() != nil {
		return "", ErrInvalidRecord
	}
	w, err := json.Marshal(taskFinalSyncWire(r))
	if err != nil || len(w) > 8192 {
		return "", ErrInvalidRecord
	}
	return string(w), nil
}
func decodeTaskFinalSyncAttempt(kv *mvccpb.KeyValue, dst *TaskFinalSyncAttemptRecord) error {
	if dst == nil {
		return ErrCorruptRecord
	}
	*dst = TaskFinalSyncAttemptRecord{}
	if !immutableDispatchKV(kv) || kv.Version != 1 || len(kv.Value) > 8192 || !taskCloseJSON(kv.Value) || strictPreparationMetadata(kv.Value, reflect.TypeOf(taskFinalSyncRecordWire{})) != nil {
		return ErrCorruptRecord
	}
	var w taskFinalSyncRecordWire
	if json.Unmarshal(kv.Value, &w) != nil {
		return ErrCorruptRecord
	}
	r := w.record()
	canonical, err := encodeTaskFinalSyncAttempt(r)
	if err != nil || !bytes.Equal(kv.Value, []byte(canonical)) || kv.CreateRevision <= r.Claim.CreateRevision || kv.CreateRevision <= r.QuiescenceRevision || string(kv.Key) != fmt.Sprintf("%sp/%02x/tasks/%s/final-sync-attempt", r.Task.Reference.Namespace, r.Task.Reference.Partition, r.Task.Reference.TaskID) {
		return ErrCorruptRecord
	}
	*dst = r
	return nil
}
func (n Namespace) taskFinalSyncAttemptKey(r TaskReference) (string, error) {
	parts, err := n.taskKeyParts(r)
	if err != nil {
		return "", err
	}
	return n.Key(append(parts, "final-sync-attempt")...)
}
func (b *Backend) LoadTaskFinalSyncAttempt(ctx context.Context, task TaskReference) (*TaskFinalSyncAttemptEntry, error) {
	if b == nil || ctx == nil {
		return nil, ErrInvalidRecord
	}
	if err := b.validateTaskReference(task); err != nil {
		return nil, err
	}
	key, err := b.namespace.taskFinalSyncAttemptKey(task)
	if err != nil {
		return nil, err
	}
	bounded, cancel := b.requestContext(ctx)
	defer cancel()
	points, err := b.readQuiescencePoints(bounded, nil, []string{b.identityKey, b.restoreKey, key})
	if err != nil {
		return nil, err
	}
	entry, err := taskFinalSyncEntry(points[2], task)
	if err != nil || entry == nil {
		return entry, err
	}
	if err = b.validateTaskFinalSyncHistory(bounded, entry.Record, nil); err != nil {
		return nil, err
	}
	return entry, nil
}
func taskFinalSyncEntry(kv *mvccpb.KeyValue, task TaskReference) (*TaskFinalSyncAttemptEntry, error) {
	if kv == nil {
		return nil, nil
	}
	var r TaskFinalSyncAttemptRecord
	if err := decodeTaskFinalSyncAttempt(kv, &r); err != nil {
		return nil, err
	}
	if r.Task.Reference != task {
		return nil, ErrCorruptRecord
	}
	return &TaskFinalSyncAttemptEntry{r.reference(), &r, kv.ModRevision}, nil
}
func (b *Backend) validateTaskFinalSyncHistory(ctx context.Context, r *TaskFinalSyncAttemptRecord, pin *taskDualDrainPin) error {
	q, err := b.loadTaskQuiescence(ctx, r.Task.Reference, nil)
	if err != nil {
		return err
	}
	if q == nil || q.Record == nil || q.Outcome != OutcomeCommitted || q.Reference != r.Quiescence || q.Revision != r.QuiescenceRevision || q.Record.Task != r.Task || q.Record.Context.Current.ControlRevision != r.DestroyingRevision || q.Record.Claim.CreateRevision > r.Claim.CreateRevision || (q.Record.Claim.CreateRevision == r.Claim.CreateRevision && q.Record.Claim != r.Claim) {
		return ErrCorruptRecord
	}
	if pin != nil {
		points, err := b.readQuiescencePoints(ctx, nil, []string{b.identityKey, b.restoreKey, pin.intent.key, pin.receipt.key, pin.reservation.key})
		if err != nil {
			return err
		}
		for i, f := range []taskFence{pin.intent, pin.receipt} {
			kv := points[i+2]
			if !immutableDispatchKV(kv) || kv.CreateRevision != f.create || kv.ModRevision != f.mod || string(kv.Value) != f.value {
				return ErrConflict
			}
		}
		kv := points[4]
		if !immutableDispatchKV(kv) || kv.CreateRevision != pin.reservation.revision || q.reservation == nil || q.reservation.Revision != pin.reservation.revision {
			return ErrConflict
		}
	}
	return nil
}
