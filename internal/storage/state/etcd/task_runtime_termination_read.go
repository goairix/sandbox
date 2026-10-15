package etcd

import (
	"context"
	"errors"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

func localRuntimeTerminationTask(r p.TaskRuntimeTerminationIntent) TaskReference {
	return TaskReference{Namespace: r.Current.Namespace, RestoreEpoch: r.Current.RestoreEpoch, TaskID: r.Current.TaskID, SandboxID: r.Current.SandboxID, Partition: r.Partition}
}

func localRuntimeTerminationQuery(r p.TaskRuntimeTerminationIntent, kv *mvccpb.KeyValue) (p.NodeTerminationQuery, error) {
	actual, err := decodeLocalRuntimeTermination(kv)
	if err != nil || !sameLocalRuntimeTerminationOrigin(actual, r) {
		return p.NodeTerminationQuery{}, errors.Join(ErrCorruptRecord, err)
	}
	r.Phase, r.TerminalProof, r.CleanupDigest = "reserved", nil, ""
	wire, err := p.EncodeTaskRuntimeTerminationIntent(r)
	if err != nil {
		return p.NodeTerminationQuery{}, err
	}
	return p.NodeTerminationQuery{Version: 1, Purpose: "task_runtime_terminate_query", Partition: r.Partition, TaskID: r.Current.TaskID, CommandID: r.Current.CommandID, IntentCreateRevision: kv.CreateRevision, IntentDigest: workspaceSyncIntentDigest(string(wire)), EnrollmentDigest: r.EnrollmentDigest}, nil
}

func localRuntimeTerminationEntry(r p.TaskRuntimeTerminationIntent, kv *mvccpb.KeyValue) *TaskRuntimeTerminationEntry {
	return &TaskRuntimeTerminationEntry{Task: localRuntimeTerminationTask(r), CreateRevision: kv.CreateRevision, ModRevision: kv.ModRevision, SHA256: workspaceSyncIntentDigest(string(kv.Value)), Phase: r.Phase}
}

func (b *Backend) verifyLocalRuntimeProof(r p.TaskRuntimeTerminationIntent, kv *mvccpb.KeyValue, certificate []byte) error {
	if b.enrolledVerifier == nil || r.Phase == "reserved" {
		return ErrInvalidRecord
	}
	query, err := localRuntimeTerminationQuery(r, kv)
	if err != nil {
		return err
	}
	proof, err := b.enrolledVerifier.VerifyNodeTerminationProof(r.TerminalProof, certificate, query)
	if err != nil || proof.Current != r.Current || proof.NodeUID != r.NodeUID || proof.NodeBootID != r.NodeBootID || proof.ObserverCertificateDigest != r.ObserverCertificateDigest || proof.PodUID != r.Current.Runtime.UID {
		return errors.Join(ErrCorruptRecord, err)
	}
	return nil
}

func (b *Backend) LoadTaskRuntimeTermination(ctx context.Context, task TaskReference) (*TaskRuntimeTerminationEntry, error) {
	o, err := b.beginLocalRuntimeRead(ctx, task)
	if err != nil {
		return nil, err
	}
	defer o.finish()
	r, kv, _, err := b.readLocalRuntimeHistory(o.ctx, task)
	if err != nil || kv == nil {
		return nil, err
	}
	return localRuntimeTerminationEntry(r, kv), nil
}

// Cold diagnosis deliberately never reads the three released keys. A signed
// terminal proof authenticates the original reserved intent and its historical
// stamps; surviving records must still match those exact stamps.
func (b *Backend) readLocalRuntimeHistory(ctx context.Context, task TaskReference) (r p.TaskRuntimeTerminationIntent, kv *mvccpb.KeyValue, provider *mvccpb.KeyValue, resultErr error) {
	tk, pk, _, err := b.namespace.localRuntimeReleaseKeys(task)
	if err != nil {
		return r, nil, nil, err
	}
	values, err := b.readEnrolledPoints(ctx, []string{tk, pk}, []int{16384, 16384})
	if err != nil || values[0] == nil {
		return r, nil, nil, err
	}
	kv, provider = values[0], values[1]
	r, err = decodeLocalRuntimeTermination(kv)
	if err != nil || localRuntimeTerminationTask(r) != task || r.MountAttempt != 0 {
		return r, nil, nil, errors.Join(ErrCorruptRecord, err)
	}
	pr, err := decodeTaskRuntimeProviderCleanup(provider)
	if err != nil || !localRuntimeProviderMatches(pr, r, b.enrolledPublication.ContractDigest) {
		return r, nil, nil, errors.Join(ErrCorruptRecord, err)
	}
	if r.Phase == "cleanup-complete" && (workspaceSyncIntentDigest(string(provider.Value)) != r.CleanupDigest || !localRuntimeProviderComplete(pr)) {
		return r, nil, nil, ErrCorruptRecord
	}
	keys, err := p.TaskRuntimeTerminationKeysFor(r)
	if err != nil {
		return r, nil, nil, err
	}
	ek, _, err := b.namespace.runtimeEnrollmentKeys(task.Partition, r.CreationIntentID)
	if err != nil {
		return r, nil, nil, err
	}
	historyKeys := []string{keys.Task, keys.LocalComplete, keys.Quiescence, keys.QuiescenceReceipt, keys.QuiescenceReservation, keys.FinalSync, keys.Issuer, keys.Archive, ek}
	history, err := b.readEnrolledPoints(ctx, historyKeys, []int{4096, 8192, 32768, 4096, 8192, 8192, 8192, maxRecordBytes, 8192})
	if err != nil {
		return r, nil, nil, err
	}
	stamps := []p.RecordStamp{r.Authority.Task, r.Authority.LocalComplete, r.Authority.Quiescence, r.Authority.QuiescenceReceipt, r.Authority.QuiescenceReservation, r.Authority.FinalSync, r.Authority.Issuer, r.Authority.Archive}
	for i, stamp := range stamps {
		if !enrolledImmutableKV(history[i]) || localRuntimeStamp(history[i]) != stamp {
			return r, nil, nil, ErrCorruptRecord
		}
	}
	var tr TaskRecord
	complete, completeErr := decodeWorkspaceSyncLocalComplete(history[1])
	if decodeTaskRecord(history[0], &tr) != nil || tr.Reference != task || tr.Runtime != RuntimeReference(r.Current.Runtime) || tr.WorkspaceHash != r.Current.WorkspaceHash || tr.Generation != r.Current.Generation || tr.DataGateEpoch != r.Current.DataGateEpoch || tr.CreationIntentID != r.CreationIntentID || history[0].CreateRevision != r.TaskBirth || completeErr != nil || complete.Task != task || complete.TaskBirth != r.TaskBirth {
		return r, nil, nil, ErrCorruptRecord
	}
	var enrollment RuntimeEnrollmentRecord
	if !enrolledImmutableKV(history[8]) || decodeEnrolledRecord(history[8].Value, &enrollment) != nil || enrollment.IntentID != r.CreationIntentID || enrollment.Runtime != tr.Runtime || enrollment.Generation != tr.Generation || enrollment.SandboxID != task.SandboxID || enrollment.WorkspaceHash != tr.WorkspaceHash || enrollment.RestoreEpoch != task.RestoreEpoch || workspaceSyncIntentDigest(string(enrollment.ObserverCertificate)) != r.ObserverCertificateDigest {
		return r, nil, nil, ErrCorruptRecord
	}
	if r.Phase != "reserved" {
		if err := b.verifyLocalRuntimeProof(r, kv, enrollment.ObserverCertificate); err != nil {
			return r, nil, nil, err
		}
	}
	return r, kv, provider, nil
}
