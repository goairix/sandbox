package sandbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/goairix/sandbox/internal/storage/state"
)

var blockingDrainPrefixes = []struct {
	name    string
	pattern string
}{
	{name: "persistent session", pattern: sandboxSessionKeyPrefix + "*"},
	{name: "ephemeral lifecycle", pattern: ephemeralLifecycleKeyPrefix + "*"},
	{name: "workspace lease", pattern: workspaceLeaseKeyPrefix + "*"},
	{name: "workspace owner", pattern: workspaceOwnerKeyPrefix + "*"},
	{name: "FUSE pool", pattern: "fusepool:*"},
}

const fusePoolMembershipGenerationsKey = "fusepool:membership-generations"

// AuditDrainedState is read-only and fail-closed. Persistent workspace
// generation counters are deliberately retained as fencing history.
func AuditDrainedState(ctx context.Context, store state.Store) error {
	if store == nil {
		return fmt.Errorf("drain audit requires a state store")
	}
	var auditErr error
	for _, prefix := range blockingDrainPrefixes {
		keys, err := store.Keys(ctx, prefix.pattern)
		if err != nil {
			auditErr = errors.Join(auditErr, fmt.Errorf("audit %s state: %w", prefix.name, err))
			continue
		}
		blocking := 0
		for _, key := range keys {
			// Membership generations are fencing history, not active pool
			// inventory. Keeping them prevents an ABA across later refills.
			if prefix.name == "FUSE pool" && key == fusePoolMembershipGenerationsKey {
				continue
			}
			blocking++
		}
		if blocking != 0 {
			auditErr = errors.Join(auditErr, fmt.Errorf("drain blocked by %s state (%d keys)", prefix.name, blocking))
		}
	}
	return auditErr
}
