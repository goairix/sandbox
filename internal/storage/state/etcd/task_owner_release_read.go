package etcd

import (
	"context"
	"errors"
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
	return &TaskOwnerReleaseEntry{Task: task, CreateRevision: values[0].CreateRevision, SHA256: workspaceSyncIntentDigest(string(values[0].Value))}, nil
}
