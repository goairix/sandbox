package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
)

// retireObsolete runs under the release-wide refill lock, after replacement
// capacity is prepared. Claims use record CAS, so an Acquire winning the race
// can never be converted to cleanup. Live owners keep their entire generation.
func (p *sharedOrdinaryPool) retireObsolete(ctx context.Context, target int) error {
	current, err := p.listCurrentRecords(ctx)
	if err != nil {
		return err
	}
	prepared := 0
	for _, entry := range current {
		if entry.record.State == ordinaryPoolPrepared {
			prepared++
		}
	}
	if prepared < target {
		return nil
	}
	keys, err := p.store.Keys(ctx, p.scopePattern())
	if err != nil {
		return err
	}
	entries, err := p.loadRecords(ctx, ordinaryPoolRecordKeys(keys))
	if err != nil {
		return err
	}
	pods, err := p.pool.runtime.ListSandboxes(ctx, map[string]string{"sandbox.pool": "true"})
	if err != nil {
		return err
	}
	byID := make(map[string]runtime.SandboxInfo)
	byInstance := make(map[string]runtime.SandboxInfo)
	for _, pod := range pods {
		if pod.Labels["sandbox.workspace.mode"] != string(WorkspaceMountFUSE) {
			byID[pod.RuntimeID] = pod
			byInstance[pod.Labels["sandbox.pool.instance"]] = pod
		}
	}
	ownerFree := make(map[string]bool)
	for _, entry := range entries {
		r := entry.record
		if r.PoolKey == p.poolKey || r.State == ordinaryPoolClaimed {
			continue
		}
		free, checked := ownerFree[r.PoolKey]
		if !checked {
			// Old, unscoped fingerprints could be shared by releases. A live
			// owner anywhere using that exact key fences legacy retirement.
			owners, err := p.store.Keys(ctx, "ordinarypool:v1:*:"+r.PoolKey+":owner:*")
			if err != nil {
				return err
			}
			free = len(owners) == 0
			ownerFree[r.PoolKey] = free
		}
		if !free || (r.State == ordinaryPoolPreparing && time.Since(r.UpdatedAt) < ordinaryPoolStaleTTL) {
			continue
		}
		var podRef *runtime.SandboxInfo
		pod, found := byID[r.RuntimeID]
		if r.RuntimeID == "" {
			pod, found = byInstance[r.PreparationID]
		}
		if found {
			if (r.RuntimeUID != "" && pod.RuntimeUID != r.RuntimeUID) || pod.Labels["sandbox.pool.key"] != r.PoolKey || pod.Labels["sandbox.pool.instance"] != r.PreparationID {
				return fmt.Errorf("obsolete ordinary pool runtime identity changed: %s", r.RuntimeID)
			}
			podRef = &pod
		} else if r.RuntimeID != "" {
			// The pool label may have been migrated by a concurrent claimant.
			// Never use absence from the filtered list as deletion authority.
			if _, err := p.pool.runtime.GetSandbox(ctx, r.RuntimeID); err == nil {
				continue
			} else if !errors.Is(err, runtime.ErrNotFound) {
				return err
			}
		}
		if err := p.cleanupRecord(ctx, entry, podRef); err != nil {
			return err
		}
	}
	return nil
}
