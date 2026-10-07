package etcd

import "context"

func (b *Backend) validateTaskReference(r TaskReference) error {
	if r.Validate() != nil {
		return ErrInvalidRecord
	}
	if b == nil || r.Namespace != b.namespace.Root() || r.RestoreEpoch != b.restoreEpoch {
		return ErrIdentityMismatch
	}
	return nil
}

// LoadTask reads owned historical diagnostics at one identity-fenced point.
// It neither joins current runtime ownership nor grants cleanup authority.
func (b *Backend) LoadTask(ctx context.Context, ref TaskReference) (*TaskRecord, error) {
	if err := b.validateTaskReference(ref); err != nil {
		return nil, err
	}
	key, err := b.namespace.taskKey(ref)
	if err != nil {
		return nil, err
	}
	values, err := b.readDomain(ctx, key)
	if err != nil {
		return nil, err
	}
	if values[0] == nil {
		return nil, nil
	}
	var record TaskRecord
	if err = decodeTaskRecord(values[0], &record); err != nil {
		return nil, err
	}
	if record.Reference != ref {
		return nil, ErrCorruptRecord
	}
	return &record, nil
}

// LoadTaskCheckpoint returns a fresh permanent metadata observation, never a
// retained claim, physical result, completion permission or authority handle.
func (b *Backend) LoadTaskCheckpoint(ctx context.Context, ref TaskReference) (*TaskCheckpointRecord, error) {
	if err := b.validateTaskReference(ref); err != nil {
		return nil, err
	}
	key, err := b.namespace.taskCheckpointKey(ref)
	if err != nil {
		return nil, err
	}
	values, err := b.readDomain(ctx, key)
	if err != nil {
		return nil, err
	}
	if values[0] == nil {
		return nil, nil
	}
	var record TaskCheckpointRecord
	if err = decodeTaskCheckpointRecord(values[0], &record); err != nil {
		return nil, err
	}
	if record.Reference != ref {
		return nil, ErrCorruptRecord
	}
	return &record, nil
}
