package etcd

import (
	"bytes"
	"errors"
	"reflect"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// These are actual surviving observations, never reconstructed native E/S or
// index records. The temporary bounded graph is not retained on a cleanup job.
type detachedRuntimeHistorySnapshot struct {
	termination *mvccpb.KeyValue
	provider    *mvccpb.KeyValue
	history     [13]*mvccpb.KeyValue
}

// This pure dispatcher supplies keys/caps, never authority. The actual Task
// derives snapshot identity without an extra paid RPC or a deleted index read.
// Local v1 keeps the original nine points and all its existing requirements.
func (b *Backend) taskOwnerReleaseHistoryKeys(intent p.TaskRuntimeTerminationIntent, actualTask *mvccpb.KeyValue) ([]string, []int, error) {
	if b == nil {
		return nil, nil, ErrInvalidRecord
	}
	if intent.Version == 1 {
		keys, err := b.localRuntimeHistoryKeys(intent, localRuntimeTerminationTask(intent))
		return keys, localRuntimeHistoryCaps(), err
	}
	keys, err := p.DetachedTaskRuntimeTerminationKeysFor(intent)
	if err != nil {
		return nil, nil, errors.Join(ErrCorruptRecord, err)
	}
	task, err := b.decodeDetachedRuntimeTask(intent, actualTask)
	if err != nil {
		return nil, nil, err
	}
	_, snapshot, err := b.namespace.sandboxKeys(intent.Partition, task.Reference.SandboxID, task.Snapshot.Version)
	if err != nil {
		return nil, nil, err
	}
	publication, proof, err := b.namespace.runtimePublicationKeys(intent.Partition, intent.CreationIntentID)
	if err != nil {
		return nil, nil, err
	}
	enrollment, activation, err := b.namespace.runtimeEnrollmentKeys(intent.Partition, intent.CreationIntentID)
	if err != nil {
		return nil, nil, err
	}
	return []string{keys.Task, keys.Quiescence, keys.QuiescenceReceipt, keys.QuiescenceReservation, keys.FinalSync, keys.Issuer, keys.Settlement, snapshot, keys.CreationIntent, publication, proof, enrollment, activation}, detachedRuntimeHistoryCaps(), nil
}

// Byte caps are inherited from the original native codecs, including the
// 64KiB generic snapshot and32KiB Q/fact. These thirteen values total208896;
// callers still measure real protobuf keys, metadata and both read branches.
func detachedRuntimeHistoryCaps() []int {
	return []int{4096, 32768, 4096, 8192, 8192, 8192, p.MaxDetachedNoEffectSettlementBytes, maxRecordBytes, 4096, 8192, 16384, 8192, 8192}
}

func (b *Backend) decodeDetachedRuntimeTask(intent p.TaskRuntimeTerminationIntent, actual *mvccpb.KeyValue) (TaskRecord, error) {
	var task TaskRecord
	if !enrolledImmutableKV(actual) || b.validateTaskReference(localRuntimeTerminationTask(intent)) != nil || decodeTaskRecord(actual, &task) != nil {
		return task, ErrCorruptRecord
	}
	key, err := b.namespace.taskKey(task.Reference)
	if err != nil {
		return TaskRecord{}, err
	}
	wire, err := encodeTaskRecord(task)
	if err != nil || wire != string(actual.Value) || string(actual.Key) != key || localRuntimeStamp(actual) != intent.Authority.Task || actual.CreateRevision != intent.TaskBirth || task.Reference != localRuntimeTerminationTask(intent) || task.CreationIntentID != intent.CreationIntentID || task.Runtime != RuntimeReference(intent.Current.Runtime) || task.WorkspaceHash != intent.Current.WorkspaceHash || task.Generation != intent.Current.Generation || task.DataGateEpoch != intent.Current.DataGateEpoch || intent.Current.ControlRevision != intent.TaskBirth || workspaceSyncIntentDigest(wire) != intent.Current.TaskDigest {
		return TaskRecord{}, errors.Join(ErrCorruptRecord, err)
	}
	return task, nil
}

// Refuse the wrong variant or an incomplete provider before spending the
// larger history read. Provider completion is bound by the intent's digest;
// a phase name alone never substitutes for the actual seven native slots.
func (b *Backend) decodeDetachedRuntimeHistoryHead(task TaskReference, snapshot *detachedRuntimeHistorySnapshot) (p.TaskRuntimeTerminationIntent, error) {
	if b == nil || b.detachedCleanup == nil || b.enrolledPublication == nil || b.enrolledVerifier == nil || b.publicationVerifier == nil || snapshot == nil {
		return p.TaskRuntimeTerminationIntent{}, ErrCorruptRecord
	}
	r, err := decodeLocalRuntimeTermination(snapshot.termination)
	if err != nil || r.Version != 2 || r.DetachedEffects == nil || r.Phase != "cleanup-complete" || localRuntimeTerminationTask(r) != task || r.Current.AuthorityID != b.publicationAuthorityID || r.Current.Target != b.publicationTarget || r.Current.Namespace != b.namespace.Root() || r.Current.RestoreEpoch != b.restoreEpoch || r.DetachedEffects.ContractDigest != b.detachedCleanup.contractDigest || r.DetachedEffects.ContractDigest != b.enrolledPublication.ContractDigest {
		return p.TaskRuntimeTerminationIntent{}, errors.Join(ErrCorruptRecord, err)
	}
	keys, err := p.DetachedTaskRuntimeTerminationKeysFor(r)
	if err != nil || string(snapshot.termination.Key) != keys.Intent {
		return p.TaskRuntimeTerminationIntent{}, errors.Join(ErrCorruptRecord, err)
	}
	provider, err := decodeTaskRuntimeProviderCleanup(snapshot.provider)
	if err != nil || string(snapshot.provider.Key) != keys.Task+"/runtime-provider-cleanup" || !localRuntimeProviderMatches(provider, r, r.DetachedEffects.ContractDigest) || !localRuntimeProviderComplete(provider) || workspaceSyncIntentDigest(string(snapshot.provider.Value)) != r.CleanupDigest {
		return p.TaskRuntimeTerminationIntent{}, errors.Join(ErrCorruptRecord, err)
	}
	return r, nil
}

// This pure final join accepts actual point envelopes only. It reauthenticates
// signatures in their original historical windows, with no provider, native IO,
// Claim/Run reconstruction or dependence on a current E/S/runtimeIndex.
func (b *Backend) decodeDetachedRuntimeHistory(task TaskReference, actualTask *mvccpb.KeyValue, snapshot *detachedRuntimeHistorySnapshot) (p.TaskRuntimeTerminationIntent, error) {
	r, err := b.decodeDetachedRuntimeHistoryHead(task, snapshot)
	if err != nil {
		return p.TaskRuntimeTerminationIntent{}, err
	}
	keys, caps, err := b.taskOwnerReleaseHistoryKeys(r, actualTask)
	if err != nil {
		return p.TaskRuntimeTerminationIntent{}, err
	}
	points := snapshot.history[:]
	for i, point := range points {
		if point == nil || string(point.Key) != keys[i] || len(point.Value) == 0 || len(point.Value) > caps[i] || point.ModRevision >= snapshot.termination.CreateRevision {
			return p.TaskRuntimeTerminationIntent{}, ErrCorruptRecord
		}
		// CreationIntent is published through legitimate mutable phases. Its
		// complete actual stamp is later matched against the permanent fact.
		if i == 8 {
			if !validLocalRuntimeMutableKV(point) {
				return p.TaskRuntimeTerminationIntent{}, ErrCorruptRecord
			}
		} else if !enrolledImmutableKV(point) {
			return p.TaskRuntimeTerminationIntent{}, ErrCorruptRecord
		}
	}
	if actualTask.Version != points[0].Version || !ownTaskFence(actualTask).matches(points[0]) {
		return p.TaskRuntimeTerminationIntent{}, ErrCorruptRecord
	}
	taskRecord, err := b.decodeDetachedRuntimeTask(r, points[0])
	if err != nil {
		return p.TaskRuntimeTerminationIntent{}, err
	}
	for i, stamp := range map[int]p.RecordStamp{0: r.Authority.Task, 1: r.Authority.Quiescence, 2: r.Authority.QuiescenceReceipt, 3: r.Authority.QuiescenceReservation, 4: r.Authority.FinalSync, 5: r.Authority.Issuer, 6: r.DetachedEffects.Settlement} {
		if localRuntimeStamp(points[i]) != stamp {
			return p.TaskRuntimeTerminationIntent{}, ErrCorruptRecord
		}
	}

	history, err := b.decodeDetachedRuntimeSources(taskRecord, points, snapshot.termination.CreateRevision)
	if err != nil {
		return p.TaskRuntimeTerminationIntent{}, err
	}
	issuer, err := b.decodeCommandIssuer(points[5])
	if err != nil || issuer == nil || issuer.Revision != history.quiescence.Record.IssuerRevision || issuer.Record.CertificateID != r.Current.IssuerCertificateID || issuer.Record.CertificateDigest != r.Current.IssuerCertificateDigest {
		return p.TaskRuntimeTerminationIntent{}, errors.Join(ErrCorruptRecord, err)
	}
	if err = b.verifyDetachedRuntimeHistory(history, issuer.Record.Certificate); err != nil {
		return p.TaskRuntimeTerminationIntent{}, err
	}
	if err = matchDetachedRuntimeHistoricalSender(r, history); err != nil {
		return p.TaskRuntimeTerminationIntent{}, err
	}
	if err = b.verifyLocalRuntimeProof(r, snapshot.termination, history.publication.Enrollment.ObserverCertificate); err != nil {
		return p.TaskRuntimeTerminationIntent{}, err
	}
	return r, nil
}

// All decoded values below come from the fixed actual point set. Reference is
// intentionally absent from publication: its Stage receipt is not in this
// surviving profile, and the common records checker must not invent one.
func (b *Backend) decodeDetachedRuntimeSources(task TaskRecord, points []*mvccpb.KeyValue, before int64) (*cleanupDetachedNoEffectHistory, error) {
	publication, err := b.decodeDetachedRuntimePublication(points[9:13])
	if err != nil {
		return nil, err
	}
	var snapshot SandboxSnapshotRecord
	var intent CreationIntentRecord
	if decodeDomainRecord(points[7], &snapshot) != nil || strictPreparationMetadata(points[8].Value, reflect.TypeOf(&intent)) != nil || decodeDomainRecord(points[8], &intent) != nil {
		return nil, ErrCorruptRecord
	}
	snapshotWire, err := encodeDomainRecord(snapshot)
	if err != nil || snapshotWire != string(points[7].Value) {
		return nil, errors.Join(ErrCorruptRecord, err)
	}
	intentWire, err := encodeDomainRecord(intent)
	if err != nil || intentWire != string(points[8].Value) {
		return nil, errors.Join(ErrCorruptRecord, err)
	}

	quiescence, pin, err := decodeDetachedRuntimeQuiescence(task, points[1], points[2], points[3])
	if err != nil {
		return nil, err
	}
	finalSync, err := taskFinalSyncEntry(points[4], task.Reference)
	if err != nil || finalSync == nil || finalSync.Record.Task != task || finalSync.Record.DestroyingRevision != points[0].CreateRevision || finalSync.Record.Plan != (TaskFinalSyncPlan{Mode: "none", Digest: b.detachedCleanup.contractDigest}) || finalSync.Record.Quiescence != quiescence.Reference || finalSync.Record.QuiescenceRevision != quiescence.Revision || finalSync.Record.Claim.CreateRevision < quiescence.Record.Claim.CreateRevision || finalSync.Record.Claim.CreateRevision == quiescence.Record.Claim.CreateRevision && finalSync.Record.Claim != quiescence.Record.Claim {
		return nil, errors.Join(ErrCorruptRecord, err)
	}
	entry, err := b.detachedNoEffectEntry(points[6], before, task.Reference)
	if err != nil || entry == nil {
		return nil, errors.Join(ErrCorruptRecord, err)
	}
	h := &cleanupDetachedNoEffectHistory{origin: b, task: task, snapshot: snapshot, intent: intent, publication: publication, quiescence: quiescence, finalSync: finalSync,
		taskFence: ownTaskFence(points[0]), quiescencePin: pin, quiescenceReservationFence: ownTaskFence(points[3]), snapshotFence: ownTaskFence(points[7]), intentFence: ownTaskFence(points[8]), publicationFence: ownTaskFence(points[9]), finalSyncFence: ownTaskFence(points[4]), settlement: entry}
	h.self = h
	if _, err = b.validateCleanupDetachedNoEffectSourceBindings(h, entry); err != nil {
		return nil, err
	}
	if _, err = b.detachedCleanupSourceRecords(task, snapshot, intent, publication); err != nil {
		return nil, errors.Join(ErrCorruptRecord, err)
	}
	return h, nil
}

func decodeDetachedRuntimeQuiescence(task TaskRecord, qkv, receipt, reservationKV *mvccpb.KeyValue) (*TaskQuiescenceEntry, taskDualDrainPin, error) {
	fail := func(err error) (*TaskQuiescenceEntry, taskDualDrainPin, error) {
		return nil, taskDualDrainPin{}, errors.Join(ErrCorruptRecord, err)
	}
	var q TaskQuiescenceRecord
	if err := decodeTaskQuiescenceRecord(qkv, &q); err != nil {
		return fail(err)
	}
	wire, err := encodeTaskQuiescenceRecord(q)
	if err != nil || wire != string(qkv.Value) || q.Task != task {
		return fail(err)
	}
	stage, err := quiescenceCommittedReceipt(receipt, string(receipt.Key), qkv.CreateRevision, q.Attempt)
	if err != nil {
		return fail(err)
	}
	receiptWire, err := encodeReceipt(stage, OutcomeCommitted)
	if err != nil || receiptWire != string(receipt.Value) {
		return fail(err)
	}
	reservation, err := taskQuiescenceAttemptEntry(reservationKV, task.Reference)
	if err != nil || !taskQuiescenceHistoryReservation(q, qkv.CreateRevision, reservation) {
		return fail(err)
	}
	reservationWire, err := encodeTaskQuiescenceAttempt(*reservation.Record)
	if err != nil || reservationWire != string(reservationKV.Value) {
		return fail(err)
	}
	reference := TaskQuiescenceReference{Task: task.Reference, CommandID: q.Context.Current.CommandID, Stage: stage}
	entry := &TaskQuiescenceEntry{Reference: reference, Record: &q, Revision: qkv.CreateRevision, Outcome: OutcomeCommitted, reservation: reservation}
	pin := taskDualDrainPin{reference: reference, task: task, intent: ownTaskFence(qkv), receipt: ownTaskFence(receipt), reservation: taskQuiescenceReservationPin{key: string(reservationKV.Key), revision: reservationKV.ModRevision}}
	return entry, pin, nil
}

func (b *Backend) decodeDetachedRuntimePublication(points []*mvccpb.KeyValue) (*RuntimeEnrolledPublicationEntry, error) {
	var publication RuntimeEnrolledPublicationRecord
	var proof RuntimeEnrolledPublicationProofRecord
	var enrollment RuntimeEnrollmentRecord
	var activation RuntimeEnrollmentActivationRecord
	for i, record := range []interface{ Validate() error }{&publication, &proof, &enrollment, &activation} {
		if !enrolledImmutableKV(points[i]) || points[i].CreateRevision != points[0].CreateRevision || decodeEnrolledRecord(points[i].Value, record) != nil {
			return nil, ErrCorruptRecord
		}
	}
	r := publication
	if proof.IntentID != r.IntentID || proof.SandboxID != r.SandboxID || proof.WorkspaceHash != r.WorkspaceHash || proof.RestoreEpoch != r.RestoreEpoch || proof.Generation != r.Generation || proof.ProofDigest != r.ProofDigest || enrollment.IntentID != r.IntentID || enrollment.SandboxID != r.SandboxID || enrollment.WorkspaceHash != r.WorkspaceHash || enrollment.RestoreEpoch != r.RestoreEpoch || enrollment.Generation != r.Generation || enrollment.Runtime != r.Runtime || activation.IntentID != r.IntentID || activation.SandboxID != r.SandboxID || activation.WorkspaceHash != r.WorkspaceHash || activation.RestoreEpoch != r.RestoreEpoch || activation.Generation != r.Generation || activation.Runtime != r.Runtime || workspaceSyncIntentDigest(string(points[2].Value)) != r.EnrollmentRecordDigest || workspaceSyncIntentDigest(string(points[3].Value)) != r.ActivationRecordDigest {
		return nil, ErrCorruptRecord
	}
	evidence := p.EnrolledRuntimeEvidence{Request: p.NodeEnrollmentRequest{Version: 1, Purpose: "runtime_node_enrollment", Activation: bytes.Clone(activation.Activation), RuntimeCertificate: bytes.Clone(activation.RuntimeCertificate), IssuerCertificate: bytes.Clone(activation.IssuerCertificate)}, ObserverCertificate: bytes.Clone(enrollment.ObserverCertificate), Receipt: bytes.Clone(enrollment.Receipt)}
	return &RuntimeEnrolledPublicationEntry{Record: publication, Proof: bytes.Clone(proof.Payload), Enrollment: evidence}, nil
}

// Normalize only the original once-selected sender fields. A newer legal
// termination sender or writer does not alter the old source's identity, and
// the sealed fact's Claim/Run never becomes current live permission.
func matchDetachedRuntimeHistoricalSender(r p.TaskRuntimeTerminationIntent, h *cleanupDetachedNoEffectHistory) error {
	source, current := h.quiescence.Record.Context.Current, r.Current
	current.CommandID, current.ClaimID, current.WorkerID = source.CommandID, source.ClaimID, source.WorkerID
	current.ClaimCreateRevision, current.LeaseID = source.ClaimCreateRevision, source.LeaseID
	writer := h.settlement.record.WriterClaim
	sender := p.DetachedNoEffectClaim{ClaimID: r.Current.ClaimID, WorkerID: r.Current.WorkerID, CreateRevision: r.Current.ClaimCreateRevision, LeaseID: r.Current.LeaseID}
	if current != source || sender.CreateRevision < writer.CreateRevision || sender.CreateRevision == writer.CreateRevision && sender != writer || r.Authority.QuiescenceStage != h.settlement.record.QuiescenceStage || r.EnrollmentDigest != h.publication.Record.EnrollmentDigest || r.ObserverCertificateDigest != h.publication.Record.ObserverCertificateDigest || r.NodeUID != h.publication.Record.NodeUID || r.NodeBootID != h.publication.Record.NodeBootID {
		return ErrCorruptRecord
	}
	return nil
}
