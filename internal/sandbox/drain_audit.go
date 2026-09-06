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
		if len(keys) != 0 {
			auditErr = errors.Join(auditErr, fmt.Errorf("drain blocked by %s state (%d keys)", prefix.name, len(keys)))
		}
	}
	return auditErr
}
