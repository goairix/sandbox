package etcd

import (
	"archive/tar"
	"context"
	"errors"
	"math"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func (s *taskWorkspaceSyncRecoverySource) checkpointContext() TaskWorkspaceSyncCheckpoint {
	m := s.manifest
	return TaskWorkspaceSyncCheckpoint{Version: 1, FinalSyncAttemptID: m.Seed.FinalSyncAttempt, PlanDigest: s.plan.digest, ArchiveSHA256: m.Received.SHA256, ArchiveBytes: m.Received.Bytes, ArchiveEntries: m.Received.Entries, TicketDigest: m.Seed.TicketDigest, TargetDigest: m.Binding.TargetDigest}
}

// A prior claimant's checkpoint is history only when strictly older than this
// genuine fresh claim. This does not relax captureWorkspaceSyncCheckpoint.
func (s *taskWorkspaceSyncRecoverySource) decodeCheckpoint(kv *mvccpb.KeyValue) (TaskCheckpointRecord, error) {
	c := s.authority.claim
	r, err := decodeTaskCheckpointAt(kv, c.reference.Task, c.birth, c.initial)
	if err != nil {
		return r, err
	}
	if string(kv.Key) != c.checkpointKey || !recyclingCanAdvance(kv.Version) || kv.ModRevision == math.MaxInt64 || r.State != TaskCheckpointPending || r.Attempt != c.initial {
		return r, ErrCorruptRecord
	}
	canonical, err := encodeTaskCheckpointRecord(r)
	if err != nil || canonical != string(kv.Value) {
		return r, ErrCorruptRecord
	}
	if r.Version == 1 {
		if kv.Version != 1 || kv.ModRevision != c.birth || r.ClaimID != "" || r.DetailDigest != "" {
			return r, ErrConflict
		}
	} else {
		if r.WorkspaceSync == nil || !workspaceSyncCheckpointSameContext(*r.WorkspaceSync, s.checkpointContext()) || r.ClaimID != c.reference.ClaimID && kv.ModRevision >= c.reference.CreateRevision || r.ClaimID == c.reference.ClaimID && kv.ModRevision <= c.reference.CreateRevision || r.WorkspaceSync.IntentBirth <= s.manifest.Seed.ArchiveRevision || r.WorkspaceSync.IntentBirth >= kv.ModRevision {
			return r, ErrConflict
		}
	}
	return r, nil
}

func (s *taskWorkspaceSyncRecoverySource) readCheckpoint(ctx context.Context) (*mvccpb.KeyValue, error) {
	b, c := s.authority.backend, s.authority.claim
	keys := []string{c.checkpointKey}
	r, err := s.native(ctx, s.comparisons(nil), workspaceSyncPointOps(keys), taskProtoPoints(keys), taskProtoResponse(keys, []string{string(make([]byte, 4096))}), false)
	if err != nil {
		return nil, err
	}
	points, err := workspaceSyncPointResponse(b, r, keys)
	if err != nil {
		return nil, err
	}
	if _, err := s.decodeCheckpoint(points[0]); err != nil {
		return nil, err
	}
	return points[0], nil
}

func (s *taskWorkspaceSyncRecoverySource) restoreCheckpoint(ctx context.Context) error {
	if err := s.begin(ctx); err != nil {
		return err
	}
	defer s.end()
	if err := s.guard(ctx); err != nil {
		return err
	}
	kv, err := s.readCheckpoint(ctx)
	if err != nil {
		return err
	}
	r, err := s.decodeCheckpoint(kv)
	if err != nil {
		return err
	}
	s.checkpointKV = kv
	if r.Version == 1 {
		return s.guard(ctx)
	}
	p := *r.WorkspaceSync
	for s.cursor.count < p.EntryOrdinal {
		e, err := s.cursor.next(ctx)
		if err != nil {
			return errors.Join(ErrCorruptRecord, err)
		}
		if s.cursor.count > p.EntryOrdinal {
			return ErrCorruptRecord
		}
		if s.cursor.count == p.EntryOrdinal {
			if e.kind != tar.TypeReg || uint64(e.offset+e.size) != p.BodyEnd || workspaceSyncIntentDigest(e.path) != p.PathDigest || e.sha256 != p.BodySHA256 {
				return ErrCorruptRecord
			}
		}
	}
	s.checkpoint = &p
	return s.guard(ctx)
}

func (s *taskWorkspaceSyncRecoverySource) captureCheckpoint(ctx context.Context, q *taskWorkspaceSyncRecoveryCompletion) error {
	if q.checkpoint != nil {
		return nil
	}
	c, charge := s.authority.claim, q.charge
	p := s.checkpointContext()
	p.EntryOrdinal, p.BodyEnd = s.cursor.count, uint64(charge.record.Offset+charge.record.Size)
	p.PathDigest, p.BodySHA256 = workspaceSyncIntentDigest(charge.record.RelativePath), charge.record.BodySHA256
	p.IntentID, p.IntentBirth = charge.record.Reference.IntentID, charge.birth
	var terminal taskWorkspaceSyncRecyclingSlotRecord
	if decodePreparationDomain(&mvccpb.KeyValue{Value: []byte(q.value)}, &terminal, 512) != nil {
		return ErrInvalidRecord
	}
	p.EvidenceDigest = terminal.EvidenceDigest
	if p.Validate() != nil {
		return ErrInvalidRecord
	}
	kv, err := s.readCheckpoint(ctx)
	if err != nil {
		return err
	}
	before, err := s.decodeCheckpoint(kv)
	if err != nil {
		return err
	}
	if before.Version == 2 && (p.EntryOrdinal <= before.WorkspaceSync.EntryOrdinal || p.BodyEnd <= before.WorkspaceSync.BodyEnd) {
		return ErrConflict
	}
	if s.checkpointKV == nil || kv.CreateRevision != s.checkpointKV.CreateRevision || kv.ModRevision != s.checkpointKV.ModRevision || kv.Version != s.checkpointKV.Version || string(kv.Value) != string(s.checkpointKV.Value) {
		return ErrConflict
	}
	payload, err := encodeOperationMetadata(p, 4096)
	if err != nil {
		return err
	}
	value, err := encodeTaskCheckpointRecord(TaskCheckpointRecord{Version: 2, Reference: c.reference.Task, ClaimID: c.reference.ClaimID, DetailDigest: workspaceSyncIntentDigest(payload), State: TaskCheckpointPending, Attempt: c.initial, WorkspaceSync: &p})
	if err != nil {
		return err
	}
	q.payload = p
	q.checkpoint = &taskWorkspaceSyncCheckpointAttempt{key: c.checkpointKey, before: taskWorkspaceSyncRecyclingGeneration{kv.CreateRevision, kv.ModRevision, kv.Version, string(kv.Value)}, value: value}
	return s.guard(ctx)
}

func (s *taskWorkspaceSyncRecoverySource) recycleLocal(ctx context.Context, charge *taskWorkspaceSyncRecoveryCharge, q *taskWorkspaceSyncRecoveryCompletion) (Outcome, error) {
	if err := s.begin(ctx); err != nil {
		return OutcomeUnknown, err
	}
	defer s.end()
	if q != nil && q.self == q && q.source == s && q.charge == charge && q.completed && q.body != nil && q.body.completion == q {
		return OutcomeCommitted, s.guard(ctx)
	}
	if err := s.validCompletion(q); err != nil || q.charge != charge || !q.known {
		return OutcomeUnknown, errors.Join(ErrInvalidRecord, err)
	}
	if q.gcDenied {
		return OutcomeUnknown, ErrConflict
	}
	if err := s.captureCheckpoint(ctx, q); err != nil {
		return OutcomeUnknown, err
	}
	p, g, ref := q.checkpoint, q.terminal, charge.record.Reference
	free, err := encodeWorkspaceSyncRecyclingSlot(taskWorkspaceSyncRecyclingSlotRecord{Version: 2, BudgetID: ref.BudgetID, Partition: ref.Task.Partition, Slot: ref.Slot, State: "free"})
	if err != nil {
		return OutcomeUnknown, err
	}
	var committedRevision int64
	if q.gcAttempted {
		points, err := s.readRecoveryCompletion(ctx, charge, p.key)
		if err != nil {
			return OutcomeUnknown, err
		}
		if points[1] != nil || !recyclingGenerationMatches(points[0], g.create, g.version+1, g.mod, free) || !workspaceSyncCheckpointGenerationMatches(points[2], p, points[0]) {
			return OutcomeUnknown, ErrOutcomeUnknown
		}
		committedRevision = points[0].ModRevision
	} else {
		if err := s.guard(ctx); err != nil {
			return OutcomeUnknown, err
		}
		cmps := append(s.comparisons(charge), clientv3.Compare(clientv3.ModRevision(charge.slotKey), "=", g.mod), clientv3.Compare(clientv3.Value(charge.slotKey), "=", g.value), clientv3.Compare(clientv3.ModRevision(p.key), "=", p.before.mod), clientv3.Compare(clientv3.Value(p.key), "=", p.before.value))
		q.gcAttempted = true
		writes := []Write{{Key: charge.slotKey, Value: []byte(free)}, {Key: charge.intentKey, Delete: true}, {Key: p.key, Value: []byte(p.value)}}
		r, err := s.native(ctx, cmps, []clientv3.Op{clientv3.OpPut(charge.slotKey, free), clientv3.OpDelete(charge.intentKey), clientv3.OpPut(p.key, p.value)}, taskProtoWrites(writes), recyclingGCReply(), true)
		if err != nil {
			q.gcDenied = r != nil && !r.Succeeded
			return OutcomeUnknown, err
		}
		if len(r.Responses) != 3 || r.Header.Revision <= g.mod || r.Header.Revision <= p.before.mod || r.Header.Revision == math.MaxInt64 || r.Responses[0] == nil || r.Responses[1] == nil || r.Responses[2] == nil {
			return OutcomeUnknown, ErrOutcomeUnknown
		}
		put, deleted, checkpoint := r.Responses[0].GetResponsePut(), r.Responses[1].GetResponseDeleteRange(), r.Responses[2].GetResponsePut()
		if put == nil || put.PrevKv != nil || !operationNestedHeader(put.Header, r.Header) || deleted == nil || deleted.Deleted != 1 || len(deleted.PrevKvs) != 0 || !operationNestedHeader(deleted.Header, r.Header) || checkpoint == nil || checkpoint.PrevKv != nil || !operationNestedHeader(checkpoint.Header, r.Header) {
			return OutcomeUnknown, ErrOutcomeUnknown
		}
		committedRevision = r.Header.Revision
	}
	// The atomic ACK/arbitration has proved free+deleted+checkpoint. Keep all
	// source/target registrations through the subsequent physical postguard.
	q.body.charge = nil
	if err := s.guard(ctx); err != nil {
		q.body.charge = charge
		return OutcomeUnknown, err
	}
	payload := q.payload
	s.checkpoint, s.checkpointKV = &payload, &mvccpb.KeyValue{Key: []byte(p.key), Value: []byte(p.value), CreateRevision: p.before.create, ModRevision: committedRevision, Version: p.before.version + 1}
	s.completedFiles++
	s.completedBytes += uint64(charge.record.Size)
	q.body.settled, q.completed = true, true
	return OutcomeCommitted, nil
}
