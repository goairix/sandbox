package etcd

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// These cases exercise real etcd operator metadata only, without worker IO.
func TestIntegrationTaskWorkspaceSyncRecyclingMetadata(t *testing.T) {
	for _, fault := range []string{"valid", "missing", "legacy-version", "wrong-budget", "wrong-limit", "leased", "modified", "duplicate", "missing-field", "null", "noncanonical"} {
		t.Run(fault, func(t *testing.T) {
			base, raw := integrationBackend(t)
			ctx := context.Background()
			budget := TaskWorkspaceSyncBudgetOptions{uuid.NewString(), 1, 4096}
			o := integrationOptions(base, raw)
			o.TaskWorkspaceSyncRecyclingBudget = &budget
			key, err := base.namespace.Key("meta", "workspace_sync_recycling_budget")
			require.NoError(t, err)
			value, err := encodeOperationMetadata(taskWorkspaceSyncRecyclingBudgetRecord{2, budget}, 1024)
			require.NoError(t, err)
			switch fault {
			case "legacy-version":
				value = strings.Replace(value, `"version":2`, `"version":1`, 1)
			case "wrong-budget":
				value = strings.Replace(value, budget.BudgetID, uuid.NewString(), 1)
			case "wrong-limit":
				value = strings.Replace(value, `"max_intent_bytes":4096`, `"max_intent_bytes":8192`, 1)
			case "duplicate":
				value = strings.Replace(value, `"version":2`, `"version":2,"version":2`, 1)
			case "missing-field":
				value = strings.Replace(value, `,"slots_per_partition":1`, "", 1)
			case "null":
				value = strings.Replace(value, `"max_intent_bytes":4096`, `"max_intent_bytes":null`, 1)
			case "noncanonical":
				value = " " + value
			}
			var putOptions []clientv3.OpOption
			if fault == "leased" {
				lease, err := raw.Grant(ctx, 60)
				require.NoError(t, err)
				putOptions = append(putOptions, clientv3.WithLease(lease.ID))
				t.Cleanup(func() { _, err := raw.Revoke(ctx, lease.ID); require.NoError(t, err) })
			}
			if fault != "missing" {
				_, err = raw.Put(ctx, key, value, putOptions...)
				require.NoError(t, err)
			}
			if fault == "modified" {
				_, err = raw.Put(ctx, key, value)
				require.NoError(t, err)
			}
			// The backend must neither provision the remaining 255 slots nor
			// modify an already operator-provisioned permanent free slot.
			free := recyclingSlotFixture()
			free.BudgetID, free.Partition, free.Slot = budget.BudgetID, 0, 0
			freeValue, err := encodeWorkspaceSyncRecyclingSlot(free)
			require.NoError(t, err)
			slotKey, err := base.namespace.Key("p", "00", "workspace-sync-recycling-slots", budget.BudgetID, "00")
			require.NoError(t, err)
			_, err = raw.Put(ctx, slotKey, freeValue)
			require.NoError(t, err)
			before, err := raw.Get(ctx, base.namespace.Root(), clientv3.WithPrefix())
			require.NoError(t, err)
			opened, err := New(ctx, o)
			if fault == "valid" {
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, opened.Close()) })
				pin := opened.workspaceSyncBudget
				require.EqualValues(t, 2, pin.profile)
				require.Equal(t, budget, pin.options)
				require.Equal(t, key, pin.key)
				require.Equal(t, value, pin.value)
				budget.MaxIntentBytes = 8192
				require.EqualValues(t, 4096, pin.options.MaxIntentBytes, "New owns a copy")
				config, err := raw.Get(ctx, key)
				require.NoError(t, err)
				require.True(t, opened.workspaceSyncBudgetMatches(config.Kvs[0]))
			} else {
				require.ErrorIs(t, err, ErrIdentityMismatch)
				require.Nil(t, opened)
			}
			after, err := raw.Get(ctx, base.namespace.Root(), clientv3.WithPrefix())
			require.NoError(t, err)
			require.Equal(t, before.Kvs, after.Kvs, "New performs no namespace writes")
			require.Equal(t, before.Header.Revision, after.Header.Revision, "New performs no cluster writes")
			t.Logf("operator metadata case=%s namespace=%s exact_keys=%d revision=%d unchanged", fault, base.namespace.Root(), len(after.Kvs), after.Header.Revision)
			for _, kv := range after.Kvs {
				t.Logf("operator inventory key=%s create=%d mod=%d version=%d lease=%d value=%s", kv.Key, kv.CreateRevision, kv.ModRevision, kv.Version, kv.Lease, kv.Value)
			}
		})
	}
}

func TestIntegrationTaskWorkspaceSyncRecyclingConfigGeneration(t *testing.T) {
	base, raw := integrationBackend(t)
	ctx := context.Background()
	budget := TaskWorkspaceSyncBudgetOptions{uuid.NewString(), 1, 4096}
	o := integrationOptions(base, raw)
	o.TaskWorkspaceSyncRecyclingBudget = &budget
	key, err := base.namespace.Key("meta", "workspace_sync_recycling_budget")
	require.NoError(t, err)
	value, err := encodeOperationMetadata(taskWorkspaceSyncRecyclingBudgetRecord{2, budget}, 1024)
	require.NoError(t, err)
	_, err = raw.Put(ctx, key, value)
	require.NoError(t, err)
	opened, err := New(ctx, o)
	require.NoError(t, err)
	defer func() { require.NoError(t, opened.Close()) }()
	old := opened.workspaceSyncBudget
	_, err = raw.Delete(ctx, key)
	require.NoError(t, err)
	_, err = raw.Put(ctx, key, value)
	require.NoError(t, err)
	current, err := raw.Get(ctx, key)
	require.NoError(t, err)
	require.Greater(t, current.Kvs[0].CreateRevision, old.create)
	require.False(t, opened.workspaceSyncBudgetMatches(current.Kvs[0]), "same value cannot replace a pinned config birth")
	fenced, err := raw.Txn(ctx).If(old.comparisons()...).Then(clientv3.OpGet(key)).Commit()
	require.NoError(t, err)
	require.False(t, fenced.Succeeded)
	before, err := raw.Get(ctx, base.namespace.Root(), clientv3.WithPrefix())
	require.NoError(t, err)
	err = opened.pinTaskWorkspaceSyncRecyclingBudget(ctx, &TaskWorkspaceSyncBudgetOptions{budget.BudgetID, 1, 8192})
	require.ErrorIs(t, err, ErrIdentityMismatch)
	require.Same(t, old, opened.workspaceSyncBudget, "failed pin does not replace original")
	after, err := raw.Get(ctx, base.namespace.Root(), clientv3.WithPrefix())
	require.NoError(t, err)
	require.Equal(t, before.Kvs, after.Kvs)
	require.Equal(t, before.Header.Revision, after.Header.Revision)
	t.Logf("config generation old=%d current=%d old comparisons rejected; failed pin readonly", old.create, current.Kvs[0].CreateRevision)
}

func TestIntegrationTaskWorkspaceSyncRecyclingLegacyConstructor(t *testing.T) {
	base, raw := integrationBackend(t)
	require.Nil(t, base.workspaceSyncBudget)
	budget := TaskWorkspaceSyncBudgetOptions{uuid.NewString(), 1, 4096}
	o := integrationOptions(base, raw)
	o.TaskWorkspaceSyncBudget = &budget
	key, err := base.namespace.Key("meta", "workspace_sync_budget")
	require.NoError(t, err)
	value, err := encodeOperationMetadata(taskWorkspaceSyncBudgetRecord{1, budget}, 1024)
	require.NoError(t, err)
	_, err = raw.Put(context.Background(), key, value)
	require.NoError(t, err)
	before, err := raw.Get(context.Background(), base.namespace.Root(), clientv3.WithPrefix())
	require.NoError(t, err)
	opened, err := New(context.Background(), o)
	require.NoError(t, err)
	defer func() { require.NoError(t, opened.Close()) }()
	require.EqualValues(t, 1, opened.workspaceSyncBudget.profile)
	require.Equal(t, value, opened.workspaceSyncBudget.value)
	after, err := raw.Get(context.Background(), base.namespace.Root(), clientv3.WithPrefix())
	require.NoError(t, err)
	require.Equal(t, before.Kvs, after.Kvs)
	require.Equal(t, before.Header.Revision, after.Header.Revision)
	t.Logf("legacy constructor explicit profile1 exact_keys=%d revision=%d unchanged", len(after.Kvs), after.Header.Revision)
}
