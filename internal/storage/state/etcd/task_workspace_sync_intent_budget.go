package etcd

import (
	"context"
	"fmt"

	clientv3 "go.etcd.io/etcd/client/v3"
)

func validateTaskWorkspaceSyncBudget(o *TaskWorkspaceSyncBudgetOptions) error {
	if o != nil && (!validPreparationUUID(o.BudgetID) || o.SlotsPerPartition < 1 || o.SlotsPerPartition > 64 || o.MaxIntentBytes < 4096 || o.MaxIntentBytes > 16384) {
		return fmt.Errorf("%w: workspace sync budget", ErrInvalidConfiguration)
	}
	return nil
}

func (r taskWorkspaceSyncBudgetRecord) Validate() error {
	if r.Version != 1 || validateTaskWorkspaceSyncBudget(&r.TaskWorkspaceSyncBudgetOptions) != nil {
		return ErrInvalidRecord
	}
	return nil
}

func (r taskWorkspaceSyncBudgetRecord) budgetOptions() TaskWorkspaceSyncBudgetOptions {
	return r.TaskWorkspaceSyncBudgetOptions
}

// Logical keys and values only; this excludes MVCC, WAL and replication costs.
func (o TaskWorkspaceSyncBudgetOptions) logicalByteBound() uint64 {
	return 256 * uint64(o.SlotsPerPartition) * (uint64(o.MaxIntentBytes) + 512 + 2*1024)
}

func (b *Backend) pinTaskWorkspaceSyncBudget(ctx context.Context, options *TaskWorkspaceSyncBudgetOptions) error {
	return pinTaskWorkspaceSyncBudgetRecord[taskWorkspaceSyncBudgetRecord](b, ctx, options, "workspace_sync_budget", 1)
}

// Each profile selects its concrete strict schema and immutable metadata key;
// the native pin validation and publication follow one shared read-only path.
func pinTaskWorkspaceSyncBudgetRecord[T interface {
	Validate() error
	budgetOptions() TaskWorkspaceSyncBudgetOptions
}](b *Backend, ctx context.Context, options *TaskWorkspaceSyncBudgetOptions, metadata string, profile uint8) error {
	if options == nil {
		return nil
	}
	key, err := b.namespace.Key("meta", metadata)
	if err != nil {
		return err
	}
	values, err := b.readDomain(ctx, key)
	if err != nil {
		return err
	}
	kv := values[0]
	var record T
	if !immutableWorkspaceSyncKV(kv) || decodePreparationDomain(kv, &record, 1024) != nil || record.budgetOptions() != *options {
		return ErrIdentityMismatch
	}
	value, err := encodeOperationMetadata(record, 1024)
	if err != nil || value != string(kv.Value) {
		return ErrIdentityMismatch
	}
	b.workspaceSyncBudget = &taskWorkspaceSyncBudgetPin{profile: profile, options: *options, key: key, value: value, create: kv.CreateRevision, mod: kv.ModRevision}
	return nil
}

func (p *taskWorkspaceSyncBudgetPin) comparisons() []clientv3.Cmp {
	return []clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(p.key), "=", p.create), clientv3.Compare(clientv3.ModRevision(p.key), "=", p.mod), clientv3.Compare(clientv3.Value(p.key), "=", p.value)}
}
