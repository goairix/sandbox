package etcd

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This is fixed writer-derived authority, not an extensible mutation program.
// It owns copies of all independently authenticated native values. Comparisons
// retain the accepted 50 pins and add only the transition-specific fixed pins.
type taskLocalRuntimeAuthority struct {
	backend                                 *Backend
	claim                                   *TaskClaim
	task                                    TaskRecord
	manifest                                taskArchiveRecoveryManifest
	command                                 taskArchiveCommandRecord
	publication                             *RuntimeEnrolledPublicationEntry
	comparisons                             []clientv3.Cmp
	prefix                                  string
	complete                                taskFence
	enrollment                              [2]taskFence
	stamps                                  p.TerminationAuthorityBindings
	terminationKey, providerKey, releaseKey string
}

func (a *taskLocalRuntimeAuthority) base() []clientv3.Cmp {
	cmps := append([]clientv3.Cmp(nil), a.comparisons...)
	cmps = append(cmps, clientv3.Compare(clientv3.Version(a.prefix), "=", 0).WithRange(clientv3.GetPrefixRangeEnd(a.prefix)))
	return append(cmps, a.complete.comparisons()...)
}

func (a *taskLocalRuntimeAuthority) providerComparisons(provider, termination *mvccpb.KeyValue) []clientv3.Cmp {
	cmps := a.base()
	if termination != nil {
		cmps = append(cmps, ownTaskFence(termination).comparisons()...)
		return append(cmps, clientv3.Compare(clientv3.ModRevision(a.providerKey), "=", provider.ModRevision))
	}
	if provider == nil {
		return append(cmps, clientv3.Compare(clientv3.Version(a.providerKey), "=", 0))
	}
	return append(cmps, ownTaskFence(provider).comparisons()...)
}

func (a *taskLocalRuntimeAuthority) terminationReserveComparisons(provider *mvccpb.KeyValue) []clientv3.Cmp {
	cmps := a.base()
	for _, e := range a.enrollment {
		cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(e.key), "=", e.mod))
	}
	cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(a.providerKey), "=", provider.ModRevision))
	return append(cmps, clientv3.Compare(clientv3.Version(a.terminationKey), "=", 0))
}

func (a *taskLocalRuntimeAuthority) terminationComparisons(termination, provider *mvccpb.KeyValue) []clientv3.Cmp {
	cmps := append(a.base(), ownTaskFence(termination).comparisons()...)
	if provider != nil {
		cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(a.providerKey), "=", provider.ModRevision))
	}
	return cmps
}

func (a *taskLocalRuntimeAuthority) preflightWrite(cmps []clientv3.Cmp, key, value string) error {
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoWrites([]Write{{Key: key, Value: []byte(value)}})}
	success := taskProtoPutResponse(1)
	return taskCheckWire(a.backend.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{success, taskProtoResponse(nil, nil)})
}

// This runs before the retained finalizer or any other provider mutation.
// A future proof uses its entire admitted byte bound, and all native scalars
// use their maximum wire width. It never treats these projection bytes as proof.
func (a *taskLocalRuntimeAuthority) preflightTerminationPhases(r p.TaskRuntimeTerminationIntent, provider *mvccpb.KeyValue) error {
	if r.Phase != "reserved" || provider == nil {
		return ErrInvalidRecord
	}
	reserved, err := p.EncodeTaskRuntimeTerminationIntent(r)
	if err != nil {
		return err
	}
	previous := &mvccpb.KeyValue{Key: []byte(a.terminationKey), Value: reserved, CreateRevision: math.MaxInt64, ModRevision: math.MaxInt64, Version: 1}
	r.Phase, r.TerminalProof = "terminated", make([]byte, 4096)
	for _, phase := range []string{"terminated", "cleanup-complete"} {
		r.Phase = phase
		if phase == "cleanup-complete" {
			r.CleanupDigest = string(bytes.Repeat([]byte{'f'}, 64))
		}
		wire, err := p.EncodeTaskRuntimeTerminationIntent(r)
		if err != nil {
			return err
		}
		if err := a.preflightWrite(a.terminationComparisons(previous, provider), a.terminationKey, string(wire)); err != nil {
			return err
		}
		previous.Value = wire
	}
	return nil
}

func (a *taskLocalRuntimeAuthority) local(ctx context.Context) error {
	if a == nil || !a.backend.validTaskClaim(a.claim) || ctx == nil {
		return ErrInvalidRecord
	}
	if err := errors.Join(ctx.Err(), a.claim.parentCtx.Err()); err != nil {
		return err
	}
	now := time.Now()
	a.claim.mu.Lock()
	valid := !a.claim.lost && now.Before(a.claim.deadline)
	a.claim.mu.Unlock()
	if !valid {
		return ErrGuardExpired
	}
	return nil
}

// Caller selects only a private fixed transition's comparisons. Neither wire
// data nor a public API can supply a comparator or choose an action here.
func (a *taskLocalRuntimeAuthority) guard(ctx context.Context, cmps []clientv3.Cmp) error {
	if err := a.local(ctx); err != nil {
		return err
	}
	b := a.backend
	keys := []string{b.identityKey}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoPoints(keys)}
	if err := taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{taskProtoResponse(keys, []string{b.identityValue}), taskProtoResponse(nil, nil)}); err != nil {
		return err
	}
	bounded, cancel := b.requestContext(ctx)
	r, err := b.client.Txn(bounded).If(cmps...).Then(clientv3.OpGet(b.identityKey)).Commit()
	err = errors.Join(err, bounded.Err())
	cancel()
	if err != nil {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	if err := b.operationResponseHeader(r); err != nil {
		return err
	}
	if err := taskCheckWire(b.namespace, nil, []*pb.TxnResponse{(*pb.TxnResponse)(r)}); err != nil {
		return err
	}
	if !r.Succeeded {
		return ErrConflict
	}
	if _, err := workspaceSyncPointResponse(b, r, keys); err != nil {
		return err
	}
	return a.local(ctx)
}

func localRuntimeStamp(kv *mvccpb.KeyValue) p.RecordStamp {
	return p.RecordStamp{CreateRevision: kv.CreateRevision, ModRevision: kv.ModRevision, SHA256: workspaceSyncIntentDigest(string(kv.Value))}
}

func localRuntimeFenceStamp(f taskFence) p.RecordStamp {
	return p.RecordStamp{CreateRevision: f.create, ModRevision: f.mod, SHA256: workspaceSyncIntentDigest(f.value)}
}

// A sealed completion is independently matched against its original archive,
// immutable manifest, checkpoint and progress from one point-read snapshot.
func validateLocalRuntimeCompletion(m taskArchiveRecoveryManifest, binding string, progressKV, completeKV, checkpointKV *mvccpb.KeyValue) (taskWorkspaceSyncLocalComplete, error) {
	complete, err := decodeWorkspaceSyncLocalComplete(completeKV)
	if err != nil {
		return complete, err
	}
	progress, err := decodeWorkspaceSyncLocalProgress(progressKV)
	if err != nil || m.validate() != nil || m.State != "bound" || progress.Phase != "sealed" || progress.Task != m.Seed.Task || progress.TaskBirth != m.Seed.TaskBirth || progress.Binding != binding || complete.Task != progress.Task || complete.TaskBirth != progress.TaskBirth || complete.Binding != binding || complete.Claim != progress.Claim || complete.Claim.CreateRevision <= m.Seed.ArchiveRevision || completeKV.CreateRevision <= complete.Claim.CreateRevision || progressKV.ModRevision != completeKV.CreateRevision || complete.ProgressDigest != workspaceSyncIntentDigest(string(progressKV.Value)) || complete.RootDigest != m.Binding.TargetDigest || complete.EvidenceDigest != progress.EvidenceDigest || complete.CheckpointRevision != progress.CheckpointRevision || complete.CheckpointDigest != progress.CheckpointDigest || checkpointKV == nil || checkpointKV.ModRevision != complete.CheckpointRevision || workspaceSyncIntentDigest(string(checkpointKV.Value)) != complete.CheckpointDigest {
		return complete, errors.Join(ErrCorruptRecord, err)
	}
	var checkpoint TaskCheckpointRecord
	if decodeTaskCheckpointRecord(checkpointKV, &checkpoint) != nil || checkpoint.Reference != m.Seed.Task || checkpoint.State != TaskCheckpointPending || checkpointKV.CreateRevision != m.Seed.TaskBirth {
		return complete, ErrCorruptRecord
	}
	wire, err := encodeTaskCheckpointRecord(checkpoint)
	if err != nil || wire != string(checkpointKV.Value) {
		return complete, ErrCorruptRecord
	}
	if checkpoint.Version == 2 {
		expected := TaskWorkspaceSyncCheckpoint{Version: 1, FinalSyncAttemptID: m.Seed.FinalSyncAttempt, PlanDigest: m.Binding.Plan.Digest, ArchiveSHA256: m.Received.SHA256, ArchiveBytes: m.Received.Bytes, ArchiveEntries: m.Received.Entries, TicketDigest: m.Seed.TicketDigest, TargetDigest: m.Binding.TargetDigest}
		if checkpoint.WorkspaceSync == nil || !workspaceSyncCheckpointSameContext(*checkpoint.WorkspaceSync, expected) {
			return complete, ErrCorruptRecord
		}
	} else if checkpoint.Version != 1 || checkpoint.ClaimID != "" || checkpoint.DetailDigest != "" || checkpointKV.ModRevision != m.Seed.TaskBirth {
		return complete, ErrCorruptRecord
	}
	return complete, nil
}

func (b *Backend) loadLocalRuntimeManifest(ctx context.Context, task TaskReference) (_ taskArchiveRecoveryManifest, binding string, resultErr error) {
	store := b.archiveStore
	if store == nil || store.recovery == nil || b.workspaceSyncBudget == nil || b.workspaceSyncBudget.profile != 2 {
		return taskArchiveRecoveryManifest{}, "", ErrInvalidConfiguration
	}
	var taskID [36]byte
	copy(taskID[:], task.TaskID)
	// Bounded private slots already charged by archive recovery. This performs
	// no filesystem inventory, key-prefix scan or runtime RPC.
	slot := -1
	store.mu.Lock()
	for i, s := range store.recovery.slots {
		if s.diagnostic.Occupied && s.diagnostic.TaskID == taskID {
			if slot >= 0 {
				store.mu.Unlock()
				return taskArchiveRecoveryManifest{}, "", ErrConflict
			}
			slot = i
		}
	}
	store.mu.Unlock()
	if slot < 0 {
		return taskArchiveRecoveryManifest{}, "", ErrConflict
	}
	s, err := store.registerRecovery(ctx, uint32(slot))
	if err != nil {
		return taskArchiveRecoveryManifest{}, "", err
	}
	defer func() { resultErr = errors.Join(resultErr, s.Close()) }()
	if err := s.begin(ctx); err != nil {
		return taskArchiveRecoveryManifest{}, "", err
	}
	err = s.load(ctx)
	s.end()
	if err != nil || s.manifest.Seed.Task != task || s.plan.workspace.provider != "local" {
		return taskArchiveRecoveryManifest{}, "", errors.Join(ErrIdentityMismatch, err)
	}
	// Use the same fixed canonical encoding as the sealed completion. Scratch
	// is bounded and released before provider IO; no target writer is opened.
	var buffer [taskArchiveRecoveryMetadataBytes]byte
	wire, err := encodeTaskArchiveRecovery(s.manifest, buffer[:])
	if err != nil {
		return taskArchiveRecoveryManifest{}, "", err
	}
	d := framedDigest("sandbox.workspace-sync-local-binding.v1", workspaceSyncIntentDigest(string(wire)), b.workspaceSyncBudget.value, strconv.FormatInt(b.workspaceSyncBudget.create, 10))
	return s.manifest, hex.EncodeToString(d[:]), nil
}

func (b *Backend) loadLocalRuntimeAuthority(ctx context.Context, c *TaskClaim) (*taskLocalRuntimeAuthority, error) {
	if !b.validTaskClaim(c) || b.enrolledPublication == nil || b.enrolledVerifier == nil || ctx == nil {
		return nil, ErrInvalidRecord
	}
	a := &taskLocalRuntimeAuthority{backend: b, claim: c}
	if err := a.local(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	fences := append([]taskFence(nil), c.fences...)
	cmps := c.comparisons()
	c.mu.Unlock()
	if len(fences) != 8 || len(cmps) != 40 {
		return nil, ErrCorruptRecord
	}
	keys := make([]string, 8)
	for i, f := range fences {
		keys[i] = f.key
	}
	values, err := b.readEnrolledPoints(ctx, keys, []int{4096, 4096, 2048, 4096, 4096, 4096, 4096, 4096})
	if err != nil {
		return nil, err
	}
	var task TaskRecord
	var cleanup CleanupIntentRecord
	var link TaskLinkRecord
	var placement SandboxPlacementRecord
	var control SandboxControlRecord
	var owner WorkspaceOwnerRecord
	var fence WorkspaceFenceRecord
	var index RuntimeIndexRecord
	records := []interface{ Validate() error }{&task, &cleanup, &link, &placement, &control, &owner, &fence, &index}
	for i, kv := range values {
		if decodeLocalRuntimeDomain(fences[i], kv, records[i]) != nil {
			return nil, fmt.Errorf("local release domain-record[%d]: %w", i, ErrCorruptRecord)
		}
	}
	if task.Reference != c.reference.Task || task.Runtime.Validate() != nil || task.CreationIntentID != index.IntentID || task.Generation != index.Generation || task.Runtime != index.Runtime || cleanup.Task != task || link.Reference != task.Reference || control.Phase != PhaseDestroying || control.MountAttempt != 0 || owner.MountAttempt != 0 || control.Runtime == nil || owner.Runtime == nil || *control.Runtime != task.Runtime || *owner.Runtime != task.Runtime || values[0].CreateRevision != c.birth || values[4].ModRevision != c.birth {
		return nil, ErrIdentityMismatch
	}
	checkedPlacement, err := b.decodePlacement(values[3], task.Reference.SandboxID)
	if err != nil {
		return nil, err
	}
	checkedControl, err := b.decodeControl(values[4], task.Reference.Partition, task.Reference.SandboxID, checkedPlacement)
	if err != nil || checkedControl == nil || !reflect.DeepEqual(*checkedControl, control) || control.WorkspaceHash != task.WorkspaceHash || control.IntentID != task.CreationIntentID || control.Generation != task.Generation || control.DataGateEpoch != task.DataGateEpoch || control.Snapshot != task.Snapshot || !control.ExpiresAt.Equal(task.ExpiresAt) || task.ControlRevision >= c.birth || values[4].CreateRevision > task.ControlRevision || values[1].CreateRevision != c.birth || values[2].CreateRevision != c.birth {
		return nil, fmt.Errorf("local release control-binding: %w", errors.Join(ErrCorruptRecord, err))
	}
	if !enrolledImmutableKV(values[7]) || owner.RestoreEpoch != task.Reference.RestoreEpoch || fence.RestoreEpoch != task.Reference.RestoreEpoch || index.RestoreEpoch != task.Reference.RestoreEpoch || owner.WorkspaceHash != task.WorkspaceHash || owner.SandboxID != task.Reference.SandboxID || owner.IntentID != task.CreationIntentID || owner.Generation != task.Generation || fence.WorkspaceHash != task.WorkspaceHash || fence.Generation != task.Generation || index.WorkspaceHash != task.WorkspaceHash || index.SandboxID != task.Reference.SandboxID {
		return nil, fmt.Errorf("local release owner-index-binding: %w", ErrCorruptRecord)
	}
	a.task = task
	var binding string
	a.manifest, binding, err = b.loadLocalRuntimeManifest(ctx, task.Reference)
	if err != nil {
		return nil, fmt.Errorf("local release manifest: %w", err)
	}
	m := a.manifest
	progressKey, completeKey, err := b.namespace.workspaceSyncLocalKeys(task.Reference)
	if err != nil {
		return nil, err
	}
	points, err := b.readEnrolledPoints(ctx, []string{progressKey, completeKey, c.checkpointKey}, []int{16384, 8192, 4096})
	if err != nil {
		return nil, err
	}
	if _, err := validateLocalRuntimeCompletion(m, binding, points[0], points[1], points[2]); err != nil {
		return nil, fmt.Errorf("local release completion: %w", err)
	}
	a.complete = ownTaskFence(points[1])
	plan, err := m.Binding.Plan.plan()
	if err != nil || plan.workspace.Hash() != task.WorkspaceHash {
		return nil, ErrIdentityMismatch
	}
	a.publication, err = b.loadRuntimePublicationEnrolled(ctx, plan.workspace, task.CreationIntentID)
	if err != nil || a.publication == nil || a.publication.Record.Runtime != task.Runtime || a.publication.Record.Generation != task.Generation || a.publication.Record.SandboxID != task.Reference.SandboxID || a.publication.Record.DataGateEpoch != task.DataGateEpoch {
		return nil, errors.Join(ErrIdentityMismatch, err)
	}
	ek, ak, err := b.namespace.runtimeEnrollmentKeys(task.Reference.Partition, task.CreationIntentID)
	if err != nil {
		return nil, err
	}
	publicationKey, _, err := b.namespace.runtimePublicationKeys(task.Reference.Partition, task.CreationIntentID)
	if err != nil {
		return nil, err
	}
	enrolled, err := b.readEnrolledPoints(ctx, []string{ek, ak, publicationKey}, []int{8192, 8192, 8192})
	if err != nil {
		return nil, err
	}
	var er RuntimeEnrollmentRecord
	var ar RuntimeEnrollmentActivationRecord
	var publication RuntimeEnrolledPublicationRecord
	if !enrolledImmutableKV(enrolled[0]) || !enrolledImmutableKV(enrolled[1]) || !enrolledImmutableKV(enrolled[2]) || enrolled[0].CreateRevision != enrolled[1].CreateRevision || enrolled[0].CreateRevision != enrolled[2].CreateRevision || decodeEnrolledRecord(enrolled[2].Value, &publication) != nil || publication != a.publication.Record || decodeEnrolledRecord(enrolled[0].Value, &er) != nil || decodeEnrolledRecord(enrolled[1].Value, &ar) != nil || workspaceSyncIntentDigest(string(enrolled[0].Value)) != a.publication.Record.EnrollmentRecordDigest || workspaceSyncIntentDigest(string(enrolled[1].Value)) != a.publication.Record.ActivationRecordDigest {
		return nil, fmt.Errorf("local release enrollment: %w", ErrCorruptRecord)
	}
	a.enrollment = [2]taskFence{ownTaskFence(enrolled[0]), ownTaskFence(enrolled[1])}
	if err := a.loadHistory(ctx, fences); err != nil {
		return nil, fmt.Errorf("local release history: %w", err)
	}
	a.comparisons = append(b.baseComparisons(), cmps...)
	historyKeys, err := a.stampsKeys()
	if err != nil {
		return nil, err
	}
	for _, s := range []struct {
		key   string
		stamp p.RecordStamp
	}{
		{historyKeys.Quiescence, a.stamps.Quiescence}, {historyKeys.QuiescenceReceipt, a.stamps.QuiescenceReceipt}, {historyKeys.QuiescenceReservation, a.stamps.QuiescenceReservation},
		{historyKeys.FinalSync, a.stamps.FinalSync}, {historyKeys.Issuer, a.stamps.Issuer}, {historyKeys.Archive, a.stamps.Archive},
	} {
		a.comparisons = append(a.comparisons, clientv3.Compare(clientv3.ModRevision(s.key), "=", s.stamp.ModRevision))
	}
	if len(a.comparisons) != 50 {
		return nil, ErrInvalidRecord
	}
	a.prefix, err = b.taskOperationsPrefix(task.Reference)
	if err != nil {
		return nil, err
	}
	a.terminationKey, a.providerKey, a.releaseKey, err = b.namespace.localRuntimeReleaseKeys(task.Reference)
	if err != nil {
		return nil, err
	}
	return a, a.guard(ctx, a.base())
}

// Each retained domain keeps its original typed decoder. The exact canonical
// comparison also rejects duplicate, unknown, omitted and null fields without
// applying a preparation-only reflection schema to optional runtime pointers.
func decodeLocalRuntimeDomain(f taskFence, kv *mvccpb.KeyValue, record interface{ Validate() error }) error {
	if !f.matches(kv) || !operationPermanentKV(kv) || kv.Version <= 0 {
		return ErrCorruptRecord
	}
	var err error
	var canonical string
	switch destination := record.(type) {
	case *TaskRecord:
		err = decodeTaskRecord(kv, destination)
		if err == nil {
			canonical, err = encodeTaskRecord(*destination)
		}
	case *CleanupIntentRecord:
		err = decodeCleanupIntentRecord(kv, destination)
		if err == nil {
			canonical, err = encodeCleanupIntentRecord(*destination)
		}
	case *TaskLinkRecord:
		err = decodeTaskLinkRecord(kv, destination)
		if err == nil {
			canonical, err = encodeTaskLinkRecord(*destination)
		}
	case *SandboxPlacementRecord, *SandboxControlRecord, *WorkspaceOwnerRecord, *WorkspaceFenceRecord, *RuntimeIndexRecord:
		err = decodeDomainRecord(kv, record)
		if err == nil {
			canonical, err = encodeDomainRecord(record)
		}
	default:
		return ErrCorruptRecord
	}
	if err != nil {
		return err
	}
	if canonical != string(kv.Value) {
		return ErrCorruptRecord
	}
	return nil
}

func (a *taskLocalRuntimeAuthority) stampsKeys() (p.TaskRuntimeTerminationKeys, error) {
	c := a.command.Claims.Context.Current
	r := p.TaskRuntimeTerminationIntent{Version: 1, Phase: "reserved", Current: c, Partition: a.task.Reference.Partition, TaskBirth: a.claim.birth, CreationIntentID: a.task.CreationIntentID, EnrollmentDigest: a.publication.Record.EnrollmentDigest, ObserverCertificateDigest: a.publication.Record.ObserverCertificateDigest, NodeUID: a.publication.Record.NodeUID, NodeBootID: a.publication.Record.NodeBootID, Action: "kill_pid1_and_observe_cgroup_empty", Authority: a.stamps}
	return p.TaskRuntimeTerminationKeysFor(r)
}

func (a *taskLocalRuntimeAuthority) loadHistory(ctx context.Context, fences []taskFence) error {
	b, task, m := a.backend, a.task.Reference, a.manifest
	archiveKey, _ := b.namespace.taskWorkspaceArchiveKey(task)
	finalKey, _ := b.namespace.taskFinalSyncAttemptKey(task)
	points, err := b.readEnrolledPoints(ctx, []string{archiveKey, finalKey}, []int{maxRecordBytes, 8192})
	if err != nil {
		return err
	}
	var command taskArchiveCommandRecord
	var final TaskFinalSyncAttemptRecord
	if decodeTaskArchiveCommand(points[0], &command) != nil || decodeTaskFinalSyncAttempt(points[1], &final) != nil || command.FinalSync.record() != final || final.Task != a.task || final.DestroyingRevision != a.claim.birth || points[0].CreateRevision != m.Seed.ArchiveRevision || points[1].CreateRevision != m.Seed.FinalSyncRevision || workspaceSyncIntentDigest(string(points[0].Value)) != m.Seed.ArchiveDigest || workspaceSyncIntentDigest(string(points[1].Value)) != m.Seed.FinalSyncDigest || final.AttemptID != m.Seed.FinalSyncAttempt || final.AttemptID != m.Seed.ArchiveCommand || command.IssuerRevision != m.Seed.IssuerRevision || workspaceSyncIntentDigest(string(command.Ticket)) != m.Seed.TicketDigest {
		return ErrCorruptRecord
	}
	oldClaim := TaskClaimReference{Task: task, ClaimID: m.Seed.ClaimID, WorkerID: m.Seed.WorkerID, LeaseID: m.Seed.LeaseID, CreateRevision: m.Seed.ClaimRevision}
	claimWire, err := encodeTaskClaimRecord(taskClaimRecord{Version: 1, Task: task, ClaimID: oldClaim.ClaimID, WorkerID: oldClaim.WorkerID, LeaseID: oldClaim.LeaseID})
	plan, planErr := m.Binding.Plan.plan()
	if err != nil || planErr != nil || plan.MatchesAttempt(final) != nil || final.Claim != oldClaim || workspaceSyncIntentDigest(claimWire) != m.Seed.ClaimDigest || localRuntimeFenceStamp(fences[0]).SHA256 != m.Seed.TaskDigest || a.claim.reference.CreateRevision < final.Claim.CreateRevision {
		return ErrCorruptRecord
	}
	q, err := b.loadTaskQuiescence(ctx, task, nil)
	if err != nil || q == nil || q.Record == nil || q.reservation == nil || q.Reference != final.Quiescence || q.Revision != final.QuiescenceRevision || q.Record.Task != a.task || q.Record.Context != command.Claims.Context.QuiescenceContext {
		return errors.Join(ErrCorruptRecord, err)
	}
	qk, _ := b.namespace.taskQuiescenceKey(task)
	_, qr, _ := b.stageKeys(q.Reference.Stage)
	qres, _ := b.namespace.taskQuiescenceAttemptKey(task)
	ik, _ := b.namespace.commandIssuerKey(command.Claims.Context.Current.IssuerCertificateID)
	history, err := b.readEnrolledPoints(ctx, []string{qk, qr, qres, ik}, []int{32768, 4096, 8192, 8192})
	if err != nil {
		return err
	}
	qwire, err := encodeTaskQuiescenceRecord(*q.Record)
	if err != nil || !enrolledImmutableKV(history[0]) || !enrolledImmutableKV(history[1]) || !enrolledImmutableKV(history[2]) || string(history[0].Value) != qwire || history[0].CreateRevision != q.Revision || history[1].CreateRevision != q.Revision || history[2].CreateRevision != q.reservation.Revision {
		return ErrCorruptRecord
	}
	if _, err := quiescenceCommittedReceipt(history[1], qr, q.Revision, q.Record.Attempt); err != nil {
		return err
	}
	var reservation TaskQuiescenceAttemptRecord
	if decodeTaskQuiescenceAttempt(history[2], &reservation) != nil || q.reservation.Record == nil || reservation != *q.reservation.Record {
		return ErrCorruptRecord
	}
	reservationWire, err := encodeTaskQuiescenceAttempt(reservation)
	if err != nil || reservationWire != string(history[2].Value) {
		return ErrCorruptRecord
	}
	issuer, err := b.decodeCommandIssuer(history[3])
	if err != nil || issuer == nil || issuer.Revision != m.Seed.IssuerRevision || workspaceSyncIntentDigest(string(history[3].Value)) != m.Seed.IssuerDigest || !bytes.Equal(issuer.Record.Certificate, command.Certificate) {
		return errors.Join(ErrCorruptRecord, err)
	}
	// Historical signature validation is deliberately confined to the archived
	// signed interval. It grants no fresh ticket or old runtime RPC capability.
	archiveAt := command.Claims.NotBefore.Add(command.Claims.NotAfter.Sub(command.Claims.NotBefore) / 2)
	if _, err := b.enrolledVerifier.VerifyTaskWorkspaceArchiveTicket(command.Ticket, command.Certificate, command.Claims.Context, archiveAt); err != nil {
		return err
	}
	var receipt taskArchiveQuiescenceReceiptWire
	if json.Unmarshal(command.QuiescenceReceipt, &receipt) != nil {
		return ErrCorruptRecord
	}
	rc := receipt.Claims
	receiptAt := rc.NotBefore.Add(rc.NotAfter.Sub(rc.NotBefore) / 2)
	if _, err := b.enrolledVerifier.VerifyTaskUserQuiescenceReceipt(command.QuiescenceReceipt, a.publication.Enrollment.Request.RuntimeCertificate, q.Record.Context, q.Record.TicketDigest, rc.NotBefore, rc.NotAfter, receiptAt); err != nil {
		return err
	}
	a.command = command
	a.stamps = p.TerminationAuthorityBindings{Task: localRuntimeFenceStamp(fences[0]), CleanupIntent: localRuntimeFenceStamp(fences[1]), TaskLink: localRuntimeFenceStamp(fences[2]), Placement: localRuntimeFenceStamp(fences[3]), Control: localRuntimeFenceStamp(fences[4]), Owner: localRuntimeFenceStamp(fences[5]), Fence: localRuntimeFenceStamp(fences[6]), RuntimeIndex: localRuntimeFenceStamp(fences[7]), Quiescence: localRuntimeStamp(history[0]), QuiescenceReceipt: localRuntimeStamp(history[1]), QuiescenceReservation: localRuntimeStamp(history[2]), FinalSync: localRuntimeStamp(points[1]), Issuer: localRuntimeStamp(history[3]), Archive: localRuntimeStamp(points[0]), LocalComplete: localRuntimeFenceStamp(a.complete), QuiescenceStage: p.TerminationQuiescenceStage{RequestID: q.Reference.Stage.RequestID, StageID: q.Reference.Stage.StageID, AttemptID: q.Reference.Stage.AttemptID, Digest: q.Reference.Stage.Digest}}
	return nil
}
