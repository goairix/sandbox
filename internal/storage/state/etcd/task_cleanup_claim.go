package etcd

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/goairix/sandbox/internal/runtime/launcher"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This private result is reserved for the physical projection producer. That
// producer must own an original worker boundary and all concrete old-chain
// templates; no caller boolean, DTO, or host-test constructor can authorize it.
// The one-use latch is consumed before Grant, including an unknown Grant. A
// retained run cannot use the same preflight to mint a second claim attempt.
type cleanupLocalWireInventory struct {
	origin                      *Backend
	self                        *cleanupLocalWireInventory
	task                        TaskReference
	incarnation, runID, claimID string
	executionMod, slotMod       int64
	kernel                      *launcher.KernelBoundary
	projections                 []cleanupWireProjection
	used                        atomic.Bool
}

// These are complete native-template families, not evidence that this slice
// implements their producer. The later physical integration must populate and
// validate each family before it can construct a usable inventory result.
var cleanupPhysicalWireFamilies = [...]string{
	"stage-begin", "stage-commit", "stage-resolve", "quiescence-stage",
	"task-checkpoint", "task-renew", "task-release", "close", "close-query",
	"quiescence", "quiescence-query", "dual-drain", "final-sync",
	"archive-intent", "archive-send", "archive-recovery", "archive-recycling",
	"local-progress", "local-complete", "termination", "provider", "owner-release",
}

func (b *Backend) validateCleanupLocalInventory(inventory *cleanupLocalWireInventory, state *cleanupSlotState) error {
	if inventory == nil || inventory.self != inventory || inventory.origin != b || inventory.kernel == nil || state == nil || state.record.Run == nil || state.record.Pending == nil || state.slot == nil || inventory.task != state.record.Task || inventory.incarnation != state.record.Incarnation || inventory.runID != state.record.Run.ID || inventory.claimID != state.record.Pending.ClaimID || inventory.executionMod != state.execution.mod || inventory.slotMod != state.slot.mod {
		return ErrInvalidConfiguration
	}
	worker, err := inventory.kernel.WorkspaceSyncWorkerReference()
	if err != nil || worker != state.record.Run.Worker {
		return ErrInvalidConfiguration
	}
	seen := make(map[string]bool, len(inventory.projections))
	for _, projection := range inventory.projections {
		if seen[projection.name] || validateCleanupWire(projection) != nil {
			return ErrInvalidConfiguration
		}
		seen[projection.name] = true
	}
	if len(seen) != len(cleanupPhysicalWireFamilies) {
		return ErrInvalidConfiguration
	}
	for _, name := range cleanupPhysicalWireFamilies {
		if !seen[name] {
			return ErrInvalidConfiguration
		}
	}
	return nil
}

func (b *Backend) loadCleanupClaimBundle(owner *workerDueOwner, ref TaskReference) (*taskClaimBundle, error) {
	if _, err := b.cleanupOwnerComparisons(owner); err != nil {
		return nil, err
	}
	if ref.Partition != owner.partition.Reference().Partition {
		return nil, ErrInvalidRecord
	}
	bundle, err := b.loadTaskClaimBundleForOwner(owner.call.ctx, ref, owner)
	if err != nil {
		return nil, err
	}
	if err = owner.postcheck(); err != nil {
		return nil, err
	}
	return bundle, nil
}

// This is a separate preparation turn, never hidden in the four-RPC Grant
// turn. Positive Mod pins below are derived only from these same-key strict
// native E/S/receipt observations and their original immutable birth proof.
func (b *Backend) readCleanupClaimState(owner *workerDueOwner, bundle *taskClaimBundle, prior *cleanupSlotState) (*cleanupSlotState, error) {
	if bundle == nil || prior == nil || prior.record.Task != bundle.task.Reference || prior.record.Phase != "occupied" || prior.slot == nil {
		return nil, ErrInvalidRecord
	}
	_, receiptKey, err := b.stageKeys(bundle.initial.reference(cleanupWirePaddingDigest()))
	if err != nil {
		return nil, err
	}
	points, revision, err := b.cleanupReadPoints(owner, prior.execution.key, prior.slot.key, receiptKey)
	if err != nil {
		return nil, err
	}
	state, err := b.cleanupStateFromPoints(bundle.task.Reference, points[0], points[1], revision)
	if err != nil {
		return nil, err
	}
	evidence, err := b.cleanupEvidenceFromReceipt(bundle, points[2])
	if err != nil {
		return nil, err
	}
	if state.record.Incarnation != evidence.record.Incarnation || state.record.TaskBirth != bundle.birth || state.record.Run == nil || state.record.Run.Joined || state.record.Pending == nil || state.record.Pending.Purpose != "claim" || prior.record.Run == nil || *state.record.Run != *prior.record.Run || state.execution.create != prior.execution.create || !prior.slot.matches(points[1]) {
		return nil, ErrConflict
	}
	state.claimEvidence = evidence
	return state, nil
}

func (b *Backend) claimCleanupTask(owner *workerDueOwner, jobCtx context.Context, bundle *taskClaimBundle, state *cleanupSlotState, inventory *cleanupLocalWireInventory, workerID string, ttl time.Duration) (*TaskClaim, error) {
	worker, err := b.cleanupOwnerComparisons(owner)
	if err != nil {
		return nil, err
	}
	// Refuse a missing physical profile before Grant even when metadata slot
	// attribution is valid. This is not a stub: a validated original inventory
	// continues into the exact shared claim construction/native pipeline.
	if err = b.validateCleanupLocalInventory(inventory, state); err != nil {
		return nil, err
	}
	if jobCtx == nil || jobCtx.Err() != nil || bundle == nil || state.claimEvidence == nil || state.claimEvidence.origin != b || state.claimEvidence.bundle != bundle || state.record.Phase != "occupied" || state.record.Run.Joined || state.record.Pending.Purpose != "claim" || state.record.Pending.LeaseID != 0 || bundle.task.Reference != state.record.Task || state.execution.mod <= 0 || state.slot.mod <= 0 {
		return nil, ErrInvalidRecord
	}
	if err = b.validateCleanupState(state); err != nil {
		return nil, err
	}
	b.archiveLife.mu.Lock()
	remaining := time.Until(owner.call.session.deadline)
	b.archiveLife.mu.Unlock()
	if remaining <= 4*b.requestTimeout+time.Second {
		return nil, ErrGuardExpired
	}
	if _, err = b.cleanupSlotWireInventory(owner, state.claimEvidence, state); err != nil {
		return nil, err
	}
	c, checkpoint, err := b.prepareTaskClaim(jobCtx, bundle, workerID, ttl, state.record.Pending.ClaimID)
	if err != nil {
		return nil, err
	}
	extra := append([]clientv3.Cmp(nil), worker[4:]...)
	extra = append(extra, clientv3.Compare(clientv3.ModRevision(state.execution.key), "=", state.execution.mod), clientv3.Compare(clientv3.ModRevision(state.slot.key), "=", state.slot.mod))
	cmps := append(b.baseComparisons(), c.creationMutation(checkpoint).Comparisons...)
	cmps = append(cmps, extra...)
	if err = b.checkCleanupClaimWire(c, cmps); err != nil {
		return nil, err
	}
	if !inventory.used.CompareAndSwap(false, true) {
		return nil, ErrConflict
	}
	owner.call.started = true
	return b.executeTaskClaim(owner.call.ctx, c, checkpoint, extra, owner)
}

func (b *Backend) checkCleanupClaimWire(c *TaskClaim, comparisons []clientv3.Cmp) error {
	projection := b.cleanupProjection("claim", comparisons, []*pb.RequestOp{taskProtoPut(c.claimKey, c.value, c.reference.LeaseID), taskProtoPut(c.guardKey, c.value, c.reference.LeaseID)}, taskProtoPutResponse(2))
	return validateCleanupWire(projection)
}
