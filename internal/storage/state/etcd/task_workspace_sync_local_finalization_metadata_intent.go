package etcd

import (
	"context"
	"errors"
	"math"

	"github.com/google/uuid"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type taskWorkspaceSyncLocalMetadataIntent struct {
	Version        uint32                           `json:"version"`
	Reference      TaskWorkspaceSyncIntentReference `json:"reference"`
	Claim          taskCloseClaimWire               `json:"claim"`
	BudgetRevision int64                            `json:"budget_revision"`
	Kind           string                           `json:"kind"`
	Provider       string                           `json:"provider"`
	Binding        string                           `json:"binding"`
	ProgressBirth  int64                            `json:"progress_birth"`
}

func (r taskWorkspaceSyncLocalMetadataIntent) Validate() error {
	if r.Version != 3 || r.Reference.Validate() != nil || !validWorkspaceSyncFinalizationClaim(r.Claim, r.Reference.Task) || r.BudgetRevision <= 0 || r.Kind != "local_metadata_pass" || r.Provider != "local" || !validHexDigest(r.Binding) || r.ProgressBirth <= 0 {
		return ErrInvalidRecord
	}
	return nil
}

func decodeWorkspaceSyncLocalMetadataIntent(kv *mvccpb.KeyValue, bound int) (taskWorkspaceSyncLocalMetadataIntent, error) {
	var r taskWorkspaceSyncLocalMetadataIntent
	if !immutableWorkspaceSyncKV(kv) || decodePreparationDomain(kv, &r, bound) != nil {
		return r, ErrCorruptRecord
	}
	wire, err := encodeOperationMetadata(r, bound)
	if err != nil || wire != string(kv.Value) || kv.CreateRevision <= r.Claim.CreateRevision || kv.CreateRevision <= r.ProgressBirth {
		return r, ErrCorruptRecord
	}
	return r, nil
}

type taskWorkspaceSyncLocalMetadataCharge struct {
	record                    taskWorkspaceSyncLocalMetadataIntent
	intentKey, slotKey, value string
	birth                     int64
	generation                taskWorkspaceSyncRecyclingGeneration
}

func (f *taskWorkspaceSyncLocalFinalizer) metadataComparisons(slotBirth bool) []clientv3.Cmp {
	s, q := f.source, f.charge
	cmps := s.comparisons(nil)
	if q != nil {
		cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(q.intentKey), "=", q.birth), clientv3.Compare(clientv3.ModRevision(q.intentKey), "=", q.birth), clientv3.Compare(clientv3.Value(q.intentKey), "=", q.value))
		if slotBirth {
			cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(q.slotKey), "=", q.generation.create))
		}
		cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(q.slotKey), "=", q.generation.mod), clientv3.Compare(clientv3.Value(q.slotKey), "=", q.generation.value))
	}
	return append(cmps, f.progressComparisons()...)
}

func (f *taskWorkspaceSyncLocalFinalizer) metadataGuard(ctx context.Context) error {
	s := f.source
	if !s.fullLocal || s.effectsSealed || f.charge == nil || f.progress.Phase != "metadata" {
		return ErrGuardExpired
	}
	if err := s.physical(ctx); err != nil {
		return err
	}
	release, gateErr := f.metadataGate(ctx)
	if gateErr != nil {
		return gateErr
	}
	defer release()
	keys := []string{s.authority.backend.identityKey}
	_, err := s.native(ctx, f.metadataComparisons(false), workspaceSyncPointOps(keys), taskProtoPoints(keys), taskProtoResponse(keys, []string{s.authority.backend.identityValue}), true)
	if err != nil {
		return err
	}
	release()
	return s.physical(ctx)
}

func (f *taskWorkspaceSyncLocalFinalizer) reserveMetadata(ctx context.Context) error {
	release, err := f.metadataGate(ctx)
	if err != nil {
		return err
	}
	err = f.reserveMetadataOwned(ctx)
	release()
	if err != nil {
		return err
	}
	if f.progress.IntentID == "" {
		next := f.progress
		next.IntentID, next.IntentBirth = f.charge.record.Reference.IntentID, f.charge.birth
		if err := f.saveProgress(ctx, next); err != nil {
			return err
		}
	}
	return f.metadataGuard(ctx)
}

func (f *taskWorkspaceSyncLocalFinalizer) reserveMetadataOwned(ctx context.Context) error {
	s, b := f.source, f.source.authority.backend
	var freeKV *mvccpb.KeyValue
	var freeSlot uint32
	for slot := uint32(0); slot < b.workspaceSyncBudget.options.SlotsPerPartition; slot++ {
		ref := TaskWorkspaceSyncIntentReference{Task: s.authority.claim.reference.Task, BudgetID: b.workspaceSyncBudget.options.BudgetID, IntentID: s.manifest.Seed.ArchiveCommand, Slot: slot}
		_, key, err := b.workspaceSyncRecyclingKeys(ref)
		if err != nil {
			return err
		}
		points, err := f.pointsOwned(ctx, []string{key}, []int{512})
		if err != nil {
			return err
		}
		slotRecord, err := decodeWorkspaceSyncRecyclingSlot(points[0])
		if err != nil || slotRecord.BudgetID != ref.BudgetID || slotRecord.Slot != slot || slotRecord.Partition != ref.Task.Partition {
			return errors.Join(ErrCorruptRecord, err)
		}
		if slotRecord.State == "free" {
			if freeKV == nil {
				freeKV, freeSlot = points[0], slot
			}
			continue
		}
		ref.IntentID = slotRecord.IntentID
		intentKey, _, err := b.workspaceSyncRecyclingKeys(ref)
		if err != nil {
			return err
		}
		pair, err := f.pointsOwned(ctx, []string{key, intentKey}, []int{512, int(b.workspaceSyncBudget.options.MaxIntentBytes)})
		if err != nil {
			return err
		}
		record, err := decodeWorkspaceSyncLocalMetadataIntent(pair[1], int(b.workspaceSyncBudget.options.MaxIntentBytes))
		if err != nil {
			regular, e := decodeWorkspaceSyncIntent(pair[1], int(b.workspaceSyncBudget.options.MaxIntentBytes))
			if e != nil || regular.Reference.Task == ref.Task {
				return errors.Join(ErrConflict, e)
			}
			continue
		}
		if record.Reference.Task != ref.Task {
			continue
		}
		current, err := decodeWorkspaceSyncRecyclingSlot(pair[0])
		if err != nil || record.Reference != ref || current.IntentID != ref.IntentID || current.IntentDigest != workspaceSyncIntentDigest(string(pair[1].Value)) || current.State != "occupied" && current.State != "terminal" || current.State == "terminal" && current.IntentBirth != pair[1].CreateRevision || pair[0].CreateRevision >= pair[1].CreateRevision || pair[0].ModRevision < pair[1].CreateRevision || record.Binding != f.binding || record.ProgressBirth != f.progressKV.CreateRevision || record.BudgetRevision != b.workspaceSyncBudget.create || f.charge != nil {
			return errors.Join(ErrCorruptRecord, err)
		}
		c := s.authority.claim.reference
		if record.Claim.ClaimID != c.ClaimID && (record.Claim.CreateRevision >= c.CreateRevision || pair[1].CreateRevision >= c.CreateRevision) || record.Claim.ClaimID == c.ClaimID && record.Claim != (taskCloseClaimWire{c.Task, c.ClaimID, c.WorkerID, c.CreateRevision, c.LeaseID}) {
			return ErrConflict
		}
		f.charge = &taskWorkspaceSyncLocalMetadataCharge{record, intentKey, key, string(pair[1].Value), pair[1].CreateRevision, taskWorkspaceSyncRecyclingGeneration{pair[0].CreateRevision, pair[0].ModRevision, pair[0].Version, string(pair[0].Value)}}
	}
	if f.charge == nil {
		if freeKV == nil {
			return errTaskWorkspaceSyncIntentQuota
		}
		c := s.authority.claim.reference
		record := taskWorkspaceSyncLocalMetadataIntent{3, TaskWorkspaceSyncIntentReference{Task: c.Task, BudgetID: b.workspaceSyncBudget.options.BudgetID, IntentID: uuid.NewString(), Slot: freeSlot}, taskCloseClaimWire{c.Task, c.ClaimID, c.WorkerID, c.CreateRevision, c.LeaseID}, b.workspaceSyncBudget.create, "local_metadata_pass", "local", f.binding, f.progressKV.CreateRevision}
		value, err := encodeOperationMetadata(record, int(b.workspaceSyncBudget.options.MaxIntentBytes))
		if err != nil {
			return err
		}
		intentKey, slotKey, err := b.workspaceSyncRecyclingKeys(record.Reference)
		if err != nil || !recyclingCanAdvance(freeKV.Version+2) {
			return errors.Join(ErrConflict, err)
		}
		occupied, err := encodeWorkspaceSyncRecyclingSlot(taskWorkspaceSyncRecyclingSlotRecord{Version: 2, BudgetID: record.Reference.BudgetID, Partition: c.Task.Partition, Slot: freeSlot, State: "occupied", IntentID: record.Reference.IntentID, IntentDigest: workspaceSyncIntentDigest(value)})
		if err != nil {
			return err
		}
		cmps := append(f.metadataComparisons(false), clientv3.Compare(clientv3.Version(intentKey), "=", 0), clientv3.Compare(clientv3.ModRevision(slotKey), "=", freeKV.ModRevision), clientv3.Compare(clientv3.Value(slotKey), "=", string(freeKV.Value)))
		r, err := s.native(ctx, cmps, []clientv3.Op{clientv3.OpPut(intentKey, value), clientv3.OpPut(slotKey, occupied)}, []*pb.RequestOp{taskProtoPut(intentKey, value, 0), taskProtoPut(slotKey, occupied, 0)}, taskProtoPutResponse(2), false)
		if err != nil {
			return err
		}
		if !operationPutResponses(r, 2) || r.Header.Revision <= freeKV.ModRevision || r.Header.Revision == math.MaxInt64 {
			return ErrOutcomeUnknown
		}
		f.charge = &taskWorkspaceSyncLocalMetadataCharge{record, intentKey, slotKey, value, r.Header.Revision, taskWorkspaceSyncRecyclingGeneration{freeKV.CreateRevision, r.Header.Revision, freeKV.Version + 1, occupied}}
	}
	if f.progress.IntentID != "" && (f.progress.IntentID != f.charge.record.Reference.IntentID || f.progress.IntentBirth != f.charge.birth) {
		return ErrConflict
	}
	return nil
}

type taskWorkspaceSyncLocalMetadataEvidence struct {
	Version               uint32             `json:"version"`
	Domain                string             `json:"domain"`
	IntentDigest          string             `json:"intent_digest"`
	IntentBirth           int64              `json:"intent_birth"`
	Claim                 taskCloseClaimWire `json:"claim"`
	RootDigest            string             `json:"root_digest"`
	CheckpointRevision    int64              `json:"checkpoint_revision"`
	CheckpointDigest      string             `json:"checkpoint_digest"`
	TranscriptDigest      string             `json:"transcript_digest"`
	CreatedDirectories    uint64             `json:"created_directories"`
	RemovedEntries        uint64             `json:"removed_entries"`
	UpdatedDirectoryModes uint64             `json:"updated_directory_modes"`
	Phases                string             `json:"phases"`
}

func (e taskWorkspaceSyncLocalMetadataEvidence) Validate() error {
	if e.Version != 1 || e.Domain != "sandbox.workspace-sync-local-metadata-evidence.v1" || !validHexDigest(e.IntentDigest) || e.IntentBirth <= 0 || e.Claim.Task.Validate() != nil || !validWorkspaceSyncFinalizationClaim(e.Claim, e.Claim.Task) || !validHexDigest(e.RootDigest) || e.CheckpointRevision <= 0 || !validHexDigest(e.CheckpointDigest) || !validHexDigest(e.TranscriptDigest) || e.Phases != "directories,policy,modes" {
		return ErrInvalidRecord
	}
	return nil
}

func (f *taskWorkspaceSyncLocalFinalizer) settleMetadata(ctx context.Context, evidence string) error {
	if err := workspaceSyncLocalProgressAdvanceable(f.progressKV); err != nil {
		return err
	}
	release, gateErr := f.metadataGate(ctx)
	if gateErr != nil {
		return gateErr
	}
	defer func() { release() }()
	s, q := f.source, f.charge
	ref, g := q.record.Reference, q.generation
	terminal, err := encodeWorkspaceSyncRecyclingSlot(taskWorkspaceSyncRecyclingSlotRecord{Version: 2, BudgetID: ref.BudgetID, Partition: ref.Task.Partition, Slot: ref.Slot, State: "terminal", IntentID: ref.IntentID, IntentDigest: workspaceSyncIntentDigest(q.value), IntentBirth: q.birth, EvidenceDigest: evidence})
	if err != nil || !recyclingCanAdvance(g.version+1) {
		return errors.Join(ErrConflict, err)
	}
	r, err := s.native(ctx, f.metadataComparisons(true), []clientv3.Op{clientv3.OpPut(q.slotKey, terminal)}, []*pb.RequestOp{taskProtoPut(q.slotKey, terminal, 0)}, taskProtoPutResponse(1), true)
	if err != nil {
		return err
	}
	if !operationPutResponses(r, 1) || r.Header.Revision <= g.mod || r.Header.Revision == math.MaxInt64 {
		return ErrOutcomeUnknown
	}
	q.generation = taskWorkspaceSyncRecyclingGeneration{g.create, r.Header.Revision, g.version + 1, terminal}
	release()
	if err := f.metadataGuard(ctx); err != nil {
		return err
	}
	nextRelease, gateErr := f.metadataGate(ctx)
	if gateErr != nil {
		return gateErr
	}
	release = nextRelease
	next := f.progress
	next.Phase, next.EvidenceDigest = "effects-done", evidence
	value, err := encodeWorkspaceSyncLocalProgress(next)
	if err != nil {
		return err
	}
	free, err := encodeWorkspaceSyncRecyclingSlot(taskWorkspaceSyncRecyclingSlotRecord{Version: 2, BudgetID: ref.BudgetID, Partition: ref.Task.Partition, Slot: ref.Slot, State: "free"})
	if err != nil {
		return err
	}
	writes := []Write{{Key: q.slotKey, Value: []byte(free)}, {Key: q.intentKey, Delete: true}, {Key: f.progressKey, Value: []byte(value)}}
	r, err = s.native(ctx, f.metadataComparisons(false), []clientv3.Op{clientv3.OpPut(q.slotKey, free), clientv3.OpDelete(q.intentKey), clientv3.OpPut(f.progressKey, value)}, taskProtoWrites(writes), recyclingGCReply(), true)
	if err != nil {
		return err
	}
	if r.Header.Revision <= q.generation.mod || r.Header.Revision <= f.progressKV.ModRevision || r.Header.Revision == math.MaxInt64 || len(r.Responses) != 3 || r.Responses[0] == nil || r.Responses[1] == nil || r.Responses[2] == nil || r.Responses[0].GetResponsePut() == nil || r.Responses[1].GetResponseDeleteRange() == nil || r.Responses[1].GetResponseDeleteRange().Deleted != 1 || r.Responses[2].GetResponsePut() == nil {
		return ErrOutcomeUnknown
	}
	f.progress, f.progressKV = next, &mvccpb.KeyValue{Key: []byte(f.progressKey), Value: []byte(value), CreateRevision: f.progressKV.CreateRevision, ModRevision: r.Header.Revision, Version: f.progressKV.Version + 1}
	f.charge = nil
	release()
	return s.guard(ctx)
}

// Preflight maximum retained record/wire encodings before progress or physical
// effects. These are transient metadata clones, released before traversal.
func (f *taskWorkspaceSyncLocalFinalizer) preflight() error {
	s, b := f.source, f.source.authority.backend
	var targetDigest string
	if f.binding != "" {
		targetDigest = s.manifest.Binding.TargetDigest
	}

	projections, err := b.taskLocalFinalizationWireProjections(s.comparisons(nil), s.authority.claim.reference, s.manifest.Seed.TaskBirth, f.binding, targetDigest, s.manifest.Seed.ArchiveCommand, f.progressKey, f.completeKey, s.authority.claim.checkpointKey)
	if err != nil {
		return err
	}
	return checkTaskWireProjections(b.namespace, projections)
}

// The same prospective codecs and complete native envelopes are shared by the
// original finalizer and cleanup admission. Plain comparison data does not
// reconstruct a source, a metadata gate or filesystem completion evidence.
func (b *Backend) taskLocalFinalizationWireProjections(base []clientv3.Cmp, c TaskClaimReference, birth int64, binding, targetDigest, command, progressKey, completeKey, checkpointKey string) ([]cleanupWireProjection, error) {
	ref := TaskWorkspaceSyncIntentReference{Task: c.Task, BudgetID: b.workspaceSyncBudget.options.BudgetID, IntentID: command, Slot: 63}
	writer := taskCloseClaimWire{c.Task, c.ClaimID, c.WorkerID, c.CreateRevision, c.LeaseID}
	// Validate the prospective schemas with longest revision encodings before
	// progress or regular repair, not just a protobuf byte-count placeholder.
	if binding != "" {
		intent := taskWorkspaceSyncLocalMetadataIntent{3, ref, writer, b.workspaceSyncBudget.create, "local_metadata_pass", "local", binding, math.MaxInt64 - 1}
		if _, err := encodeOperationMetadata(intent, int(b.workspaceSyncBudget.options.MaxIntentBytes)); err != nil {
			return nil, err
		}
		projection := taskWorkspaceSyncLocalProgress{Version: 1, Task: c.Task, TaskBirth: birth, Binding: binding, Claim: writer, Phase: "effects-done", CheckpointRevision: math.MaxInt64 - 1, CheckpointDigest: binding, IntentID: ref.IntentID, IntentBirth: math.MaxInt64 - 1, EvidenceDigest: binding}
		if _, err := encodeWorkspaceSyncLocalProgress(projection); err != nil {
			return nil, err
		}
		complete := taskWorkspaceSyncLocalComplete{1, "sandbox.workspace-sync-local-complete.v1", c.Task, birth, binding, writer, binding, binding, math.MaxInt64 - 1, binding, targetDigest}
		if _, err := encodeOperationMetadata(complete, 8192); err != nil {
			return nil, err
		}
	}
	intentKey, slotKey, err := b.workspaceSyncRecyclingKeys(ref)
	if err != nil {
		return nil, err
	}
	progress := string(make([]byte, 16384))
	intent := string(make([]byte, b.workspaceSyncBudget.options.MaxIntentBytes))
	cmps := append(append([]clientv3.Cmp(nil), base...), clientv3.Compare(clientv3.CreateRevision(intentKey), "=", math.MaxInt64-1), clientv3.Compare(clientv3.ModRevision(intentKey), "=", math.MaxInt64-1), clientv3.Compare(clientv3.Value(intentKey), "=", intent), clientv3.Compare(clientv3.ModRevision(slotKey), "=", math.MaxInt64-1), clientv3.Compare(clientv3.Value(slotKey), "=", string(make([]byte, 512))), clientv3.Compare(clientv3.ModRevision(progressKey), "=", math.MaxInt64-1), clientv3.Compare(clientv3.Value(progressKey), "=", progress))
	writes := []Write{{Key: slotKey, Value: []byte(make([]byte, 512))}, {Key: intentKey, Delete: true}, {Key: progressKey, Value: []byte(progress)}}
	settle := workspaceSyncNativeProjection(b, cmps, taskProtoWrites(writes), recyclingGCReply(), true)
	settle.name = "metadata-settle"
	complete := string(make([]byte, 8192))
	cmps = append(append([]clientv3.Cmp(nil), base...), clientv3.Compare(clientv3.ModRevision(progressKey), "=", math.MaxInt64-1), clientv3.Compare(clientv3.Value(progressKey), "=", progress), clientv3.Compare(clientv3.ModRevision(checkpointKey), "=", math.MaxInt64-1), clientv3.Compare(clientv3.Value(checkpointKey), "=", string(make([]byte, 4096))), clientv3.Compare(clientv3.Version(completeKey), "=", 0))
	publish := workspaceSyncNativeProjection(b, cmps, []*pb.RequestOp{taskProtoPut(progressKey, progress, 0), taskProtoPut(completeKey, complete, 0)}, taskProtoPutResponse(2), false)
	publish.name = "completion-publish"
	return []cleanupWireProjection{settle, publish}, nil
}
