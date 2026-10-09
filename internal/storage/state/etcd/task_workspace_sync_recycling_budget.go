package etcd

import "context"

func (r taskWorkspaceSyncRecyclingBudgetRecord) Validate() error {
	if r.Version != 2 || validateTaskWorkspaceSyncBudget(&r.TaskWorkspaceSyncBudgetOptions) != nil {
		return ErrInvalidRecord
	}
	return nil
}

func (r taskWorkspaceSyncRecyclingBudgetRecord) budgetOptions() TaskWorkspaceSyncBudgetOptions {
	return r.TaskWorkspaceSyncBudgetOptions
}

func (b *Backend) pinTaskWorkspaceSyncRecyclingBudget(ctx context.Context, options *TaskWorkspaceSyncBudgetOptions) error {
	return pinTaskWorkspaceSyncBudgetRecord[taskWorkspaceSyncRecyclingBudgetRecord](b, ctx, options, "workspace_sync_recycling_budget", 2)
}
