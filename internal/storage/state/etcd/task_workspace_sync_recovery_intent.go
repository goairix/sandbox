package etcd

import (
	"context"
	"encoding/hex"
	"errors"
	"math"

	"github.com/google/uuid"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func (s *taskWorkspaceSyncRecoverySource) readCharged(ctx context.Context, slot uint32) (*taskWorkspaceSyncRecoveryCharge, error) {
	b, c := s.authority.backend, s.authority.claim
	if slot >= b.workspaceSyncBudget.options.SlotsPerPartition {
		return nil, ErrInvalidRecord
	}
	ref := TaskWorkspaceSyncIntentReference{Task: c.reference.Task, BudgetID: b.workspaceSyncBudget.options.BudgetID, IntentID: s.manifest.Seed.ArchiveCommand, Slot: slot}
	_, slotKey, err := b.workspaceSyncRecyclingKeys(ref)
	if err != nil {
		return nil, err
	}
	keys := []string{slotKey}
	r, err := s.native(ctx, s.comparisons(nil), workspaceSyncPointOps(keys), taskProtoPoints(keys), taskProtoResponse(keys, []string{string(make([]byte, 512))}), false)
	if err != nil {
		return nil, err
	}
	points, err := workspaceSyncPointResponse(b, r, keys)
	if err != nil {
		return nil, err
	}
	slotRecord, err := decodeWorkspaceSyncRecyclingSlot(points[0])
	if err != nil || slotRecord.Slot != slot || slotRecord.Partition != ref.Task.Partition || slotRecord.BudgetID != ref.BudgetID {
		return nil, errors.Join(ErrCorruptRecord, err)
	}
	if slotRecord.State == "free" {
		return nil, nil
	}
	ref.IntentID = slotRecord.IntentID
	intentKey, _, err := b.workspaceSyncRecyclingKeys(ref)
	if err != nil {
		return nil, err
	}
	cmps := append(s.comparisons(nil), clientv3.Compare(clientv3.ModRevision(slotKey), "=", points[0].ModRevision), clientv3.Compare(clientv3.Value(slotKey), "=", string(points[0].Value)))
	keys = []string{slotKey, intentKey}
	r, err = s.native(ctx, cmps, workspaceSyncPointOps(keys), taskProtoPoints(keys), taskProtoResponse(keys, []string{string(make([]byte, 512)), string(make([]byte, b.workspaceSyncBudget.options.MaxIntentBytes))}), false)
	if err != nil {
		return nil, err
	}
	points, err = workspaceSyncPointResponse(b, r, keys)
	if err != nil {
		return nil, err
	}
	if s.fullLocal {
		metadata, metadataErr := decodeWorkspaceSyncLocalMetadataIntent(points[1], int(b.workspaceSyncBudget.options.MaxIntentBytes))
		if metadataErr == nil {
			if metadata.Reference.Task != ref.Task {
				return nil, nil
			}
			return nil, ErrConflict // phase dispatch must bypass regular-only slots
		}
	}
	record, err := decodeWorkspaceSyncIntent(points[1], int(b.workspaceSyncBudget.options.MaxIntentBytes))
	if err != nil {
		return nil, err
	}
	// Other tasks retain their permanent charges; no refund or prefix discovery.
	if record.Reference.Task != ref.Task {
		return nil, nil
	}
	entry, err := b.recyclingIntentEntry(record.Reference, points[1], points[0])
	if err != nil {
		return nil, err
	}
	kv := points[0]
	charge := &taskWorkspaceSyncRecoveryCharge{source: s, record: entry.Record, intentKey: intentKey, slotKey: slotKey, value: string(points[1].Value), birth: points[1].CreateRevision, generation: taskWorkspaceSyncRecyclingGeneration{kv.CreateRevision, kv.ModRevision, kv.Version, string(kv.Value)}}
	if charge.record.Reference != ref || charge.birth <= charge.record.Claim.CreateRevision || charge.birth <= s.manifest.Seed.ArchiveRevision || charge.record.Claim.CreateRevision >= c.reference.CreateRevision && charge.record.Claim != c.reference || charge.record.Claim != c.reference && charge.birth >= c.reference.CreateRevision || charge.generation.create >= charge.birth || !recyclingCanAdvance(charge.generation.version) || !recyclingCanAdvance(charge.generation.version+1) {
		return nil, ErrCorruptRecord
	}
	return charge, nil
}

func (s *taskWorkspaceSyncRecoverySource) validRecoveryBody(body *taskWorkspaceSyncRecoveryBody, charge *taskWorkspaceSyncRecoveryCharge) error {
	if body == nil || body.self != body || body.source != s || s.body != body || body.entry == nil || body.entry != s.current || charge == nil || charge.source != s || body.charge != charge {
		return ErrInvalidRecord
	}
	r, e, m := charge.record, body.entry, s.manifest
	w := s.plan.workspace
	if r.Validate() != nil || r.Version != 2 || r.Local == nil || !workspaceSyncRecoverySameTarget(*r.Local, m.Binding.Local) || r.BudgetRevision != s.authority.backend.workspaceSyncBudget.create || r.FinalSyncAttemptID != m.Seed.FinalSyncAttempt || r.CommandID != m.Seed.ArchiveCommand || r.TicketDigest != m.Seed.TicketDigest || r.ArchiveSHA256 != m.Received.SHA256 || r.PlanDigest != s.plan.digest || r.Provider != w.provider || r.StorageIdentityHash != w.storageIdentityHash || r.Bucket != w.bucket || r.Prefix != w.prefix || r.WorkspaceHash != w.hash || r.RelativePath != e.path || r.Mode != e.mode || r.Size != e.size || r.Offset != e.offset || r.BodySHA256 != e.sha256 {
		return ErrIdentityMismatch
	}
	return nil
}

func (s *taskWorkspaceSyncRecoverySource) reserveNext(ctx context.Context, body *taskWorkspaceSyncRecoveryBody) (*taskWorkspaceSyncRecoveryCharge, error) {
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	defer s.end()
	if body == nil || body.self != body || body.source != s || s.body != body || body.entry != s.current || body.charge != nil || body.bytes != 0 {
		return nil, ErrInvalidRecord
	}
	if err := s.guard(ctx); err != nil {
		return nil, err
	}
	b, c := s.authority.backend, s.authority.claim
	for slot := uint32(0); slot < b.workspaceSyncBudget.options.SlotsPerPartition; slot++ {
		charge, err := s.readCharged(ctx, slot)
		if err != nil {
			return nil, err
		}
		if charge != nil {
			if body.charge != nil {
				return nil, ErrConflict
			}
			body.charge = charge
			if err := s.validRecoveryBody(body, charge); err != nil {
				return nil, err
			}
		}
	}
	if body.charge != nil {
		return body.charge, s.guard(ctx)
	}
	for slot := uint32(0); slot < b.workspaceSyncBudget.options.SlotsPerPartition; slot++ {
		ref := TaskWorkspaceSyncIntentReference{Task: c.reference.Task, BudgetID: b.workspaceSyncBudget.options.BudgetID, IntentID: uuid.NewString(), Slot: slot}
		intentKey, slotKey, err := b.workspaceSyncRecyclingKeys(ref)
		if err != nil {
			return nil, err
		}
		keys := []string{slotKey}
		r, err := s.native(ctx, s.comparisons(nil), workspaceSyncPointOps(keys), taskProtoPoints(keys), taskProtoResponse(keys, []string{string(make([]byte, 512))}), false)
		if err != nil {
			return nil, err
		}
		points, err := workspaceSyncPointResponse(b, r, keys)
		if err != nil {
			return nil, err
		}
		slotRecord, err := decodeWorkspaceSyncRecyclingSlot(points[0])
		if err != nil || slotRecord.BudgetID != ref.BudgetID || slotRecord.Slot != slot || slotRecord.Partition != ref.Task.Partition {
			return nil, errors.Join(ErrCorruptRecord, err)
		}
		if slotRecord.State != "free" {
			continue
		}
		kv, e, m, w := points[0], body.entry, s.manifest, s.plan.workspace
		if !recyclingCanAdvance(kv.Version) || !recyclingCanAdvance(kv.Version+1) || !recyclingCanAdvance(kv.Version+2) {
			return nil, ErrConflict
		}
		local := s.target.pin
		record := TaskWorkspaceSyncIntentRecord{taskWorkspaceSyncIntentAttribution: taskWorkspaceSyncIntentAttribution{Version: 2, Reference: ref, BudgetRevision: b.workspaceSyncBudget.create, Kind: "put_object", FinalSyncAttemptID: m.Seed.FinalSyncAttempt, CommandID: m.Seed.ArchiveCommand, TicketDigest: m.Seed.TicketDigest, ArchiveSHA256: m.Received.SHA256, PlanDigest: s.plan.digest, WorkspaceHash: w.hash, Provider: w.provider, StorageIdentityHash: w.storageIdentityHash, Bucket: w.bucket, Prefix: w.prefix, RelativePath: e.path, Mode: e.mode, Size: e.size, Offset: e.offset, BodySHA256: e.sha256, Local: &local}, Claim: c.reference}
		value, err := encodeWorkspaceSyncIntent(record, int(b.workspaceSyncBudget.options.MaxIntentBytes))
		if err != nil {
			return nil, err
		}
		occupied, err := encodeWorkspaceSyncRecyclingSlot(taskWorkspaceSyncRecyclingSlotRecord{Version: 2, BudgetID: ref.BudgetID, Partition: ref.Task.Partition, Slot: slot, State: "occupied", IntentID: ref.IntentID, IntentDigest: workspaceSyncIntentDigest(value)})
		if err != nil {
			return nil, err
		}
		cmps := append(s.comparisons(nil), clientv3.Compare(clientv3.Version(intentKey), "=", 0), clientv3.Compare(clientv3.ModRevision(slotKey), "=", kv.ModRevision), clientv3.Compare(clientv3.Value(slotKey), "=", string(kv.Value)))
		// This unique reserve is attempted once. Unknown never sends it again.
		r, err = s.native(ctx, cmps, []clientv3.Op{clientv3.OpPut(intentKey, value), clientv3.OpPut(slotKey, occupied)}, []*pb.RequestOp{taskProtoPut(intentKey, value, 0), taskProtoPut(slotKey, occupied, 0)}, taskProtoPutResponse(2), false)
		if err != nil {
			return nil, err
		}
		if !operationPutResponses(r, 2) || r.Header.Revision <= kv.ModRevision || r.Header.Revision == math.MaxInt64 {
			return nil, ErrOutcomeUnknown
		}
		charge := &taskWorkspaceSyncRecoveryCharge{source: s, record: record, intentKey: intentKey, slotKey: slotKey, value: value, birth: r.Header.Revision, generation: taskWorkspaceSyncRecyclingGeneration{kv.CreateRevision, r.Header.Revision, kv.Version + 1, occupied}}
		body.charge = charge
		return charge, s.guard(ctx)
	}
	return nil, errTaskWorkspaceSyncIntentQuota
}

// Domain-separated fresh completion preserves the original immutable attempt.
// Its old outcome remains unknown; this digest names NEW guarded physical IO.
type taskWorkspaceSyncRecoveryCompletionWire struct {
	Version      uint32                            `json:"version"`
	Domain       string                            `json:"domain"`
	IntentDigest string                            `json:"intent_digest"`
	IntentBirth  int64                             `json:"intent_birth"`
	Claim        taskCloseClaimWire                `json:"claim"`
	Local        taskWorkspaceSyncLocalAttribution `json:"local"`
	Bytes        int64                             `json:"bytes"`
	BodySHA256   string                            `json:"body_sha256"`
	Dev          uint64                            `json:"dev"`
	Ino          uint64                            `json:"ino"`
}

type taskWorkspaceSyncLocalRepairCompletionWire struct {
	Version          uint32                                  `json:"version"`
	Domain           string                                  `json:"domain"`
	Regular          taskWorkspaceSyncRecoveryCompletionWire `json:"regular"`
	TranscriptDigest string                                  `json:"transcript_digest"`
}

func (w taskWorkspaceSyncLocalRepairCompletionWire) Validate() error {
	if w.Version != 1 || w.Domain != "sandbox.workspace-sync-local-full-regular-repair.v1" || w.Regular.Validate() != nil || !validHexDigest(w.TranscriptDigest) {
		return ErrInvalidRecord
	}
	return nil
}

func (w taskWorkspaceSyncRecoveryCompletionWire) Validate() error {
	if w.Version != 1 || w.Domain != "sandbox.workspace-sync-local-recovery.v1" || !validHexDigest(w.IntentDigest) || w.IntentBirth <= 0 || w.Claim.Task.Validate() != nil || !validPreparationUUID(w.Claim.ClaimID) || w.Claim.CreateRevision <= 0 || w.Local.Validate() != nil || w.Bytes < 0 || !validHexDigest(w.BodySHA256) || w.Dev != w.Local.Dev || w.Ino == 0 {
		return ErrInvalidRecord
	}
	return nil
}

func (s *taskWorkspaceSyncRecoverySource) recoveryTerminalValue(q *taskWorkspaceSyncRecoveryCompletion) (string, error) {
	c, r, p := s.authority.claim.reference, q.charge.record.Reference, q.physical
	completion := taskWorkspaceSyncRecoveryCompletionWire{1, "sandbox.workspace-sync-local-recovery.v1", workspaceSyncIntentDigest(q.charge.value), q.charge.birth, taskCloseClaimWire{c.Task, c.ClaimID, c.WorkerID, c.CreateRevision, c.LeaseID}, s.target.pin, p.bytes, p.sha256, p.dev, p.ino}
	value, err := encodeOperationMetadata(completion, 16384)
	if s.fullLocal && s.finalization != nil {
		value, err = encodeOperationMetadata(taskWorkspaceSyncLocalRepairCompletionWire{1, "sandbox.workspace-sync-local-full-regular-repair.v1", completion, hex.EncodeToString(s.finalization.transcript.Sum(nil))}, 16384)
	}
	if err != nil {
		return "", err
	}
	return encodeWorkspaceSyncRecyclingSlot(taskWorkspaceSyncRecyclingSlotRecord{Version: 2, BudgetID: r.BudgetID, Partition: r.Task.Partition, Slot: r.Slot, State: "terminal", IntentID: r.IntentID, IntentDigest: workspaceSyncIntentDigest(q.charge.value), IntentBirth: q.charge.birth, EvidenceDigest: workspaceSyncIntentDigest(value)})
}

func (s *taskWorkspaceSyncRecoverySource) validCompletion(q *taskWorkspaceSyncRecoveryCompletion) error {
	if q == nil || q.self != q || q.source != s || q.charge == nil || q.body == nil || q.body.completion != q || q.completed {
		return ErrInvalidRecord
	}
	if err := s.validRecoveryBody(q.body, q.charge); err != nil {
		return err
	}
	q.body.mu.Lock()
	closed := q.body.closed || q.body.ctx.Err() != nil || q.body.active
	q.body.mu.Unlock()
	if closed {
		return ErrGuardExpired
	}
	if !q.body.complete || q.physical.bytes != q.charge.record.Size || q.physical.sha256 != q.charge.record.BodySHA256 || q.physical.dev != s.target.pin.Dev || q.physical.ino == 0 {
		return ErrInvalidRecord
	}
	return nil
}

func (s *taskWorkspaceSyncRecoverySource) settleLocal(ctx context.Context, charge *taskWorkspaceSyncRecoveryCharge, q *taskWorkspaceSyncRecoveryCompletion) error {
	if err := s.begin(ctx); err != nil {
		return err
	}
	defer s.end()
	if err := s.validCompletion(q); err != nil || q.charge != charge {
		return errors.Join(ErrInvalidRecord, err)
	}
	if q.denied {
		return ErrConflict
	}
	if q.known {
		return s.guard(ctx)
	}
	// The current guard generation advances after a terminal ACK; the one-shot
	// attempt's predecessor must remain fixed even if that ACK's postguard fails.
	g := q.beforeTerminal
	if q.attempted {
		points, err := s.readRecoveryCompletion(ctx, charge, "")
		if err != nil {
			return err
		}
		if !immutableWorkspaceSyncKV(points[1]) || points[1].CreateRevision != charge.birth || string(points[1].Value) != charge.value || !recyclingGenerationMatches(points[0], g.create, g.version+1, g.mod, q.value) {
			return ErrOutcomeUnknown
		}
		kv := points[0]
		q.terminal = taskWorkspaceSyncRecyclingGeneration{kv.CreateRevision, kv.ModRevision, kv.Version, string(kv.Value)}
	} else {
		if err := s.guard(ctx); err != nil {
			return err
		}
		cmps := append(s.comparisons(charge), clientv3.Compare(clientv3.CreateRevision(charge.slotKey), "=", g.create), clientv3.Compare(clientv3.ModRevision(charge.slotKey), "=", g.mod), clientv3.Compare(clientv3.Value(charge.slotKey), "=", g.value))
		q.attempted = true
		r, err := s.native(ctx, cmps, []clientv3.Op{clientv3.OpPut(charge.slotKey, q.value)}, []*pb.RequestOp{taskProtoPut(charge.slotKey, q.value, 0)}, taskProtoPutResponse(1), false)
		if err != nil {
			q.denied = r != nil && !r.Succeeded
			return err
		}
		if !operationPutResponses(r, 1) || r.Header.Revision <= g.mod || r.Header.Revision == math.MaxInt64 {
			return ErrOutcomeUnknown
		}
		q.terminal = taskWorkspaceSyncRecyclingGeneration{g.create, r.Header.Revision, g.version + 1, q.value}
	}
	charge.generation = q.terminal
	if err := s.guard(ctx); err != nil {
		return err
	}
	q.known = true
	return nil
}

func (s *taskWorkspaceSyncRecoverySource) readRecoveryCompletion(ctx context.Context, charge *taskWorkspaceSyncRecoveryCharge, checkpoint string) ([]*mvccpb.KeyValue, error) {
	b := s.authority.backend
	keys := []string{charge.slotKey, charge.intentKey}
	values := []string{string(make([]byte, 512)), string(make([]byte, b.workspaceSyncBudget.options.MaxIntentBytes))}
	if checkpoint != "" {
		keys = append(keys, checkpoint)
		values = append(values, string(make([]byte, 4096)))
	}
	r, err := s.native(ctx, s.comparisons(nil), workspaceSyncPointOps(keys), taskProtoPoints(keys), taskProtoResponse(keys, values), false)
	if err != nil {
		return nil, err
	}
	return workspaceSyncPointResponse(b, r, keys)
}
