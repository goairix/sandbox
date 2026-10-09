package etcd

import (
	"math"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

// Fixed only on the original registered settlement, before its one GC request.
// Serialized diagnostics cannot reconstruct this attempt or its authority.
type taskWorkspaceSyncCheckpointAttempt struct {
	key    string
	before taskWorkspaceSyncRecyclingGeneration
	value  string
}

func (op *taskWorkspaceSyncSettlementOperation) checkpointPayload(q *taskWorkspaceSyncLocalSettlement) (TaskWorkspaceSyncCheckpoint, error) {
	s, d := op.source, q.draft
	r := d.record
	if r.Version != 2 || r.Local == nil || r.Validate() != nil || r.PlanDigest != s.plan.digest || r.ArchiveSHA256 != s.diagnostics.SHA256 || r.TicketDigest != s.diagnostics.TicketDigest || r.FinalSyncAttemptID != s.archive.owner.prepared.draft.finalSync.record.AttemptID || r.Claim != s.archive.owner.claim.reference || r.Offset > math.MaxInt64-r.Size {
		return TaskWorkspaceSyncCheckpoint{}, ErrInvalidRecord
	}
	target, err := encodeOperationMetadata(*r.Local, 16384)
	if err != nil {
		return TaskWorkspaceSyncCheckpoint{}, err
	}
	var terminal taskWorkspaceSyncRecyclingSlotRecord
	if decodePreparationDomain(&mvccpb.KeyValue{Value: []byte(q.value)}, &terminal, 512) != nil || terminal.State != "terminal" {
		return TaskWorkspaceSyncCheckpoint{}, ErrInvalidRecord
	}
	p := TaskWorkspaceSyncCheckpoint{Version: 1, FinalSyncAttemptID: r.FinalSyncAttemptID, PlanDigest: r.PlanDigest, ArchiveSHA256: r.ArchiveSHA256, ArchiveBytes: s.diagnostics.Bytes, ArchiveEntries: s.diagnostics.Entries, TicketDigest: r.TicketDigest, TargetDigest: workspaceSyncIntentDigest(target), EntryOrdinal: s.count, BodyEnd: uint64(r.Offset + r.Size), PathDigest: workspaceSyncIntentDigest(r.RelativePath), BodySHA256: r.BodySHA256, IntentID: r.Reference.IntentID, IntentBirth: d.revision, EvidenceDigest: terminal.EvidenceDigest}
	return p, p.Validate()
}

func workspaceSyncCheckpointSameContext(a, b TaskWorkspaceSyncCheckpoint) bool {
	return a.Version == b.Version && a.FinalSyncAttemptID == b.FinalSyncAttemptID && a.PlanDigest == b.PlanDigest && a.ArchiveSHA256 == b.ArchiveSHA256 && a.ArchiveBytes == b.ArchiveBytes && a.ArchiveEntries == b.ArchiveEntries && a.TicketDigest == b.TicketDigest && a.TargetDigest == b.TargetDigest
}

func captureWorkspaceSyncCheckpoint(kv *mvccpb.KeyValue, c *TaskClaim, next TaskWorkspaceSyncCheckpoint) (*taskWorkspaceSyncCheckpointAttempt, error) {
	r, err := decodeTaskCheckpointAt(kv, c.reference.Task, c.birth, c.initial)
	if err != nil {
		return nil, err
	}
	if string(kv.Key) != c.checkpointKey || !recyclingCanAdvance(kv.Version) || kv.ModRevision == math.MaxInt64 || next.Validate() != nil {
		return nil, ErrCorruptRecord
	}
	canonical, err := encodeTaskCheckpointRecord(r)
	if err != nil || canonical != string(kv.Value) {
		return nil, ErrCorruptRecord
	}
	if r.Version == 1 {
		if r.State != TaskCheckpointPending || r.ClaimID != "" || r.DetailDigest != "" || r.Attempt != c.initial || kv.Version != 1 || kv.ModRevision != c.birth {
			return nil, ErrConflict
		}
	} else if r.ClaimID != c.reference.ClaimID || r.Attempt != c.initial || !workspaceSyncCheckpointSameContext(*r.WorkspaceSync, next) || next.EntryOrdinal <= r.WorkspaceSync.EntryOrdinal || next.BodyEnd <= r.WorkspaceSync.BodyEnd {
		return nil, ErrConflict
	}
	payload, err := encodeOperationMetadata(next, 4096)
	if err != nil {
		return nil, err
	}
	value, err := encodeTaskCheckpointRecord(TaskCheckpointRecord{Version: 2, Reference: c.reference.Task, ClaimID: c.reference.ClaimID, DetailDigest: workspaceSyncIntentDigest(payload), State: TaskCheckpointPending, Attempt: c.initial, WorkspaceSync: &next})
	if err != nil {
		return nil, err
	}
	return &taskWorkspaceSyncCheckpointAttempt{key: c.checkpointKey, before: taskWorkspaceSyncRecyclingGeneration{kv.CreateRevision, kv.ModRevision, kv.Version, canonical}, value: value}, nil
}

func (op *taskWorkspaceSyncSettlementOperation) captureCheckpoint(q *taskWorkspaceSyncLocalSettlement) error {
	if q.checkpoint != nil {
		return nil
	}
	s := op.source
	c := s.archive.owner.claim
	keys := []string{c.checkpointKey}
	r, err := op.native(s.recyclingCompletionComparisons(q.draft, false), workspaceSyncPointOps(keys), taskProtoPoints(keys), taskProtoResponse(keys, []string{string(make([]byte, 4096))}))
	if err != nil {
		return err
	}
	points, err := workspaceSyncPointResponse(s.archive.owner.backend, r, keys)
	if err != nil {
		return err
	}
	next, err := op.checkpointPayload(q)
	if err != nil {
		return err
	}
	captured, err := captureWorkspaceSyncCheckpoint(points[0], c, next)
	if err != nil {
		return err
	}
	if err := op.guard(); err != nil {
		return err
	}
	q.checkpoint = captured
	return nil
}

func (op *taskWorkspaceSyncSettlementOperation) readGCCompletion(q *taskWorkspaceSyncLocalSettlement) ([]*mvccpb.KeyValue, error) {
	b := op.source.archive.owner.backend
	keys := []string{q.draft.slotKey, q.draft.intentKey, q.checkpoint.key}
	values := []string{string(make([]byte, 512)), string(make([]byte, b.workspaceSyncBudget.options.MaxIntentBytes)), string(make([]byte, 4096))}
	r, err := op.native(op.source.recyclingCompletionComparisons(q.draft, false), workspaceSyncPointOps(keys), taskProtoPoints(keys), taskProtoResponse(keys, values))
	if err != nil {
		return nil, err
	}
	return workspaceSyncPointResponse(b, r, keys)
}

func workspaceSyncCheckpointGenerationMatches(kv *mvccpb.KeyValue, p *taskWorkspaceSyncCheckpointAttempt, slot *mvccpb.KeyValue) bool {
	var r TaskCheckpointRecord
	return p != nil && kv != nil && slot != nil && decodeTaskCheckpointRecord(kv, &r) == nil && string(kv.Key) == p.key && kv.CreateRevision == p.before.create && kv.Version == p.before.version+1 && kv.ModRevision > p.before.mod && kv.ModRevision == slot.ModRevision && string(kv.Value) == p.value
}
