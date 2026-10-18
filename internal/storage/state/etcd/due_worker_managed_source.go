package etcd

import "time"

// The source phase owns at most 16MiB (see cleanupManagedSourceBound); the
// serial source/preflight reservation is 24MiB. It comes from the original
// 40MiB dynamic allowance, independently of automatic and cold reservations.
const cleanupManagedWorkspaceBytes = 24 << 20
const cleanupManagedWorkspaceKeys = 20

type cleanupManagedSourceWorkspace struct {
	job *cleanupLocalJob
	// Compact native observations are diagnostics, never Claim/Run authority.
	pins  [16]cleanupManagedSourcePin
	count int
}

// acquireCleanupManagedSource runs only on the original serial actor, before
// acquiring a native owner. Pending checkpoints must finish their original CAS
// first: resetView deliberately cannot reset them, so invalidation here would
// otherwise lose the earlier coverage while preserving its later cursor.
func (w *DueWorker) acquireCleanupManagedSource(j *cleanupLocalJob) (bool, error) {
	if w.backend.detachedCleanup == nil || j.step != "snapshot" {
		return true, nil
	}
	b := w.backend
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if w.self != w || w.cache == nil || !w.cache.validLocked() || w.cache.session != w.session || b.archiveLife.closed || w.session.operation != nil || j.owner != nil || j.worker != w || !b.validWorkerPartition(j.partition) || j.partition.session != w.session || j.partition.lost || j.partition.released || j.partition.release != nil {
		return false, ErrInvalidRecord
	}
	registered := false
	for _, candidate := range w.cleanupJobs {
		registered = registered || candidate == j
	}
	if !registered || j.started || j.ctx.Err() != nil {
		return false, ErrGuardExpired
	}
	if err := w.session.live(); err != nil {
		return false, err
	}
	for _, scan := range w.scans {
		if scan.checkpoint != nil && scan.checkpoint.record.Pending != nil {
			return false, nil
		}
	}
	if w.managedSource != nil {
		return w.managedSource.job == j, nil
	}
	if cleanupManagedSourceBound() > 16<<20 {
		return false, ErrInvalidConfiguration
	}
	w.cache.managedSource = true
	keys, limit := w.cache.limitsLocked(w.cache.automatic, w.cache.coldQuota)
	if w.cache.bytes > limit || w.cache.count > keys {
		// This is an expiry heap, not an LRU. Rebuild complete coverage rather
		// than evicting an arbitrary suffix and pretending it remains complete.
		w.cache.invalidateLocked("managed source workspace")
		for _, scan := range w.scans {
			scan.resetView()
		}
		w.windowAt = time.Time{}
	}
	w.pauseDiscoveryLocked("replaying")
	w.managedSource = &cleanupManagedSourceWorkspace{job: j}
	return true, nil
}

// The caller has returned from the entire source stack and returned its native
// owner before retiring scratch. A canceled RPC remains charged until return;
// neither Stop nor a caller callback can refund another goroutine's work.
func (w *DueWorker) releaseCleanupManagedSource(j *cleanupLocalJob) {
	w.backend.archiveLife.mu.Lock()
	defer w.backend.archiveLife.mu.Unlock()
	if w.managedSource != nil && w.managedSource.job == j && j.owner == nil {
		w.managedSource = nil
		w.cache.managedSource = false
	}
}
