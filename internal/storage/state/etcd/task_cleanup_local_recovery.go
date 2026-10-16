package etcd

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
)

// This is the existing job goroutine, still registered among the fixed 32 jobs.
// Manifest/tar IO and actual source/target Close complete before requesting a
// charged actor continuation; no native owner is held across filesystem work.
func (b *Backend) cleanupResumeBoundArchive(j *cleanupLocalJob) error {
	bound, err := b.loadCleanupBoundArchive(j.ctx, j)
	if err != nil {
		return err
	}

	j.boundArchive = bound
	reply, err := j.worker.cleanupContinuation(j, &cleanupLocalContinuation{kind: "resume-bound"})
	j.claim = reply.claim
	if err != nil {
		return err
	}
	return b.cleanupFinalizeBoundArchive(j, bound.Slot)
}

func (b *Backend) loadCleanupBoundArchive(ctx context.Context, j *cleanupLocalJob) (_ *cleanupExecutionArchive, resultErr error) {
	if ctx == nil || j == nil || !j.boundRecovery || j.target == nil || j.state == nil || j.owner != nil || b.cleanupPhysicalProfile() != nil {
		return nil, ErrInvalidRecord
	}

	manifest, _, err := b.loadLocalRuntimeManifest(ctx, j.task)
	if err != nil {
		return nil, err
	}
	target, err := openTaskWorkspaceSyncLocalTarget(ctx, b.localCleanup.Worker, j.target.plan, j.target.root)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, target.Close()) }()
	if err = matchCleanupBoundManifest(j.state, j.target, b.archiveStore.recovery.root, target.pin, manifest); err != nil {
		return nil, err
	}

	return &cleanupExecutionArchive{StoreID: manifest.StoreID, Slot: manifest.Slot, BindingDigest: manifest.Binding.TargetDigest}, nil
}

// Matching copied metadata is a necessary attribution check, not an effect
// capability. The caller must have loaded and closed the original recovery
// source and opened the actual target using its genuine current worker.
func matchCleanupBoundManifest(state *cleanupSlotState, target *cleanupLocalTarget, root taskArchiveRecoveryRoot, current taskWorkspaceSyncLocalAttribution, manifest taskArchiveRecoveryManifest) error {
	if state == nil || state.record.Validate() != nil || state.record.Phase != "occupied" || state.record.Confirmed != "archive-bound" || state.record.Run == nil || !state.record.Run.Joined || state.record.Archive == nil || target == nil || target.plan.validate() != nil || target.plan.workspace.provider != "local" || current.Validate() != nil || manifest.validate() != nil || manifest.State != "bound" {
		return ErrInvalidRecord
	}
	r, bound := state.record, state.record.Archive
	if manifest.Seed.Task != r.Task || manifest.Seed.TaskBirth != r.TaskBirth || manifest.Seed.TaskDigest != r.TaskDigest || manifest.StoreID != bound.StoreID || manifest.Slot != bound.Slot || manifest.Root != root || manifest.Binding.TargetDigest != bound.BindingDigest || current.Root != target.root || !workspaceSyncRecoverySameTarget(manifest.Binding.Local, current) {
		return ErrIdentityMismatch
	}

	plan, err := manifest.Binding.Plan.plan()
	if err != nil || plan.workspace != target.plan.workspace || plan.digest != target.plan.digest || plan.deleteExtraneous != target.plan.deleteExtraneous || !slices.Equal(plan.exclude, target.plan.exclude) {
		return errors.Join(ErrIdentityMismatch, err)
	}
	return nil
}

// Only one new Run candidate is attempted for this completed preflight. A lost
// CAS ACK retains its original candidate and occupation; no alternate Run or
// Claim is manufactured. This is a finite native turn, with no manifest IO.
func (b *Backend) beginCleanupRecoveryRun(owner *workerDueOwner, j *cleanupLocalJob) error {
	if j == nil || j.worker == nil || j.worker.backend != b || j.worker.self != j.worker || owner == nil || j.owner != owner || !j.started || !j.boundRecovery || j.boundArchive == nil || j.runID != "" || j.claim != nil || j.pendingProgress != nil || j.state == nil || j.task != j.state.record.Task || j.state.slot == nil || j.state.record.Run == nil || !j.state.record.Run.Joined || j.state.record.Confirmed != "archive-bound" || j.state.record.Archive == nil || *j.boundArchive != *j.state.record.Archive || j.evidence == nil {
		return ErrInvalidRecord
	}

	// A copied diagnostic job cannot borrow the actor's current owner. Check
	// pointer membership before consulting Kernel/profile or preparing a write.
	b.archiveLife.mu.Lock()
	registered := false
	for _, candidate := range j.worker.cleanupJobs {
		if candidate == j {
			registered = true
			break
		}
	}
	b.archiveLife.mu.Unlock()
	if !registered || b.cleanupPhysicalProfile() != nil {
		return ErrInvalidRecord
	}

	s := j.state
	if err := b.validateCleanupState(s); err != nil {
		return err
	}
	expected := cleanupSlotRecordFromExecution(j.evidence.record)
	expected.Selection, expected.Slot = s.record.Selection, s.record.Slot
	if expected != cleanupSlotRecordFromExecution(s.record) {
		return ErrConflict
	}
	cmps, err := b.cleanupEvidenceComparisons(owner, j.evidence)
	if err != nil {
		return err
	}

	// Project this transition and the existing slot/Claim chain before its
	// dispatch. claim-state later checks the complete physical inventory too.
	if _, err = b.cleanupSlotWireInventory(owner, j.evidence, s); err != nil {
		return err
	}
	worker, err := b.localCleanup.Worker.WorkspaceSyncWorkerReference()
	if err != nil {
		return err
	}

	j.runID = uuid.NewString()
	r := s.record
	r.Run = &cleanupExecutionRun{ID: j.runID, Worker: worker}
	r.Pending = &cleanupExecutionPending{Purpose: "claim", ClaimID: uuid.NewString()}
	wire, err := encodeCleanupExecution(r)
	if err != nil {
		return err
	}
	cmps = append(cmps, s.execution.comparisons()...)
	cmps = append(cmps, s.slot.comparisons()...)
	if err = b.cleanupWrite(owner, "recovery-run", cmps, []Write{{Key: s.execution.key, Value: []byte(wire)}}); err != nil {
		return err
	}

	observed, err := b.readCleanupKnownState(owner, s)
	if err != nil {
		return err
	}
	if observed.execution.value != wire || observed.execution.mod <= s.execution.mod {
		return ErrConflict
	}
	j.state = observed
	return nil
}

// Accepted is not terminal quiescence. Wait using only the original read-only
// query, without resending a command or constructing another Prepared value.
// Any unknown/error retains the original pending reference; only a verified
// terminal receipt permits the following dual-drain preparation.
func (b *Backend) waitCleanupQuiescence(j *cleanupLocalJob, result TaskUserQuiescenceDeliveryResult) error {
	for result.Receipt == nil {
		if result.Accepted == nil {
			return ErrDeliveryUnknown
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-j.ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return j.ctx.Err()
		case <-timer.C:
		}
		var err error
		result, err = b.QueryTaskQuiescence(j.ctx, j.task, j.target.destination)
		if err != nil {
			return err
		}
	}
	return nil
}
