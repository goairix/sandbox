package etcd

import (
	"strings"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

// cleanupColdQuotaJob contains copied attribution only. self ties every seal
// and verified result to the exact original registry object; copying this value
// or its public entry cannot create another job's completion authority.
type cleanupColdQuotaJob struct {
	self      *cleanupLocalJob
	record    TaskRecord
	birth     int64
	workspace WorkspaceIdentity
	owner     ColdSlotOwner
	quota     ColdSlotReference
	sealed    bool
	quotaOnly bool
	verified  *cleanupColdQuotaVerified
}

type cleanupColdQuotaVerified struct {
	job     *cleanupLocalJob
	entry   ColdRuntimeQuotaReturnEntry
	release cleanupExecutionRelease
}

// One serial actor owns this graph until every synchronous native call has
// returned. Cursors and permits borrow the same history/preparation. The only
// waiting room is cleanupJobs; a busy workspace never creates another owner.
type cleanupColdQuotaWorkspace struct {
	job       *cleanupLocalJob
	owner     *runtimeEnrolledPublicationOwner
	ranges    *WorkerBudgetShare
	claims    *WorkerBudgetShare
	history   *coldQuotaReturnHistory
	cursor    *coldQuotaWorkerHistory
	recovery  *coldQuotaWorkerRecovery
	recovered *coldQuotaWorkerRecoveryResult
	prepare   *coldQuotaWorkerPrepare
	prepared  *coldQuotaReturnPrepared
	journal   *coldQuotaWorkerJournalPermit
	permit    *coldQuotaWorkerCommitPermit
	postread  *coldQuotaWorkerPostread
	// P's final original CAS reauthenticates fresh birth/release after quota.
	// These temporary heavy observations remain solely in this workspace.
	birth   *cleanupBirthEvidence
	release *cleanupBirthReleaseEvidence
}

func (b *Backend) cleanupColdQuotaEnabled() bool {
	return b != nil && b.localCleanup != nil && b.localCleanup.ColdRuntimeQuotaReturn
}

func cleanupColdQuotaPhase(phase string) bool {
	return phase == "cold-origin" || strings.HasPrefix(phase, "cold-quota-")
}

// Registration is checked under the lifetime lock. The actor owns ordinary job
// fields; physical work transfers them only through the existing continuation.
func (w *DueWorker) cleanupColdQuotaRegistered(j *cleanupLocalJob) bool {
	if w == nil || w.self != w || w.backend == nil || j == nil || j.worker != w || j.partition == nil || j.partition.session != w.session || j.partition.partition != j.task.Partition {
		return false
	}
	w.backend.archiveLife.mu.Lock()
	defer w.backend.archiveLife.mu.Unlock()
	for _, original := range w.cleanupJobs {
		if original == j {
			return true
		}
	}
	return false
}

func (w *DueWorker) cleanupColdQuotaTask(j *cleanupLocalJob) (TaskRecord, int64, error) {
	if !w.cleanupColdQuotaRegistered(j) {
		return TaskRecord{}, 0, ErrInvalidRecord
	}
	if j.bundle != nil {
		if j.bundle.task.Reference != j.task || j.bundle.birth <= 0 {
			return TaskRecord{}, 0, ErrInvalidRecord
		}
		return j.bundle.task, j.bundle.birth, nil
	}
	c := &j.coldQuota
	if c.self != j || c.record.Reference != j.task || c.birth <= 0 {
		return TaskRecord{}, 0, ErrInvalidRecord
	}
	return c.record, c.birth, nil
}

// captureCleanupColdQuotaOrigin runs after actual paid publication and settled
// reservation authentication, before E/S/Run or any quota mutation. A complete
// post-work history additionally authenticates the same original Task birth.
func (w *DueWorker) captureCleanupColdQuotaOrigin(j *cleanupLocalJob, h *coldQuotaReturnHistory) error {
	task, birth, err := w.cleanupColdQuotaTask(j)
	if err != nil || h == nil || h.publication == nil || h.publication.entry == nil {
		return ErrInvalidRecord
	}
	p, owner := h.publication.entry.Record, h.reservation.Owner
	if task.CreationIntentID != p.IntentID || task.Reference.SandboxID != p.SandboxID || task.WorkspaceHash != p.WorkspaceHash || task.Runtime != p.Runtime || task.Generation != p.Generation || task.DataGateEpoch != p.DataGateEpoch || task.Snapshot != p.Snapshot || p.MountAttempt != 0 || owner.IntentID != p.IntentID || owner.SandboxID != p.SandboxID || owner.WorkspaceHash != p.WorkspaceHash || owner.Generation != p.Generation || owner.Partition != j.task.Partition || owner.RequestID != p.Attempt.RequestID {
		return ErrInvalidRecord
	}
	workspace := j.coldQuota.workspace
	if j.target != nil {
		workspace = j.target.plan.workspace
	}
	if w.backend.validateWorkspaceBinding(workspace) != nil || workspace.Hash() != task.WorkspaceHash || workspace.Partition() != j.task.Partition {
		return ErrInvalidRecord
	}
	if j.state != nil && (j.state.record.Task != j.task || j.state.record.TaskBirth != birth) {
		return ErrInvalidRecord
	}
	if h.release != nil || h.profileRelease != nil {
		binding, err := coldQuotaReturnRelease(h)
		if err != nil || binding.task != j.task || binding.birth != birth || !coldQuotaReturnHistoryOriginMatches(h) {
			return ErrInvalidRecord
		}
	}
	c := &j.coldQuota
	if c.sealed && (c.self != j || c.record != task || c.birth != birth || c.workspace != workspace || c.owner != owner || c.quota != h.reservation.Quota) {
		return ErrInvalidRecord
	}
	c.self, c.record, c.birth, c.workspace, c.owner, c.sealed = j, task, birth, workspace, owner, true
	c.quota = h.reservation.Quota
	_, _, err = w.cleanupColdQuotaOwnedBytes()
	return err
}

func (w *DueWorker) validateCleanupColdQuotaOrigin(j *cleanupLocalJob) error {
	task, birth, err := w.cleanupColdQuotaTask(j)
	if err != nil {
		return err
	}
	c := &j.coldQuota
	if c.self != j || !c.sealed || c.record != task || c.birth != birth || c.owner.IntentID != task.CreationIntentID || c.owner.WorkspaceHash != task.WorkspaceHash || c.owner.Generation != task.Generation || c.workspace.Hash() != task.WorkspaceHash || c.workspace.Partition() != task.Reference.Partition {
		return ErrInvalidRecord
	}
	if j.target != nil && j.target.plan.workspace != c.workspace {
		return ErrInvalidRecord
	}
	return nil
}

func (w *DueWorker) validateCleanupColdQuotaVerified(j *cleanupLocalJob) error {
	if err := w.validateCleanupColdQuotaOrigin(j); err != nil {
		return err
	}
	c, proof := &j.coldQuota, j.coldQuota.verified
	if proof == nil || proof.job != j || proof.entry.Task != j.task || proof.entry.TaskBirth != c.birth || proof.entry.Owner != c.owner || proof.entry.Quota != c.quota || proof.entry.Runtime != c.record.Runtime || proof.entry.CreateRevision <= 0 || !validHexDigest(proof.entry.SHA256) || proof.release.CreateRevision <= 0 || !validHexDigest(proof.release.Digest) {
		return ErrInvalidRecord
	}
	return nil
}

func (b *Backend) validateCleanupColdQuotaPending(j *cleanupLocalJob, p *WorkerPartitionClaim, birth *cleanupBirthEvidence, release *cleanupBirthReleaseEvidence) error {
	if j == nil || j.worker == nil || j.worker.backend != b || j.partition != p || j.worker.validateCleanupColdQuotaVerified(j) != nil || birth == nil || release == nil || release.proof == nil || release.proof.kv == nil {
		return ErrInvalidRecord
	}
	c := &j.coldQuota
	if birth.record != c.record || birth.birth != c.birth || release.proof.kv.CreateRevision != c.verified.release.CreateRevision || workspaceSyncIntentDigest(string(release.proof.kv.Value)) != c.verified.release.Digest {
		return ErrInvalidRecord
	}
	return nil
}

// enqueueCleanupColdQuota copies only the authenticated original Task/birth.
// Signed proof is checked now and reread in the one shared workspace later;
// neither the automatic discovery batch nor each job retains a history clone.
func (w *DueWorker) enqueueCleanupColdQuota(birth *cleanupBirthEvidence, release *cleanupBirthReleaseEvidence) (*cleanupLocalJob, error) {
	if birth == nil || release == nil || w.backend.validateCleanupBirthEvidence(birth.partition, birth) != nil || release.self != release || release.origin != w.backend || release.birth != birth || release.partition != birth.partition || matchCleanupBirthReleaseProof(birth, release.proof) != nil || birth.pending == nil || birth.execution != nil && birth.execution.Phase != "settled" {
		return nil, ErrInvalidRecord
	}
	j, err := w.enqueueCleanupLocal(w.ctx, birth.record.Reference)
	if err != nil {
		return nil, err
	}
	j.coldQuota = cleanupColdQuotaJob{self: j, record: birth.record, birth: birth.birth, quotaOnly: true}
	j.step = "cold-quota-task"
	return j, nil
}

// The actor calls this only after its actual native turn returns. finish also
// calls it after the physical tail joins, making cancellation/Close symmetric.
func (w *DueWorker) releaseCleanupColdQuota(j *cleanupLocalJob) {
	if w == nil || w.coldQuota == nil || w.coldQuota.job != j {
		return
	}
	workspace := w.coldQuota
	if workspace.owner != nil {
		workspace.owner.finish()
	}
	w.coldQuota = nil
}

func cleanupColdQuotaReleaseStamp(kv *mvccpb.KeyValue) cleanupExecutionRelease {
	return cleanupExecutionRelease{CreateRevision: kv.CreateRevision, Digest: workspaceSyncIntentDigest(string(kv.Value))}
}

// Cancellation is inherited from the original registered job, never replaced
// with a background context to keep an old quota continuation alive.
func (w *DueWorker) beginCleanupColdQuota(j *cleanupLocalJob) error {
	if w.coldQuota != nil {
		if w.coldQuota.job == j {
			return nil
		}
		return ErrConflict
	}
	if !w.cleanupColdQuotaRegistered(j) || !w.backend.cleanupColdQuotaEnabled() || j.ctx == nil {
		return ErrInvalidRecord
	}
	if err := j.ctx.Err(); err != nil {
		return err
	}
	if j.step != "cold-origin" {
		if j.started && (!j.effectsJoined || j.state == nil || j.state.record.Run == nil || !j.state.record.Run.Joined || j.state.record.Run.ID != j.runID) {
			return ErrInvalidRecord
		}
		if j.state != nil && j.state.record.Phase == "occupied" && (j.state.record.Run == nil || !j.state.record.Run.Joined) {
			return ErrInvalidRecord
		}
	}
	var ranges, claims *WorkerBudgetShare
	for _, scan := range w.scans {
		if scan.partition == j.partition {
			ranges, claims = scan.ranges, scan.claims
			break
		}
	}
	if ranges == nil || claims == nil {
		return ErrInvalidRecord
	}
	o, err := w.backend.registerEnrolledPublication(j.ctx)
	if err != nil {
		return err
	}
	w.coldQuota = &cleanupColdQuotaWorkspace{job: j, owner: o, ranges: ranges, claims: claims}
	return nil
}
