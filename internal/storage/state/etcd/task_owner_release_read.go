package etcd

import (
	"context"
	"errors"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

func (b *Backend) LoadTaskOwnerRelease(ctx context.Context, task TaskReference) (*TaskOwnerReleaseEntry, error) {
	o, err := b.beginLocalRuntimeRead(ctx, task)
	if err != nil {
		return nil, err
	}
	defer o.finish()
	return b.readLocalOwnerRelease(o.ctx, task)
}

func (b *Backend) readLocalOwnerRelease(ctx context.Context, task TaskReference) (*TaskOwnerReleaseEntry, error) {
	snapshot, err := b.readLocalOwnerReleaseProof(ctx, task)
	if err != nil || snapshot == nil {
		return nil, err
	}
	return &TaskOwnerReleaseEntry{Task: task, CreateRevision: snapshot.kv.CreateRevision, SHA256: workspaceSyncIntentDigest(string(snapshot.kv.Value))}, nil
}

// Only this private loader can authenticate a release seal. The public DTO is
// deliberately a projection and cannot be supplied to quota settlement.
type localOwnerReleaseProof struct {
	record      localOwnerReleaseRecord
	kv          *mvccpb.KeyValue
	termination p.TaskRuntimeTerminationIntent
	// Only terminal observation attaches history until its final MVCC pins.
	// Existing physical/quota/worker callers retain the original small proof.
	history *localRuntimeHistorySnapshot
}

func (b *Backend) readLocalOwnerReleaseProof(ctx context.Context, task TaskReference) (*localOwnerReleaseProof, error) {
	_, _, key, err := b.namespace.localRuntimeReleaseKeys(task)
	if err != nil {
		return nil, err
	}
	values, err := b.readEnrolledPoints(ctx, []string{key}, []int{8192})
	if err != nil || values[0] == nil {
		return nil, err
	}
	r, err := decodeLocalOwnerRelease(values[0])
	if err != nil || r.Task != task {
		return nil, errors.Join(ErrCorruptRecord, err)
	}
	history, err := b.readLocalRuntimeHistorySnapshot(ctx, task)
	if err != nil || history == nil {
		return nil, errors.Join(ErrCorruptRecord, err)
	}
	return b.decodeLocalOwnerReleaseProof(task, values[0], history)
}

// A release record becomes authenticated only by joining its exact canonical
// bytes to the original signed termination and complete retained history. This
// pure validator grants neither physical cleanup nor quota/claim authority.
func (b *Backend) decodeLocalOwnerReleaseProof(task TaskReference, release *mvccpb.KeyValue, history *localRuntimeHistorySnapshot) (*localOwnerReleaseProof, error) {
	r, err := decodeLocalOwnerRelease(release)
	if err != nil || r.Task != task {
		return nil, errors.Join(ErrCorruptRecord, err)
	}
	termination, err := b.decodeLocalRuntimeHistory(task, history)
	if err != nil {
		return nil, errors.Join(ErrCorruptRecord, err)
	}
	kv := history.termination
	expected, err := newLocalOwnerReleaseRecord(termination, kv.CreateRevision, kv.ModRevision, r.Claim)
	if err != nil || expected != r {
		return nil, errors.Join(ErrCorruptRecord, err)
	}
	return &localOwnerReleaseProof{record: r, kv: release, termination: termination}, nil
}
