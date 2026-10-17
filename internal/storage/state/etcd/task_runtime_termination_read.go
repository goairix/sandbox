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
	snapshot, err := b.readLocalRuntimeHistorySnapshot(ctx, task)
	if err != nil || snapshot == nil {
		return r, nil, nil, err
	}
	r, err = b.decodeLocalRuntimeHistory(task, snapshot)
	if err != nil {
		return r, nil, nil, err
	}
	return r, snapshot.termination, snapshot.provider, nil
}

// The fixed snapshot preserves original MVCC points rather than reconstructing
// stamps from decoded fields. The read-only terminal observer retains it until
// final confirmation; the original physical loaders keep their own admission.
type localRuntimeHistorySnapshot struct {
	termination *mvccpb.KeyValue
	provider    *mvccpb.KeyValue
	history     [9]*mvccpb.KeyValue
}

func (b *Backend) readLocalRuntimeHistorySnapshot(ctx context.Context, task TaskReference) (*localRuntimeHistorySnapshot, error) {
	tk, pk, _, err := b.namespace.localRuntimeReleaseKeys(task)
	if err != nil {
		return nil, err
	}
	values, err := b.readEnrolledPoints(ctx, []string{tk, pk}, []int{16384, 16384})
	if err != nil || values[0] == nil {
		return nil, err
	}
	snapshot := &localRuntimeHistorySnapshot{termination: values[0], provider: values[1]}
	r, err := b.decodeLocalRuntimeHistoryHead(task, snapshot)
	if err != nil {
		return nil, err
	}
	keys, err := b.localRuntimeHistoryKeys(r, task)
	if err != nil {
		return nil, err
	}
	history, err := b.readEnrolledPoints(ctx, keys, localRuntimeHistoryCaps())
	if err != nil {
		return nil, err
	}
	copy(snapshot.history[:], history)
	return snapshot, nil
}

func localRuntimeHistoryCaps() []int {
	return []int{4096, 8192, 32768, 4096, 8192, 8192, 8192, maxRecordBytes, 8192}
}

func (b *Backend) localRuntimeHistoryKeys(r p.TaskRuntimeTerminationIntent, task TaskReference) ([]string, error) {
	keys, err := p.TaskRuntimeTerminationKeysFor(r)
	if err != nil {
		return nil, err
	}
	ek, _, err := b.namespace.runtimeEnrollmentKeys(task.Partition, r.CreationIntentID)
	if err != nil {
		return nil, err
	}
	return []string{keys.Task, keys.LocalComplete, keys.Quiescence, keys.QuiescenceReceipt, keys.QuiescenceReservation, keys.FinalSync, keys.Issuer, keys.Archive, ek}, nil
}

// This validator performs no reads and accepts only the complete fixed private
// snapshot. Signature verification authenticates the original reserved intent;
// every surviving historical point must match its signed full stamp.
func (b *Backend) decodeLocalRuntimeHistory(task TaskReference, snapshot *localRuntimeHistorySnapshot) (r p.TaskRuntimeTerminationIntent, resultErr error) {
	r, err := b.decodeLocalRuntimeHistoryHead(task, snapshot)
	if err != nil {
		return r, err
	}
	kv, history := snapshot.termination, snapshot.history[:]
	keys, err := b.localRuntimeHistoryKeys(r, task)
	if err != nil {
		return r, err
	}
	for i, point := range history {
		if point == nil || string(point.Key) != keys[i] {
			return r, ErrCorruptRecord
		}
	}
	stamps := []p.RecordStamp{r.Authority.Task, r.Authority.LocalComplete, r.Authority.Quiescence, r.Authority.QuiescenceReceipt, r.Authority.QuiescenceReservation, r.Authority.FinalSync, r.Authority.Issuer, r.Authority.Archive}
	for i, stamp := range stamps {
		if !enrolledImmutableKV(history[i]) || localRuntimeStamp(history[i]) != stamp {
			return r, ErrCorruptRecord
		}
	}
	var tr TaskRecord
	complete, completeErr := decodeWorkspaceSyncLocalComplete(history[1])
	if decodeTaskRecord(history[0], &tr) != nil || tr.Reference != task || tr.Runtime != RuntimeReference(r.Current.Runtime) || tr.WorkspaceHash != r.Current.WorkspaceHash || tr.Generation != r.Current.Generation || tr.DataGateEpoch != r.Current.DataGateEpoch || tr.CreationIntentID != r.CreationIntentID || history[0].CreateRevision != r.TaskBirth || completeErr != nil || complete.Task != task || complete.TaskBirth != r.TaskBirth {
		return r, ErrCorruptRecord
	}
	var enrollment RuntimeEnrollmentRecord
	if !enrolledImmutableKV(history[8]) || decodeEnrolledRecord(history[8].Value, &enrollment) != nil || enrollment.IntentID != r.CreationIntentID || enrollment.Runtime != tr.Runtime || enrollment.Generation != tr.Generation || enrollment.SandboxID != task.SandboxID || enrollment.WorkspaceHash != tr.WorkspaceHash || enrollment.RestoreEpoch != task.RestoreEpoch || workspaceSyncIntentDigest(string(enrollment.ObserverCertificate)) != r.ObserverCertificateDigest {
		return r, ErrCorruptRecord
	}
	if r.Phase != "reserved" {
		if err := b.verifyLocalRuntimeProof(r, kv, enrollment.ObserverCertificate); err != nil {
			return r, err
		}
	}
	return r, nil
}

// Validate the terminal/provider pair before spending the historical read. Both
// original loaders and the read-only observer retain this early-refusal order.
func (b *Backend) decodeLocalRuntimeHistoryHead(task TaskReference, snapshot *localRuntimeHistorySnapshot) (r p.TaskRuntimeTerminationIntent, resultErr error) {
	if snapshot == nil || b.enrolledPublication == nil {
		return r, ErrCorruptRecord
	}
	r, err := decodeLocalRuntimeTermination(snapshot.termination)
	if err != nil || localRuntimeTerminationTask(r) != task || r.MountAttempt != 0 {
		return r, errors.Join(ErrCorruptRecord, err)
	}
	provider := snapshot.provider
	pr, err := decodeTaskRuntimeProviderCleanup(provider)
	if err != nil || !localRuntimeProviderMatches(pr, r, b.enrolledPublication.ContractDigest) {
		return r, errors.Join(ErrCorruptRecord, err)
	}
	if r.Phase == "cleanup-complete" && (workspaceSyncIntentDigest(string(provider.Value)) != r.CleanupDigest || !localRuntimeProviderComplete(pr)) {
		return r, ErrCorruptRecord
	}
	return r, nil
}
