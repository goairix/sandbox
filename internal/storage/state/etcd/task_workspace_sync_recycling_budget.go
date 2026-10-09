package etcd

import "context"

func (r taskWorkspaceSyncRecyclingBudgetRecord) Validate() error {
	if r.Version != 2 || validateTaskWorkspaceSyncBudget(&r.TaskWorkspaceSyncBudgetOptions) != nil {
		return ErrInvalidRecord
	}
	return nil
}

func (b *Backend) pinTaskWorkspaceSyncRecyclingBudget(ctx context.Context, options *TaskWorkspaceSyncBudgetOptions) error {
	if options == nil {
		return nil
	}
	key, err := b.namespace.Key("meta", "workspace_sync_recycling_budget")
	if err != nil {
		return err
	}
	values, err := b.readDomain(ctx, key)
	if err != nil {
		return err
	}
	kv := values[0]
	var record taskWorkspaceSyncRecyclingBudgetRecord
	if !immutableWorkspaceSyncKV(kv) || decodePreparationDomain(kv, &record, 1024) != nil || record.TaskWorkspaceSyncBudgetOptions != *options {
		return ErrIdentityMismatch
	}
	value, err := encodeOperationMetadata(record, 1024)
	if err != nil || value != string(kv.Value) {
		return ErrIdentityMismatch
	}
	b.workspaceSyncBudget = &taskWorkspaceSyncBudgetPin{profile: 2, options: *options, key: key, value: value, create: kv.CreateRevision, mod: kv.ModRevision}
	return nil
}
