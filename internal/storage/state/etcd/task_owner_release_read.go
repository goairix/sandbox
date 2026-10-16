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
	termination, kv, _, err := b.readLocalRuntimeHistory(ctx, task)
	if err != nil || kv == nil {
		return nil, errors.Join(ErrCorruptRecord, err)
	}
	expected, err := newLocalOwnerReleaseRecord(termination, kv.CreateRevision, kv.ModRevision, r.Claim)
	if err != nil || expected != r {
		return nil, errors.Join(ErrCorruptRecord, err)
	}
	return &localOwnerReleaseProof{record: r, kv: values[0], termination: termination}, nil
}
